import { createHash, createPrivateKey, createPublicKey, generateKeyPairSync, sign, verify } from 'node:crypto'
import { execFileSync } from 'node:child_process'
import { existsSync, mkdirSync, readFileSync, renameSync, writeFileSync } from 'node:fs'
import { arch, hostname, platform } from 'node:os'
import { dirname, join } from 'node:path'
import { app, safeStorage } from 'electron'
import { buildDeviceProof, deviceAccessTokens, fetchWithDeviceKey, requestDeviceAccessToken } from '@shared/deviceProof'
import { createEnclaveKey, enclaveSigner } from '@main/enclaveKey'
import { keychainAvailable } from '@main/keychain'

export type DeviceIdentity = {
  device_id: string
  public_key: string
}

export type DeviceMetadata = {
  name: string
  platform: string
  device_family: string
  model_identifier: string
  app_version: string
}

// Version 2 keeps the private key sealed by the system keychain, so a copy of
// userData alone cannot sign device proofs. Version 1 holds it in plain text
// and is sealed on first read wherever a keychain is available. Version 3 is a
// P-256 key in the Secure Enclave: the private key never leaves this Mac, and
// only its handle is stored, sealed by the keychain.
type StoredIdentity = DeviceIdentity & { created_at_ms: number } & (
  | { version: 1; public_key_pem: string; private_key_pem: string }
  | { version: 2; public_key_pem: string; private_key_sealed: string }
  | { version: 3; key_handle_sealed: string })
type Ed25519Identity = Exclude<StoredIdentity, { version: 3 }>
type Sign = (input: Uint8Array) => Promise<Uint8Array>

const SPKI_PREFIX = Buffer.from('302a300506032b6570032100', 'hex')

export function getDeviceIdentity(): DeviceIdentity {
  const { device_id, public_key } = storedIdentity()
  return { device_id, public_key }
}

function storedIdentity(): StoredIdentity {
  const existing = readStoredIdentity()
  if (existing && (existing.version !== 1 || !keychainAvailable())) return existing
  const stored = existing ? sealIdentity(existing) : createIdentity()
  writeStoredIdentity(stored)
  return stored
}

function sealIdentity({ private_key_pem, ...identity }: Extract<StoredIdentity, { version: 1 }>): StoredIdentity {
  return { ...identity, version: 2, private_key_sealed: seal(private_key_pem) }
}

function seal(secret: string): string {
  return safeStorage.encryptString(secret).toString('base64')
}

function unseal(sealed: string): string {
  try {
    return safeStorage.decryptString(Buffer.from(sealed, 'base64'))
  } catch (error) {
    throw new Error('Could not unlock the device key. Allow Jaz access to your system keychain, then restart Jaz.', { cause: error })
  }
}

let signingKey: { identity: DeviceIdentity; sign: Sign } | undefined

function deviceSigner() {
  if (!signingKey) {
    const identity = storedIdentity()
    const sign = identity.version === 3 ? enclaveSigner(deviceKeyHelper(), unseal(identity.key_handle_sealed)) : ed25519Signer(identity)
    signingKey = { identity: { device_id: identity.device_id, public_key: identity.public_key }, sign }
  }
  return signingKey
}

function ed25519Signer(identity: Ed25519Identity): Sign {
  const pem = identity.version === 1 ? identity.private_key_pem : unseal(identity.private_key_sealed)
  if (!keyPairMatches(identity.public_key_pem, pem)) {
    throw new Error('Device identity keys do not match')
  }
  const key = createPrivateKey(pem)
  return async (input) => new Uint8Array(sign(null, input, key))
}

// Proofs and assertions are built here so the renderer never gets a general
// signing oracle.
export function deviceProof(method: string, url: string, token?: string): Promise<string> {
  const { identity, sign } = deviceSigner()
  return buildDeviceProof(identity.public_key, sign, method, url, token)
}

export const deviceAccessToken = deviceAccessTokens((baseUrl) => {
  const { identity, sign } = deviceSigner()
  return requestDeviceAccessToken(baseUrl, identity, sign)
})

// Fetches as this device on a backend in proof mode.
export function deviceKeyFetch(url: string, init?: RequestInit): Promise<Response> {
  return fetchWithDeviceKey(deviceAccessToken, deviceProof, url, init)
}

export function getDeviceMetadata(): DeviceMetadata {
  const devicePlatform = platformLabel()
  const family = deviceFamily()
  return {
    name: defaultDeviceName(family),
    platform: devicePlatform,
    device_family: family,
    model_identifier: modelIdentifier(),
    app_version: app.getVersion(),
  }
}

