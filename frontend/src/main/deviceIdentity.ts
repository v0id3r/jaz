import { createHash, createPrivateKey, createPublicKey, generateKeyPairSync, sign, verify, type KeyObject } from 'node:crypto'
import { execFileSync } from 'node:child_process'
import { mkdirSync, readFileSync, renameSync, writeFileSync } from 'node:fs'
import { arch, hostname, platform } from 'node:os'
import { dirname, join } from 'node:path'
import { app, safeStorage } from 'electron'
import { buildDeviceProof, deviceAccessTokens, fetchWithDeviceKey, requestDeviceAccessToken } from '@shared/deviceProof'
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
// and is sealed on first read wherever a keychain is available.
type StoredIdentity = DeviceIdentity & { public_key_pem: string; created_at_ms: number } &
  ({ version: 1; private_key_pem: string } | { version: 2; private_key_sealed: string })

const SPKI_PREFIX = Buffer.from('302a300506032b6570032100', 'hex')

export function getDeviceIdentity(): DeviceIdentity {
  const { device_id, public_key } = storedIdentity()
  return { device_id, public_key }
}

function storedIdentity(): StoredIdentity {
  const existing = readStoredIdentity()
  if (existing?.version === 2 || (existing && !keychainAvailable())) return existing
  const plain = existing ?? createIdentity()
  const stored = keychainAvailable() ? sealIdentity(plain) : plain
  writeStoredIdentity(stored)
  return stored
}

function sealIdentity({ private_key_pem, ...identity }: Extract<StoredIdentity, { version: 1 }>): StoredIdentity {
  return { ...identity, version: 2, private_key_sealed: safeStorage.encryptString(private_key_pem).toString('base64') }
}

function privateKeyPem(identity: StoredIdentity): string {
  if (identity.version === 1) return identity.private_key_pem
  try {
    return safeStorage.decryptString(Buffer.from(identity.private_key_sealed, 'base64'))
  } catch (error) {
    throw new Error('Could not unlock the device key. Allow Jaz access to your system keychain, then restart Jaz.', { cause: error })
  }
}

let signingKey: { identity: DeviceIdentity; key: KeyObject } | undefined

function deviceSigner() {
  if (!signingKey) {
    const identity = storedIdentity()
    const pem = privateKeyPem(identity)
    if (!keyPairMatches(identity.public_key_pem, pem)) {
      throw new Error('Device identity keys do not match')
    }
    signingKey = { identity: { device_id: identity.device_id, public_key: identity.public_key }, key: createPrivateKey(pem) }
  }
  const { identity, key } = signingKey
  return { identity, sign: async (input: Uint8Array) => new Uint8Array(sign(null, input, key)) }
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
      typeof identity.public_key_pem !== 'string' ||
      !(identity.version === 1 && typeof identity.private_key_pem === 'string') &&
      !(identity.version === 2 && typeof identity.private_key_sealed === 'string')
    ) {
      throw new Error('Invalid device identity format')
    }
    const raw = rawPublicKey(identity.public_key_pem)
    // A sealed private key is checked against the public one when it is unsealed.
    if (identity.device_id !== deviceIDForRawKey(raw) || identity.public_key !== base64Url(raw) ||
      (identity.version === 1 && !keyPairMatches(identity.public_key_pem, identity.private_key_pem))) {
      throw new Error('Device identity keys do not match')
    }
    return identity
  } catch (error) {
    throw new Error('The existing device identity is invalid; it has been preserved', { cause: error })
  }
}

function createIdentity(): Extract<StoredIdentity, { version: 1 }> {
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
