import { expect, test } from 'bun:test'
import { serve } from 'bun'
import { URL } from 'node:url'
import { connectComputer } from './computerConnection'
import { setTimeout } from 'node:timers/promises'

const { Response } = globalThis

test('remote websocket bridge runs without a side browser and cancellation preserves its connection', async () => {
  let peer
  const replies = new Map()
  const server = serve({
    port: 0,
    fetch(request, server) {
      if (new URL(request.url).searchParams.get('key') !== 'fixture') {
        return new Response('unauthorized', { status: 401 })
      }
      if (server.upgrade(request)) {
        return
      }
      return new Response('upgrade required', { status: 400 })
    },
    websocket: {
      open(ws) {
        peer = ws
      },
      message(_ws, message) {
        const reply = JSON.parse(message.toString())
        replies.get(reply.id)?.resolve(reply)
      },
    },
  })
  let aborted = false
  let endCount = 0
  let calls = 0
  const host = {
    status: async () => ({ available: true, platform: 'darwin', driverVersion: 'fixture' }),
    begin: async () => {},
    call: async () => {
      calls += 1
      return { status: 'ok', data: { answer: 42 } }
    },
    cancel: async () => {
      aborted = true
    },
    end: async () => {
      endCount += 1
    },
  }
  const disconnect = connectComputer(async () => new WebSocket('ws://127.0.0.1:' + server.port + '/computer?key=fixture'), host, 'thread')
  const request = (id, method, params) => {
    const result = Promise.withResolvers()
    replies.set(id, result)
    peer.send(JSON.stringify({ id, method, params }))
    return result.promise
  }
  try {
    for (let i = 0; !peer && i < 100; i += 1) {
      await setTimeout(5)
    }
    expect(peer).toBeDefined()
    const first = await request(1, 'Jaz.run', { code: 'const data = await computer.call("inspect", {})' })
    expect(first.result.status).toBe('ok')
    const next = await request(2, 'Jaz.run', { code: 'nodeRepl.write(data.answer)' })
    expect(next.result.text).toContain('42')
    const running = request(3, 'Jaz.run', { code: 'await new Promise(() => {})' })
    await setTimeout(10)
    const status = await request(4, 'Jaz.status')
    expect(status.result.data.driverVersion).toBe('fixture')
    peer.send(JSON.stringify({ method: 'Jaz.cancel', params: { id: 3 } }))
    expect((await running).error.message).toContain('cancelled')
    expect(aborted).toBe(true)
    expect((await request(5, 'Jaz.status')).result.status).toBe('connected')
    expect(calls).toBe(1)
    expect(endCount).toBe(3)
    const last = request(6, 'Jaz.run', { code: 'await new Promise(() => {})' })
    void last
    await setTimeout(10)
    aborted = false
    disconnect()
    await setTimeout(10)
    expect(aborted).toBe(true)
  } finally {
    disconnect()
    server.stop(true)
  }
})
