// A DPoP proof (RFC 9449) that the device key signed this one request. The
// server matches htu by path, so the origin a proxy rewrites does not matter.
// publicKey is the raw key: 32 bytes of Ed25519 or a 65-byte P-256 point.
export async function buildDeviceProof(
  publicKey: string,
  sign: (input: Uint8Array) => Promise<Uint8Array>,
  method: string,
  url: string,
  token?: string,
): Promise<string> {
  const target = new URL(url)
  const raw = Uint8Array.from(atob(publicKey.replaceAll('-', '+').replaceAll('_', '/')), (c) => c.charCodeAt(0))
  const header = raw.length === 65
    ? { typ: 'dpop+jwt', alg: 'ES256', jwk: { kty: 'EC', crv: 'P-256', x: base64Url(raw.subarray(1, 33)), y: base64Url(raw.subarray(33)) } }
    : { typ: 'dpop+jwt', alg: 'EdDSA', jwk: { kty: 'OKP', crv: 'Ed25519', x: publicKey } }
  const claims = {
    jti: crypto.randomUUID(),
    htm: method.toUpperCase(),
    htu: target.origin + target.pathname,
    iat: Math.floor(Date.now() / 1000),
    ...(token ? { ath: base64Url(new Uint8Array(await crypto.subtle.digest('SHA-256', new TextEncoder().encode(token)))) } : {}),
  }
  const input = segment(header) + '.' + segment(claims)
  return input + '.' + base64Url(await sign(new TextEncoder().encode(input)))
}

export function deviceAuthHeaders(token: string, proof: string): Record<string, string> {
  return proof ? { Authorization: `DPoP ${token}`, DPoP: proof } : { Authorization: `Bearer ${token}` }
}

function segment(value: unknown): string {
  return base64Url(new TextEncoder().encode(JSON.stringify(value)))
}

export function base64Url(bytes: Uint8Array): string {
  let binary = ''
  for (const byte of bytes) binary += String.fromCharCode(byte)
  return btoa(binary).replaceAll('+', '-').replaceAll('/', '_').replace(/=+$/g, '')
}
