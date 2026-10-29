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

export function deviceAuthHeaders(token: string, proof: string): Record<string, string> {
  return proof ? { Authorization: `DPoP ${token}`, DPoP: proof } : { Authorization: `Bearer ${token}` }
}

export class DeviceTokenError extends Error {
  constructor(readonly code: string) {
    super(code)
  }
}

export type DeviceAccessToken = { token: string; expiresAt: number }
export type DeviceKey = { device_id: string; public_key: string }

// Gets an access token from a proof-mode backend: the client_credentials grant
// with a private_key_jwt assertion (RFC 7523) and a DPoP proof, both signed by
// the device key. Fails with the OAuth error code, e.g. invalid_client for a
// device the backend has not approved or unsupported_grant_type off proof mode.
export async function requestDeviceAccessToken(baseUrl: string, key: DeviceKey, sign: Sign): Promise<DeviceAccessToken> {
  const url = baseUrl + '/v1/devices/token'
  const assertion = await signJWS({ typ: 'client-authentication+jwt', alg: keyHeader(key.public_key).alg }, {
    iss: key.device_id, sub: key.device_id, aud: new URL(baseUrl).origin, jti: crypto.randomUUID(), exp: Math.floor(Date.now() / 1000) + 30,
  }, sign)
  const response = await fetch(url, {
    method: 'POST', redirect: 'error', signal: AbortSignal.timeout(5000),
    headers: { 'Content-Type': 'application/x-www-form-urlencoded', DPoP: await buildDeviceProof(key.public_key, sign, 'POST', url) },
    body: new URLSearchParams({ grant_type: 'client_credentials', client_assertion_type: 'urn:ietf:params:oauth:client-assertion-type:jwt-bearer', client_assertion: assertion }),
  })
  const body = await response.json().catch(() => null) as { access_token?: unknown; token_type?: unknown; expires_in?: unknown; error?: unknown } | null
  if (!response.ok || body?.token_type !== 'DPoP' || typeof body.access_token !== 'string' || typeof body.expires_in !== 'number') {
    throw new DeviceTokenError(typeof body?.error === 'string' ? body.error : 'token_request_failed')
  }
  return { token: body.access_token, expiresAt: Date.now() + body.expires_in * 1000 }
}

// One access token per backend, renewed shortly before it expires or once the
// backend rejects it.
export function deviceAccessTokens(request: (baseUrl: string) => Promise<DeviceAccessToken>) {
  const cache = new Map<string, Promise<DeviceAccessToken>>()
  const renew = (baseUrl: string) => {
    const next = request(baseUrl)
    cache.set(baseUrl, next)
    next.catch(() => cache.get(baseUrl) === next && cache.delete(baseUrl))
    return next
  }
  return async (baseUrl: string, rejected?: string): Promise<string> => {
    const pending = cache.get(baseUrl) ?? renew(baseUrl)
    const current = await pending
    if (current.token !== rejected && current.expiresAt - 30_000 > Date.now()) return current.token
    return (await (cache.get(baseUrl) === pending ? renew(baseUrl) : cache.get(baseUrl) ?? renew(baseUrl))).token
  }
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
