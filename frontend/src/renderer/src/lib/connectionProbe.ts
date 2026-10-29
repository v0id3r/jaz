import { CLIENT_PLATFORM, CLIENT_PLATFORM_HEADER, deviceRequestHeaders, secureBackendConnection } from '@/lib/api/client'
import { clientRuntime } from '@/lib/clientRuntime'
import { deviceAccess, type ResourceTicket } from '@shared/connectionSession'
import { DeviceTokenError } from '@shared/deviceProof'
import { deviceAccessToken } from '@/lib/deviceIdentity'

export type ProbeFailure = { ok: false; error: string; recoverable: boolean; code?: string }
export type Access = { ok: true; kind: 'device' | 'root'; deviceId: string; ticket?: ResourceTicket } | ProbeFailure
export type Health = { ok: true; authRequired: boolean } | ProbeFailure

export async function readJSON<T>(response: Response): Promise<T | null> {
  try {
    return await response.json() as T
  } catch {
    return null
  }
}

function loginResponse(url: string, response: Response): boolean {
  if (clientRuntime.kind !== 'web') {
    return false
  }
  const contentType = response.headers.get('content-type') || ''
  return contentType.includes('text/html') || (response.redirected && new URL(response.url).origin !== url) ||
    ((response.status === 401 || response.status === 403) && !contentType.includes('json'))
}

export async function readHealth(url: string, signal?: AbortSignal): Promise<Health> {
  if (!secureBackendConnection(url)) {
    return { ok: false, error: 'Remote connections require HTTPS', recoverable: false }
  }
  try {
    const response = await fetch(url + '/health', { signal: signal ? AbortSignal.any([signal, AbortSignal.timeout(3000)]) : AbortSignal.timeout(3000) })
    if (loginResponse(url, response)) {
      return { ok: false, error: `Sign in through Teleport again to reach the backend at ${url}`, recoverable: false }
    }
    const body = await readJSON<{ ok?: boolean; auth_required?: boolean }>(response)
    if (response.ok && body?.ok === true && typeof body.auth_required === 'boolean') {
      return { ok: true, authRequired: body.auth_required }
    }
  } catch {
    // A health failure never unlocks a credential or issues a replacement.
  }
  return { ok: false, error: `No backend responded at ${url}`, recoverable: true }
}

export async function readAccess(url: string, token: string, signal?: AbortSignal, expectedDeviceId?: string): Promise<Access> {
  if (!token || !secureBackendConnection(url)) {
    return { ok: false, error: 'A secure device credential is required', recoverable: false }
  }
  try {
    const response = await fetch(url + '/v1/auth/check', {
      headers: { ...await deviceRequestHeaders('GET', url + '/v1/auth/check', token), [CLIENT_PLATFORM_HEADER]: CLIENT_PLATFORM },
      redirect: 'error', signal: signal ? AbortSignal.any([signal, AbortSignal.timeout(3000)]) : AbortSignal.timeout(3000),
    })
    const body = await readJSON<{ ok?: boolean; auth_kind?: string; error?: string; code?: string }>(response)
    if (body?.code === 'invalid_dpop_proof') {
      return { ok: false, error: body.error || 'Device proof was rejected', recoverable: true, code: body.code }
    }
    if (!response.ok) {
      return { ok: false, error: response.status === 401 ? 'Backend key was rejected' : body?.error || 'Backend authentication failed', recoverable: response.status >= 500, code: body?.code }
    }
    if (body?.ok === true && body.auth_kind === 'root' && expectedDeviceId === undefined) {
      return { ok: true, kind: 'root', deviceId: '' }
    }
    try {
      return { ok: true, kind: 'device', ...deviceAccess(body, expectedDeviceId) }
    } catch (error) {
      return { ok: false, error: (error as Error).message, recoverable: false }
    }
  } catch {
    return { ok: false, error: 'Backend auth check failed', recoverable: true }
  }
}

// Access for a device whose key is the credential: it first needs an access
// token, which the backend refuses with invalid_client until it approves the key.
export async function readKeyAccess(url: string, deviceId: string, signal?: AbortSignal): Promise<Access> {
  let token: string
  try {
    token = await deviceAccessToken(url)
  } catch (error) {
    const code = error instanceof DeviceTokenError ? error.code : ''
    const settled = code === 'invalid_client' || code === 'unsupported_grant_type' || code === 'unsupported_device_key'
    return { ok: false, error: code === 'invalid_client' ? 'This device is not approved on this server' : 'Could not get a device access token', recoverable: !settled, code }
  }
  return readAccess(url, token, signal, deviceId)
}
