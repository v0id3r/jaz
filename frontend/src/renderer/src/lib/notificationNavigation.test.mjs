import { expect, test } from 'bun:test'
import { consumeNotificationTarget, saveNotificationTarget } from './notificationNavigation'

test('notification target survives until the app consumes it once', async () => {
  const stored = new Map()
  globalThis.caches = {
    open: async () => ({
      put: async (key, response) => { stored.set(key, await response.text()) },
      match: async (key) => stored.has(key) ? new Response(stored.get(key)) : undefined,
      delete: async (key) => stored.delete(key),
    }),
  }
  await saveNotificationTarget('/bots/bot%20one')
  expect(await consumeNotificationTarget()).toBe('/bots/bot%20one')
  expect(await consumeNotificationTarget()).toBeNull()
})
