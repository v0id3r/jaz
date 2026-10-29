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
function fixture() {
  const root = mkdtempSync(resolve('../runs/identity-regressions/case-'))
  const module = { exports: {} }
  evaluate((name) => name === 'electron' ? { app: { getPath: () => root } } : require(name), module, module.exports)
  return { get: module.exports.getDeviceIdentity, proof: module.exports.deviceProof, file: join(root, 'device-identity.json') }
}

test('first install creates an identity which subsequent loads preserve exactly', () => {
  const f = fixture()
  const identity = f.get()
  const bytes = readFileSync(f.file)
  expect(f.get()).toEqual(identity)
  expect(readFileSync(f.file)).toEqual(bytes)
  expect(identity.device_id).toHaveLength(64)
})

test('device proofs are signed by the stored identity key', async () => {
  const f = fixture()
  const { public_key } = f.get()
  const [header, claims, signature] = (await f.proof('GET', 'https://jaz.example/v1/sessions', 'jaz_token')).split('.')
  const key = await crypto.subtle.importKey('raw', Buffer.from(public_key, 'base64url'), { name: 'Ed25519' }, false, ['verify'])
  expect(JSON.parse(Buffer.from(header, 'base64url').toString()).jwk.x).toBe(public_key)
  expect(await crypto.subtle.verify('Ed25519', key, Buffer.from(signature, 'base64url'), new TextEncoder().encode(header + '.' + claims))).toBe(true)
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
