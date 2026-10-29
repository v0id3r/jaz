import { base64Url, buildDeviceProof } from '@shared/deviceProof'
import { clientRuntime } from './clientRuntime'

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

export type DeviceProfile = DeviceIdentity & DeviceMetadata

const DEVICE_IDENTITY_KEY = 'jaz.deviceIdentity'
const DEVICE_KEY_DB = 'jaz-device-key'

export async function getDeviceIdentity(): Promise<DeviceIdentity> {
  if (clientRuntime.getDeviceIdentity) return clientRuntime.getDeviceIdentity()
  return getBrowserDeviceIdentity()
}

export async function getDeviceProfile(): Promise<DeviceProfile> {
  const [identity, metadata] = await Promise.all([getDeviceIdentity(), getDeviceMetadata()])
  return { ...identity, ...metadata }
}

async function getDeviceMetadata(): Promise<DeviceMetadata> {
  if (clientRuntime.getDeviceMetadata) return clientRuntime.getDeviceMetadata()
  const platform = navigator.platform?.trim() || 'Browser'
  return {
    name: defaultBrowserDeviceName(platform),
    platform,
    device_family: 'Browser',
    model_identifier: '',
    app_version: '',
  }
}

// The device proof for one request, or '' where this runtime holds no device key.
export async function deviceProof(method: string, url: string, token?: string): Promise<string> {
  if (clientRuntime.deviceProof) return clientRuntime.deviceProof(method, url, token)
  const key = await browserKey()
  const algorithm = key?.privateKey.algorithm.name === 'ECDSA' ? P256 : 'Ed25519'
  return key ? buildDeviceProof(key.identity.public_key, async (input) => new Uint8Array(await crypto.subtle.sign(algorithm, key.privateKey, input as BufferSource)), method, url, token) : ''
}

type BrowserKey = { identity: DeviceIdentity; privateKey: CryptoKey }

let browserKeyPromise: Promise<BrowserKey | null> | undefined

const P256 = { name: 'ECDSA', namedCurve: 'P-256', hash: 'SHA-256' }

// A non-extractable Ed25519 key kept in IndexedDB, or P-256 where the browser
// lacks Ed25519. Only browsers without IndexedDB or either algorithm fall back
// to the keyless identity and send no proofs; any other failure surfaces and
// is retried on the next call.
function browserKey(): Promise<BrowserKey | null> {
  browserKeyPromise ??= loadBrowserKey().catch((error) => {
    browserKeyPromise = undefined
    throw error
  })
  return browserKeyPromise
}

async function loadBrowserKey(): Promise<BrowserKey | null> {
  if (typeof indexedDB === 'undefined' || !crypto.subtle) return null
  const db = await new Promise<IDBDatabase>((resolve, reject) => {
    const open = indexedDB.open(DEVICE_KEY_DB, 1)
    open.onupgradeneeded = () => open.result.createObjectStore('keys')
    open.onsuccess = () => resolve(open.result)
    open.onerror = () => reject(open.error)
  })
  const keys = (mode: IDBTransactionMode) => db.transaction('keys', mode).objectStore('keys')
  const read = () => idbRequest<CryptoKeyPair | undefined>(keys('readonly').get('device'))
  let pair = await read()
  if (!pair) {
    const created = await generateDeviceKey({ name: 'Ed25519' }) ?? await generateDeviceKey(P256)
    if (!created) return null
    // add() fails when another tab stored its key first; every tab then uses that one.
    await idbRequest(keys('readwrite').add(created, 'device')).catch(() => {})
    pair = await read()
  }
  if (!pair) throw new Error('Could not store the device key')
  const raw = new Uint8Array(await crypto.subtle.exportKey('raw', pair.publicKey))
  return { identity: { device_id: await sha256Hex(raw), public_key: base64Url(raw) }, privateKey: pair.privateKey }
}

function generateDeviceKey(algorithm: AlgorithmIdentifier | EcKeyGenParams): Promise<CryptoKeyPair | null> {
  return (crypto.subtle.generateKey(algorithm, false, ['sign', 'verify']) as Promise<CryptoKeyPair>)
    .catch((error: unknown) => (error as DOMException)?.name === 'NotSupportedError' ? null : Promise.reject(error))
}

function idbRequest<T>(request: IDBRequest): Promise<T> {
  return new Promise((resolve, reject) => {
    request.onsuccess = () => resolve(request.result as T)
    request.onerror = () => reject(request.error)
  })
}

async function getBrowserDeviceIdentity(): Promise<DeviceIdentity> {
  const key = await browserKey()
  if (key) return key.identity
  const stored = readStoredBrowserIdentity()
  if (stored) return stored
  const bytes = new Uint8Array(32)
  crypto.getRandomValues(bytes)
  const identity = {
    device_id: await sha256Hex(bytes),
    public_key: base64Url(bytes),
  }
  localStorage.setItem(DEVICE_IDENTITY_KEY, JSON.stringify(identity))
  return identity
}

function readStoredBrowserIdentity(): DeviceIdentity | null {
  try {
    const parsed = JSON.parse(localStorage.getItem(DEVICE_IDENTITY_KEY) || '') as Partial<DeviceIdentity>
    if (typeof parsed.device_id === 'string' && typeof parsed.public_key === 'string') {
      return { device_id: parsed.device_id, public_key: parsed.public_key }
    }
  } catch {
    return null
  }
  return null
}

async function sha256Hex(bytes: Uint8Array): Promise<string> {
  const input = bytes.buffer.slice(bytes.byteOffset, bytes.byteOffset + bytes.byteLength) as ArrayBuffer
  const hash = new Uint8Array(await crypto.subtle.digest('SHA-256', input))
  return Array.from(hash, (byte) => byte.toString(16).padStart(2, '0')).join('')
}

function defaultBrowserDeviceName(platform: string): string {
  return platform ? `Jaz browser on ${platform}` : 'Jaz browser'
}
