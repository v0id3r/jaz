self.addEventListener('install', () => {
  self.skipWaiting()
})

self.addEventListener('activate', (event) => {
  event.waitUntil(self.clients.claim())
})

self.addEventListener('push', (event) => {
  let data = {}
  try {
    data = event.data ? event.data.json() : {}
  } catch {}
  const session = typeof data.session_id === 'string' ? data.session_id : ''
  event.waitUntil(self.registration.showNotification(data.title || 'Jaz', {
    body: typeof data.body === 'string' ? data.body : '',
    tag: session ? `jaz:${session}:${data.kind || 'update'}` : 'jaz',
    data: { session, kind: data.kind || 'chat' },
  }))
})

async function openInClient(client, path, waitForAck) {
  if (!waitForAck) {
    client.postMessage({ type: 'jaz:open', path })
    return true
  }
  const channel = new MessageChannel()
  const acknowledged = new Promise((resolve) => {
    channel.port1.onmessage = () => resolve(true)
  })
  client.postMessage({ type: 'jaz:open', path }, [channel.port2])
  return Promise.race([acknowledged, new Promise((resolve) => setTimeout(() => resolve(false), 250))])
}

self.addEventListener('notificationclick', (event) => {
  event.notification.close()
  const { session, kind = 'chat' } = event.notification.data
  const path = session ? `/${kind === 'chat' ? 'sessions' : 'bots'}/${encodeURIComponent(session)}` : '/'
  event.waitUntil((async () => {
    const url = new URL(path, self.location.origin).href
    const ios = /iPhone|iPad/.test(self.navigator?.userAgent || '')
    const windows = await self.clients.matchAll({ type: 'window', includeUncontrolled: true })
    for (const client of windows) {
      if ('focus' in client && await openInClient(client, path, ios)) {
        if (!client.focused) await client.focus().catch(() => {})
        return
      }
    }
    await self.clients.openWindow(url)
  })())
})
