import { expect, test } from 'bun:test'

globalThis.window = {
  jaz: undefined,
  addEventListener: () => {},
  matchMedia: () => ({
    matches: false,
    addEventListener: () => {},
    removeEventListener: () => {},
  }),
}
globalThis.localStorage = {
  getItem: () => null,
  setItem: () => {},
  removeItem: () => {},
}
globalThis.document = {
  documentElement: {
    classList: { add: () => {}, remove: () => {}, toggle: () => {}, contains: () => false },
    style: { setProperty: () => {}, removeProperty: () => {} },
  },
}

const { stateDot } = await import('./SessionRow')

test('an idle thread with live background work is marked as waiting on it', () => {
  const dot = stateDot({ status: 'idle', background_work: true })
  expect(dot?.title).toBe('Waiting for a background task')
})

test('background work does not mask running, failed or unread states', () => {
  expect(stateDot({ status: 'running', background_work: true })?.title).toBe('Running')
  expect(stateDot({ status: 'error', background_work: true })?.title).toBe('Failed')
  expect(stateDot({ status: 'idle', background_work: true, unread: true })?.title).toBe('Waiting for a background task')
  expect(stateDot({ status: 'idle', unread: true })?.title).toBe('Unread reply')
  expect(stateDot({ status: 'idle' })).toBeNull()
})
