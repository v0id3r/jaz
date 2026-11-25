import type { Session } from 'electron'
import { LOCAL_BACKEND_SCHEME, LOCAL_BACKEND_TRANSPORT_URL } from '@shared/localBackend'
import { RENDERER_HOST, RENDERER_SCHEME } from '@main/permissions'

export function configureLocalBackendTransport(target: Session, backendURL: string): void {
  const rendererURL = process.env.ELECTRON_RENDERER_URL
  const origin = rendererURL ? new URL(rendererURL).origin : `${RENDERER_SCHEME}://${RENDERER_HOST}`
  target.protocol.handle(LOCAL_BACKEND_SCHEME, async (request) => {
    const url = new URL(request.url)
    if (url.host !== 'backend' || url.username || url.password ||
      !('initiatorOrigin' in request) || request.initiatorOrigin !== origin) {
      return new Response('Forbidden', { status: 403 })
    }
    // Node's loopback transport stays available when Chromium reports the computer offline.
    const response = await fetch(new Request(backendURL + url.pathname + url.search, request), { redirect: 'manual' })
    const location = response.headers.get('Location')
    if (!location || ![301, 302, 303, 307, 308].includes(response.status)) return response
    const redirect = new URL(location, backendURL + url.pathname + url.search)
    if (redirect.origin !== new URL(backendURL).origin) {
      await response.body?.cancel()
      return Response.error()
    }
    const headers = new Headers(response.headers)
    headers.set('Location', LOCAL_BACKEND_TRANSPORT_URL + redirect.pathname + redirect.search + redirect.hash)
    return new Response(response.body, { status: response.status, statusText: response.statusText, headers })
  })
}
