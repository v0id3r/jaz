import { expect, test } from 'bun:test'
import { build } from 'bun'
import { mkdtempSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { createRequire } from 'node:module'
import { resolve, join } from 'node:path'

const bundle = await build({ entrypoints: [new URL('./deviceIdentity.ts', import.meta.url).pathname], target: 'node', format: 'cjs', external: ['electron'] })
if (!bundle.success) {
  throw new AggregateError(bundle.logs)
}
const evaluate = new Function('require', 'module', 'exports', await bundle.outputs[0].text())
const require = createRequire(import.meta.url)
mkdirSync(resolve('../runs/identity-regressions'), { recursive: true })
function keychain() {
  const state = {
    available: true,
    refuse: false,
    isEncryptionAvailable: () => state.available,
    getSelectedStorageBackend: () => 'keychain',
    encryptString: (value) => Buffer.from('sealed:' + Buffer.from(value).toString('base64')),
    decryptString: (sealed) => {
      if (state.refuse) throw new Error('denied')
      return Buffer.from(sealed.toString().slice(7), 'base64').toString()
    },
  }
  return state
}

function fixture(root = mkdtempSync(resolve('../runs/identity-regressions/case-')), safeStorage = keychain()) {
  const module = { exports: {} }
  evaluate((name) => name === 'electron' ? { app: { getPath: () => root }, safeStorage } : require(name), module, module.exports)
  return { root, safeStorage, get: module.exports.getDeviceIdentity, proof: module.exports.deviceProof, file: join(root, 'device-identity.json') }
}

async function proofVerifies(f) {
  const { public_key } = f.get()
  const [header, claims, signature] = (await f.proof('GET', 'https://jaz.example/v1/sessions', 'jaz_token')).split('.')
  const key = await crypto.subtle.importKey('raw', Buffer.from(public_key, 'base64url'), { name: 'Ed25519' }, false, ['verify'])
  return JSON.parse(Buffer.from(header, 'base64url').toString()).jwk.x === public_key &&
    await crypto.subtle.verify('Ed25519', key, Buffer.from(signature, 'base64url'), new TextEncoder().encode(header + '.' + claims))
}

test('the private key is stored only sealed by the keychain and still signs proofs', async () => {
  const f = fixture()
  f.get()
  const stored = JSON.parse(readFileSync(f.file, 'utf8'))
  expect(stored.version).toBe(2)
  expect(JSON.stringify(stored)).not.toContain('PRIVATE KEY')
  expect(await proofVerifies(f)).toBeTrue()
})

test('a plain-text identity is sealed once a keychain is available and keeps its device id', async () => {
  const safeStorage = keychain()
  safeStorage.available = false
  const before = fixture(undefined, safeStorage)
  const identity = before.get()
  expect(JSON.parse(readFileSync(before.file, 'utf8')).version).toBe(1)
  expect(await proofVerifies(before)).toBeTrue()
  safeStorage.available = true
  const after = fixture(before.root, safeStorage)
  expect(after.get()).toEqual(identity)
  expect(JSON.parse(readFileSync(after.file, 'utf8')).version).toBe(2)
  expect(await proofVerifies(after)).toBeTrue()
})

test('a refused keychain keeps the identity but cannot sign', () => {
  const f = fixture()
  const identity = f.get()
  f.safeStorage.refuse = true
  const locked = fixture(f.root, f.safeStorage)
  expect(locked.get()).toEqual(identity)
  expect(() => locked.proof('GET', 'https://jaz.example/v1/sessions', 'jaz_token')).toThrow('Allow Jaz access')
})

test('first install creates an identity which subsequent loads preserve exactly', () => {
  const f = fixture()
  const identity = f.get()
  const bytes = readFileSync(f.file)
  expect(f.get()).toEqual(identity)
  expect(readFileSync(f.file)).toEqual(bytes)
  expect(identity.device_id).toHaveLength(64)
})

test.each(['malformed', 'mismatched'])('existing %s identity is preserved and fails closed', (kind) => {
  const f = fixture()
  f.get()
  const identity = JSON.parse(readFileSync(f.file, 'utf8'))
  identity.device_id = 'wrong'
  writeFileSync(f.file, kind === 'malformed' ? '{broken' : JSON.stringify(identity))
  const bytes = readFileSync(f.file)
  expect(f.get).toThrow('preserved')
  expect(readFileSync(f.file)).toEqual(bytes)
})

test('existing unreadable identity path is not treated as first install', () => {
  const f = fixture()
  mkdirSync(f.file)
  expect(f.get).toThrow('Could not read the existing device identity')
})
