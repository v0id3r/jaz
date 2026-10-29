import { expect, test } from 'bun:test'
import { base64Url, buildDeviceProof } from './deviceProof'

const decode = (segment) => JSON.parse(Buffer.from(segment, 'base64url').toString())

test('a device proof is a JWS of the request signed by the device key and bound to the token', async () => {
  const pair = await crypto.subtle.generateKey({ name: 'Ed25519' }, false, ['sign', 'verify'])
  const publicKey = base64Url(new Uint8Array(await crypto.subtle.exportKey('raw', pair.publicKey)))
  const sign = async (input) => new Uint8Array(await crypto.subtle.sign('Ed25519', pair.privateKey, input))
  const proof = await buildDeviceProof(publicKey, sign, 'post', 'https://jaz.example/v1/sessions?x=1#y', 'jaz_token')
  const [header, claims, signature] = proof.split('.')
  expect(decode(header)).toEqual({ typ: 'dpop+jwt', alg: 'EdDSA', jwk: { kty: 'OKP', crv: 'Ed25519', x: publicKey } })
  const tokenHash = Buffer.from(await crypto.subtle.digest('SHA-256', new TextEncoder().encode('jaz_token'))).toString('base64url')
  expect(decode(claims)).toMatchObject({ htm: 'POST', htu: 'https://jaz.example/v1/sessions', ath: tokenHash })
  expect(await crypto.subtle.verify('Ed25519', pair.publicKey, Buffer.from(signature, 'base64url'), new TextEncoder().encode(header + '.' + claims))).toBe(true)
  const registration = decode((await buildDeviceProof(publicKey, sign, 'POST', 'https://jaz.example/v1/devices/register')).split('.')[1])
  expect(registration.ath).toBeUndefined()
  expect(registration.jti).not.toBe(decode(claims).jti)
})
