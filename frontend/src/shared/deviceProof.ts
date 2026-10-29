type Sign = (input: Uint8Array) => Promise<Uint8Array>

// A DPoP proof (RFC 9449) that the device key signed this one request.
// publicKey is the raw key: 32 bytes of Ed25519 or a 65-byte P-256 point.
export async function buildDeviceProof(publicKey: string, sign: Sign, method: string, url: string, token?: string): Promise<string> {
  const target = new URL(url)
  const { alg, jwk } = keyHeader(publicKey)
  return signJWS({ typ: 'dpop+jwt', alg, jwk }, {
    jti: crypto.randomUUID(),
    htm: method.toUpperCase(),
    htu: target.origin + target.pathname,
    iat: Math.floor(Date.now() / 1000),
    ...(token ? { ath: base64Url(new Uint8Array(await crypto.subtle.digest('SHA-256', new TextEncoder().encode(token)))) } : {}),
  }, sign)
}

export class DeviceTokenError extends Error {
  constructor(readonly code: string) {
    super(code)
  }
}

export type DeviceAccessToken = { token: string; expiresAt: number }
export type DeviceKey = { device_id: string; public_key: string }
// The access token of a proof-mode backend at baseUrl; rejected names one the
// backend refused, so a new one replaces it.
export type DeviceAccessTokens = (baseUrl: string, rejected?: string) => Promise<string>
type Prove = (method: string, url: string, token: string) => Promise<string>

// Gets an access token from a proof-mode backend: the client_credentials grant
// with a private_key_jwt assertion (RFC 7523) and a DPoP proof, both signed by
// the device key. Fails with the OAuth error code, e.g. invalid_client for a
// device the backend has not approved, or token_request_failed, worth a retry,
// when the backend could not answer.
export async function requestDeviceAccessToken(baseUrl: string, key: DeviceKey, sign: Sign): Promise<DeviceAccessToken> {
  const url = baseUrl + '/v1/devices/token'
  const now = Math.floor(Date.now() / 1000)
  const assertion = await signJWS({ typ: 'client-authentication+jwt', alg: keyHeader(key.public_key).alg }, {
    iss: key.device_id, sub: key.device_id, aud: new URL(baseUrl).origin, jti: crypto.randomUUID(), iat: now, exp: now + 60,
  }, sign)
  const response = await fetch(url, {
    method: 'POST', redirect: 'error', signal: AbortSignal.timeout(5000),
    headers: { 'Content-Type': 'application/x-www-form-urlencoded', DPoP: await buildDeviceProof(key.public_key, sign, 'POST', url) },
    body: new URLSearchParams({ grant_type: 'client_credentials', client_assertion_type: 'urn:ietf:params:oauth:client-assertion-type:jwt-bearer', client_assertion: assertion }),
  })
  const body = await response.json().catch(() => null) as { access_token?: unknown; token_type?: unknown; expires_in?: unknown; error?: unknown } | null
  if (!response.ok || body?.token_type !== 'DPoP' || typeof body.access_token !== 'string' || typeof body.expires_in !== 'number') {
    throw new DeviceTokenError(response.status < 500 && typeof body?.error === 'string' ? body.error : 'token_request_failed')
  }
  return { token: body.access_token, expiresAt: Date.now() + body.expires_in * 1000 }
}

// One access token per backend, renewed shortly before it expires or once the
// backend rejects it.
export function deviceAccessTokens(request: (baseUrl: string) => Promise<DeviceAccessToken>): DeviceAccessTokens {
  const cache = new Map<string, Promise<DeviceAccessToken>>()
  return async (baseUrl, rejected) => {
    let pending = cache.get(baseUrl)
    const current = await pending?.catch(() => undefined)
    if (pending && current && current.token !== rejected && current.expiresAt - 30_000 > Date.now()) return current.token
    // A concurrent caller may already have replaced the token this one refused.
    if (!cache.has(baseUrl) || cache.get(baseUrl) === pending) {
      pending = request(baseUrl)
      cache.set(baseUrl, pending)
      pending.catch(() => cache.get(baseUrl) === pending && cache.delete(baseUrl))
    }
    return (await cache.get(baseUrl)!).token
  }
}

// Sends a request with a DPoP-bound access token of the device key and gets a
// new token once if the backend has forgotten it, as after a restart.
export async function fetchWithDeviceKey(tokens: DeviceAccessTokens, prove: Prove, url: string, init: RequestInit = {}, fetcher: typeof fetch = fetch): Promise<Response> {
  const baseUrl = new URL(url).origin
  const send = async (token: string) => {
    const headers = new Headers(init.headers)
    headers.set('Authorization', `DPoP ${token}`)
    headers.set('DPoP', await prove(init.method ?? 'GET', url, token))
    return fetcher(url, { ...init, headers })
  }
  const token = await tokens(baseUrl)
  const response = await send(token)
  const body = response.status === 401 ? await response.clone().json().catch(() => null) as { code?: unknown } | null : null
  return body?.code === 'invalid_token' ? send(await tokens(baseUrl, token)) : response
}

function keyHeader(publicKey: string) {
  const raw = Uint8Array.from(atob(publicKey.replaceAll('-', '+').replaceAll('_', '/')), (c) => c.charCodeAt(0))
  return raw.length === 65
    ? { alg: 'ES256', jwk: { kty: 'EC', crv: 'P-256', x: base64Url(raw.subarray(1, 33)), y: base64Url(raw.subarray(33)) } }
    : { alg: 'EdDSA', jwk: { kty: 'OKP', crv: 'Ed25519', x: publicKey } }
}

async function signJWS(header: unknown, claims: unknown, sign: Sign): Promise<string> {
  const input = segment(header) + '.' + segment(claims)
  return input + '.' + base64Url(await sign(new TextEncoder().encode(input)))
}

function segment(value: unknown): string {
  return base64Url(new TextEncoder().encode(JSON.stringify(value)))
}

export function base64Url(bytes: Uint8Array): string {
  let binary = ''
  for (const byte of bytes) binary += String.fromCharCode(byte)
  return btoa(binary).replaceAll('+', '-').replaceAll('/', '_').replace(/=+$/g, '')
}
