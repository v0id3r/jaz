import { expect, test } from 'bun:test'
import { build } from 'bun'

const bundle = await build({
  entrypoints: [new URL('./useSessionPresence.ts', import.meta.url).pathname],
  target: 'browser',
  format: 'cjs',
  external: ['react', '@/lib/api/client'],
})
if (!bundle.success) throw new AggregateError(bundle.logs)
const compiled = new Function('module', 'exports', 'require', await bundle.outputs[0].text())

function load(calls) {
  const module = { exports: {} }
  compiled(module, module.exports, (name) => {
    if (name === 'react') return { useEffect: () => {} }
    if (name === '@/lib/api/client') return { put: async (_path, body) => { calls.push(body.session_id) } }
    throw new Error(`Unexpected import: ${name}`)
  })
  return module.exports
}

test('session presence follows focus, heartbeat and cleanup', async () => {
  const calls = []
  const listeners = {}
  const state = { visible: true, focused: true, heartbeat: undefined }
  globalThis.document = {
    visibilityState: 'visible',
    hasFocus: () => state.focused,
    addEventListener: (name, handler) => { listeners[name] = handler },
    removeEventListener: () => {},
  }
  globalThis.window = {
    setInterval: (handler) => { state.heartbeat = handler; return 1 },
    clearInterval: () => {},
    addEventListener: (name, handler) => { listeners[name] = handler },
    removeEventListener: () => {},
  }
  const { startSessionPresence } = load(calls)
  const stop = startSessionPresence('thread-one')
  await flush()
  expect(calls).toEqual(['thread-one'])
  state.focused = false
  listeners.blur()
  await flush()
  expect(calls).toEqual(['thread-one', ''])
  state.focused = true
  listeners.focus()
  await flush()
  state.heartbeat()
  await flush()
  expect(calls).toEqual(['thread-one', '', 'thread-one', 'thread-one'])
  stop()
  await flush()
  expect(calls.at(-1)).toBe('')
})

async function flush() {
  await Promise.resolve()
  await Promise.resolve()
  await Promise.resolve()
}
