import { expect, test } from 'bun:test'
import { base64Url, buildDeviceProof, deviceAccessTokens, DeviceTokenError, fetchWithDeviceKey, requestDeviceAccessToken } from './deviceProof'

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

test('a P-256 device key signs ES256 proofs with its point as the JWK', async () => {
  const ecdsa = { name: 'ECDSA', namedCurve: 'P-256', hash: 'SHA-256' }
  const pair = await crypto.subtle.generateKey(ecdsa, false, ['sign', 'verify'])
  const raw = new Uint8Array(await crypto.subtle.exportKey('raw', pair.publicKey))
  const sign = async (input) => new Uint8Array(await crypto.subtle.sign(ecdsa, pair.privateKey, input))
  const [header, claims, signature] = (await buildDeviceProof(base64Url(raw), sign, 'GET', 'https://jaz.example/v1/sessions', 'jaz_token')).split('.')
  expect(decode(header)).toEqual({ typ: 'dpop+jwt', alg: 'ES256', jwk: { kty: 'EC', crv: 'P-256', x: base64Url(raw.subarray(1, 33)), y: base64Url(raw.subarray(33)) } })
  expect(await crypto.subtle.verify(ecdsa, pair.publicKey, Buffer.from(signature, 'base64url'), new TextEncoder().encode(header + '.' + claims))).toBe(true)
})

test('a device asks for an access token with a client assertion and a proof by its key', async () => {
  const pair = await crypto.subtle.generateKey({ name: 'Ed25519' }, false, ['sign', 'verify'])
  const publicKey = base64Url(new Uint8Array(await crypto.subtle.exportKey('raw', pair.publicKey)))
  const sign = async (input) => new Uint8Array(await crypto.subtle.sign('Ed25519', pair.privateKey, input))
  const original = globalThis.fetch
  let sent
  globalThis.fetch = async (url, init) => {
    sent = { url, init }
    return Response.json({ access_token: 'jaz_access', token_type: 'DPoP', expires_in: 600 })
  }
  try {
    const token = await requestDeviceAccessToken('https://Jaz.Example', { device_id: 'device', public_key: publicKey }, sign)
    expect(token.token).toBe('jaz_access')
    expect(token.expiresAt).toBeGreaterThan(Date.now() + 590_000)
  } finally {
    globalThis.fetch = original
  }
  expect(sent.url).toBe('https://Jaz.Example/v1/devices/token')
  const form = new URLSearchParams(sent.init.body)
  expect(form.get('grant_type')).toBe('client_credentials')
  expect(form.get('client_assertion_type')).toBe('urn:ietf:params:oauth:client-assertion-type:jwt-bearer')
  const [header, claims] = form.get('client_assertion').split('.')
  expect(decode(header)).toEqual({ typ: 'client-authentication+jwt', alg: 'EdDSA' })
  const assertion = decode(claims)
  expect(assertion).toMatchObject({ iss: 'device', sub: 'device', aud: 'https://jaz.example' })
  expect([Math.abs(assertion.iat - Date.now() / 1000) < 5, assertion.exp - assertion.iat]).toEqual([true, 60])
  expect(decode(sent.init.headers.DPoP.split('.')[1])).toMatchObject({ htm: 'POST', htu: 'https://jaz.example/v1/devices/token' })
  globalThis.fetch = async () => Response.json({ error: 'invalid_client' }, { status: 401 })
  try {
    const failure = await requestDeviceAccessToken('https://jaz.example', { device_id: 'device', public_key: publicKey }, sign).catch((error) => error)
    expect(failure).toBeInstanceOf(DeviceTokenError)
    expect(failure.code).toBe('invalid_client')
    globalThis.fetch = async () => Response.json({ error: 'auth proxy JWKS: connection refused' }, { status: 500 })
    const outage = await requestDeviceAccessToken('https://jaz.example', { device_id: 'device', public_key: publicKey }, sign).catch((error) => error)
    expect(outage.code).toBe('token_request_failed')
  } finally {
    globalThis.fetch = original
  }
})

test('access tokens are shared per backend and renewed when rejected, expiring or failed', async () => {
  let issued = 0
  let lifetime = 600_000
  let fail = true
  const tokens = deviceAccessTokens(async () => {
    if (fail) {
      fail = false
      throw new DeviceTokenError('token_request_failed')
    }
    issued += 1
    return { token: `t${issued}`, expiresAt: Date.now() + lifetime }
  })
  expect(await tokens('https://a').catch((error) => error.code)).toBe('token_request_failed')
  expect(await Promise.all([tokens('https://a'), tokens('https://a')])).toEqual(['t1', 't1'])
  expect(await tokens('https://a', 'old')).toBe('t1')
  expect(await Promise.all([tokens('https://a', 't1'), tokens('https://a', 't1')])).toEqual(['t2', 't2'])
  expect(await tokens('https://b')).toBe('t3')
  lifetime = 10_000
  expect(await tokens('https://a', 't2')).toBe('t4')
  lifetime = 600_000
  expect(await tokens('https://a')).toBe('t5')
})

test('a request with a device key presents a bound token and replaces one the backend forgot, once', async () => {
  const issued = []
  const tokens = async (baseUrl, rejected) => {
    issued.push([baseUrl, rejected])
    return `t${issued.length}`
  }
  const prove = async (method, url, token) => `${method} ${url} ${token}`
  const sent = []
  const respond = (...responses) => async (url, init) => {
    sent.push([init.headers.get('Authorization'), init.headers.get('DPoP'), init.headers.get('X-Kept')])
    return responses.shift()
  }
  const forgotten = () => Response.json({ code: 'invalid_token' }, { status: 401 })
  const response = await fetchWithDeviceKey(tokens, prove, 'https://jaz.example/v1/feed', { headers: { 'X-Kept': 'yes' } }, respond(forgotten(), forgotten()))
  expect(response.status).toBe(401)
  expect(issued).toEqual([['https://jaz.example', undefined], ['https://jaz.example', 't1']])
  expect(sent).toEqual([
    ['DPoP t1', 'GET https://jaz.example/v1/feed t1', 'yes'],
    ['DPoP t2', 'GET https://jaz.example/v1/feed t2', 'yes'],
  ])
  sent.length = 0
  await fetchWithDeviceKey(tokens, prove, 'https://jaz.example/v1/feed', { method: 'POST' }, respond(Response.json({ code: 'unauthorized' }, { status: 401 })))
  expect(sent).toHaveLength(1)
})
