import { afterEach, expect, test } from 'bun:test'
import { build } from 'bun'
import { readFileSync } from 'node:fs'

const bundle = await build({ entrypoints: [new URL('./webPush.ts', import.meta.url).pathname], target: 'browser', format: 'cjs', external: ['@/lib/api/client'] })
if (!bundle.success) throw new AggregateError(bundle.logs)
const compiled = new Function('module', 'exports', 'require', await bundle.outputs[0].text())

function load() {
  const module = { exports: {} }
  compiled(module, module.exports, (name) => {
    if (name !== '@/lib/api/client') throw new Error(`Unexpected import: ${name}`)
    return {
      get: async (path) => {
        const response = await fetch(path)
        return response.json()
      },
      put: async (path, body) => {
        const response = await fetch(path, { method: 'PUT', body: JSON.stringify(body) })
        if (!response.ok) throw new Error(String(response.status))
      },
      del: async (path) => {
        const response = await fetch(path, { method: 'DELETE' })
        if (!response.ok) throw new Error(String(response.status))
      },
    }
  })
  return module.exports
}

const originals = new Map()

afterEach(() => {
  for (const [name, descriptor] of originals) {
    if (descriptor) Object.defineProperty(globalThis, name, descriptor)
    else delete globalThis[name]
  }
  originals.clear()
})

function replace(name, value) {
  if (!originals.has(name)) originals.set(name, Object.getOwnPropertyDescriptor(globalThis, name))
  Object.defineProperty(globalThis, name, { configurable: true, writable: true, value })
}

test('PWA subscribes with the server VAPID key and registers the paired device', async () => {
  const f = fixture()
  const module = load()
  await module.subscribeWebPush()
  expect(f.calls.map((call) => call.init?.method || 'GET')).toEqual(['GET', 'PUT'])
  expect(JSON.parse(f.calls[1].init.body)).toEqual({ endpoint: 'https://push.example/device', keys: { p256dh: 'key', auth: 'auth' } })
  expect(f.subscriptionOptions.applicationServerKey.byteLength).toBe(65)
  expect(f.unsubscribed).toBe(0)
})

test('failed server registration does not leave an orphaned local subscription', async () => {
  const f = fixture({ putStatus: 500 })
  const module = load()
  await expect(module.subscribeWebPush()).rejects.toThrow('500')
  expect(f.unsubscribed).toBe(1)
})

test('disabling removes the server registration and local subscription', async () => {
  const f = fixture({ existing: true })
  const module = load()
  await module.unsubscribeWebPush()
  expect(f.calls[0].init.method).toBe('DELETE')
  expect(f.unsubscribed).toBe(1)
})

test('service worker shows a push and opens its thread on click', async () => {
  const listeners = {}
  const shown = []
  const opened = []
  const self = {
    registration: { showNotification: async (title, options) => shown.push({ title, ...options }) },
    clients: {
      matchAll: async () => [],
      openWindow: async (url) => opened.push(url),
    },
    location: { origin: 'https://jaz.example' },
    addEventListener: (type, handler) => { listeners[type] = handler },
  }
  new Function('self', readFileSync(new URL('../../public/sw.js', import.meta.url), 'utf8'))(self)
  const waiting = []
  listeners.push({ data: { json: () => ({ title: 'Done', body: 'Ready', session_id: 'thread one', kind: 'chat' }) }, waitUntil: (promise) => waiting.push(promise) })
  await Promise.all(waiting)
  expect(shown[0]).toMatchObject({ title: 'Done', body: 'Ready', tag: 'jaz:thread one:chat' })
  const notification = { data: shown[0].data, close: () => {} }
  listeners.notificationclick({ notification, waitUntil: (promise) => waiting.push(promise) })
  await Promise.all(waiting)
  expect(opened).toEqual(['https://jaz.example/sessions/thread%20one'])
})

test('notification click routes a bot to the existing client and navigates to it', async () => {
  const listeners = {}
  const messages = []
  const navigated = []
  let focused = 0
  const client = {
    postMessage: (message) => messages.push(message),
    focus: async () => { focused += 1 },
    navigate: async (url) => { navigated.push(url) },
  }
  const self = {
    registration: { showNotification: async () => {} },
    clients: { matchAll: async () => [client], openWindow: async () => { throw new Error('unexpected new window') } },
    location: { origin: 'https://jaz.example' },
    addEventListener: (type, handler) => { listeners[type] = handler },
  }
  new Function('self', readFileSync(new URL('../../public/sw.js', import.meta.url), 'utf8'))(self)
  const waiting = []
  listeners.push({ data: { json: () => ({ title: 'Done', session_id: 'bot one', kind: 'bot' }) }, waitUntil: (promise) => waiting.push(promise) })
  await Promise.all(waiting)
  const notification = { data: { session: 'bot one', kind: 'bot' }, close: () => {} }
  listeners.notificationclick({ notification, waitUntil: (promise) => waiting.push(promise) })
  await Promise.all(waiting)
  expect(messages).toEqual([{ type: 'jaz:open', path: '/bots/bot%20one' }])
  expect(navigated).toEqual(['https://jaz.example/bots/bot%20one'])
  expect(focused).toBe(1)
})

function fixture({ publicKey = Buffer.alloc(65, 1).toString('base64url'), existing = false, putStatus = 204 } = {}) {
  const calls = []
  const state = {
    calls,
    subscriptionOptions: undefined,
    unsubscribed: 0,
  }
  const subscription = {
    toJSON: () => ({ endpoint: 'https://push.example/device', keys: { p256dh: 'key', auth: 'auth' } }),
    unsubscribe: async () => { state.unsubscribed += 1 },
  }
  const registration = {
    pushManager: {
      getSubscription: async () => existing ? subscription : null,
      subscribe: async (options) => {
        state.subscriptionOptions = options
        return subscription
      },
    },
  }
  replace('window', { matchMedia: () => ({ matches: true }) })
  replace('navigator', {
    platform: 'MacIntel',
    serviceWorker: {
      register: async () => registration,
      getRegistration: async () => registration,
      ready: Promise.resolve(registration),
    },
  })
  replace('Notification', { requestPermission: async () => 'granted' })
  replace('localStorage', { getItem: () => null, setItem: () => {}, removeItem: () => {} })
  replace('PushManager', class {})
  replace('fetch', async (url, init) => {
    calls.push({ url, init })
    if ((init?.method || 'GET') === 'GET') {
      return new Response(JSON.stringify({ web_push_public_key: publicKey }), { status: 200, headers: { 'Content-Type': 'application/json' } })
    }
    return new Response(null, { status: putStatus })
  })
  return state
}
