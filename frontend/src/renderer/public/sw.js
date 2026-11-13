self.addEventListener('push', (event) => {
  let data = {}
  try {
    data = event.data ? event.data.json() : {}
  } catch {}
  const session = typeof data.session_id === 'string' ? data.session_id : ''
  event.waitUntil(self.registration.showNotification(data.title || 'Jaz', {
    body: typeof data.body === 'string' ? data.body : '',
    tag: session ? `jaz:${session}:${data.kind || 'update'}` : 'jaz',
    data: { session },
  }))
})

self.addEventListener('notificationclick', (event) => {
  event.notification.close()
  const path = event.notification.data.session ? `/sessions/${encodeURIComponent(event.notification.data.session)}` : '/'
  event.waitUntil((async () => {
    const windows = await self.clients.matchAll({ type: 'window', includeUncontrolled: true })
    for (const client of windows) {
      if ('focus' in client) {
        await client.navigate(new URL(path, self.location.origin).href)
        await client.focus()
        return
      }
    }
    await self.clients.openWindow(new URL(path, self.location.origin).href)
  })())
})
