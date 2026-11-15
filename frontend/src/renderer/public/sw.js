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

self.addEventListener('notificationclick', (event) => {
  event.notification.close()
  const { session, kind = 'chat' } = event.notification.data
  const path = session ? `/${kind === 'chat' ? 'sessions' : 'bots'}/${encodeURIComponent(session)}` : '/'
  event.waitUntil((async () => {
    const cache = await caches.open('jaz-notification-target')
    await cache.put('/', new Response(path))
    const windows = await self.clients.matchAll({ type: 'window', includeUncontrolled: true })
    for (const client of windows) {
      if ('focus' in client) {
        client.postMessage({ type: 'jaz:open' })
        await client.focus()
        return
      }
    }
    await self.clients.openWindow(new URL(path, self.location.origin).href)
  })())
})
