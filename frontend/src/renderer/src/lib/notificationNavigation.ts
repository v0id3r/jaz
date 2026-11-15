const CACHE_NAME = 'jaz-notification-target'
const TARGET_KEY = '/'

export async function saveNotificationTarget(path: string): Promise<void> {
  const cache = await caches.open(CACHE_NAME)
  await cache.put(TARGET_KEY, new Response(path))
}

export async function consumeNotificationTarget(): Promise<string | null> {
  const cache = await caches.open(CACHE_NAME)
  const response = await cache.match(TARGET_KEY)
  if (!response) return null
  const path = await response.text()
  await cache.delete(TARGET_KEY)
  return /^\/(?:sessions|bots)\/[^/]+$/.test(path) ? path : null
}