function readStoredIdentity(): StoredIdentity | null {
  let contents: string
  try {
    contents = readFileSync(identityPath(), 'utf8')
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ENOENT') {
      return null
    }
    throw new Error('Could not read the existing device identity', { cause: error })
  }
  try {
    const identity = JSON.parse(contents) as StoredIdentity
    if (
      !(identity.version === 1 && typeof identity.public_key_pem === 'string' && typeof identity.private_key_pem === 'string') &&
      !(identity.version === 2 && typeof identity.public_key_pem === 'string' && typeof identity.private_key_sealed === 'string') &&
      !(identity.version === 3 && typeof identity.public_key === 'string' && typeof identity.key_handle_sealed === 'string')
    ) {
      throw new Error('Invalid device identity format')
    }
    const raw = identity.version === 3 ? Buffer.from(identity.public_key, 'base64url') : rawPublicKey(identity.public_key_pem)
    // A sealed private key is checked against the public one when it is unsealed.
    if (identity.device_id !== deviceIDForRawKey(raw) || identity.public_key !== base64Url(raw) ||
      (identity.version === 3 && raw.length !== 65) ||
      (identity.version === 1 && !keyPairMatches(identity.public_key_pem, identity.private_key_pem))) {
      throw new Error('Device identity keys do not match')
    }
    return identity
  } catch (error) {
    throw new Error('The existing device identity is invalid; it has been preserved', { cause: error })
  }
}

// A Mac with the device-key helper keeps a new key in the Secure Enclave;
// elsewhere, or if the enclave refuses, the key is Ed25519 sealed by the keychain.
function createIdentity(): StoredIdentity {
  const enclave = keychainAvailable() && existsSync(deviceKeyHelper()) ? createEnclaveIdentity() : null
  if (enclave) return enclave
  const plain = createEd25519Identity()
  return keychainAvailable() ? sealIdentity(plain) : plain
}

function createEnclaveIdentity(): StoredIdentity | null {
  try {
    const { handle, publicKey } = createEnclaveKey(deviceKeyHelper())
    return {
      version: 3,
      device_id: deviceIDForRawKey(publicKey),
      public_key: base64Url(publicKey),
      key_handle_sealed: seal(handle),
      created_at_ms: Date.now(),
    }
  } catch (error) {
    console.warn('Secure Enclave device key unavailable, using a keychain-sealed key', error)
    return null
  }
}

function deviceKeyHelper(): string {
  return app.isPackaged
    ? join(process.resourcesPath, 'bin/jaz-device-key')
    : join(app.getAppPath(), 'resources/bin/jaz-device-key')
}

function createEd25519Identity(): Extract<StoredIdentity, { version: 1 }> {
  const { publicKey, privateKey } = generateKeyPairSync('ed25519')
  const publicKeyPem = publicKey.export({ type: 'spki', format: 'pem' })
  const privateKeyPem = privateKey.export({ type: 'pkcs8', format: 'pem' })
  const raw = rawPublicKey(publicKeyPem)
  return {
    version: 1,
    device_id: deviceIDForRawKey(raw),
    public_key: base64Url(raw),
    public_key_pem: publicKeyPem,
    private_key_pem: privateKeyPem,
    created_at_ms: Date.now(),
  }
}

// Written beside the target and renamed over it, so sealing an existing
// identity can never leave it half-written.
function writeStoredIdentity(identity: StoredIdentity): void {
  const file = identityPath()
  mkdirSync(dirname(file), { recursive: true, mode: 0o700 })
  writeFileSync(file + '.tmp', `${JSON.stringify(identity, null, 2)}\n`, { mode: 0o600 })
  renameSync(file + '.tmp', file)
}

function identityPath(): string {
  return join(app.getPath('userData'), 'device-identity.json')
}

function rawPublicKey(publicKeyPem: string): Buffer {
  const key = createPublicKey(publicKeyPem)
  const spki = key.export({ type: 'spki', format: 'der' }) as Buffer
  if (spki.length === SPKI_PREFIX.length + 32 && spki.subarray(0, SPKI_PREFIX.length).equals(SPKI_PREFIX)) {
    return spki.subarray(SPKI_PREFIX.length)
  }
  return spki
}

function keyPairMatches(publicKeyPem: string, privateKeyPem: string): boolean {
  try {
    const payload = Buffer.from('jaz-device-identity-self-check')
    const signature = sign(null, payload, createPrivateKey(privateKeyPem))
    return verify(null, payload, createPublicKey(publicKeyPem), signature)
  } catch {
    return false
  }
}

function deviceIDForRawKey(raw: Buffer): string {
  return createHash('sha256').update(raw).digest('hex')
}

function base64Url(raw: Buffer): string {
  return raw.toString('base64').replaceAll('+', '-').replaceAll('/', '_').replace(/=+$/g, '')
}

function defaultDeviceName(family: string): string {
  const host = hostname().trim().replace(/\.local$/i, '').replaceAll('-', ' ')
  return host || `Jaz ${family}`
}

function platformLabel(): string {
  switch (platform()) {
    case 'darwin':
      return 'macOS'
    case 'win32':
      return 'Windows'
    case 'linux':
      return 'Linux'
    default:
      return platform()
  }
}

function deviceFamily(): string {
  switch (platform()) {
    case 'darwin':
      return 'Mac'
    case 'win32':
    case 'linux':
      return 'Desktop'
    default:
      return 'Desktop'
  }
}

function modelIdentifier(): string {
  if (platform() === 'darwin') {
    try {
      return execFileSync('/usr/sbin/sysctl', ['-n', 'hw.model'], {
        encoding: 'utf8',
        timeout: 500,
      }).trim()
    } catch {
      return arch()
    }
  }
  return arch()
}
