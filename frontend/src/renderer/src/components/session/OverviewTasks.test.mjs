import { expect, test } from 'bun:test'
import { createElement } from 'react'
import { renderToStaticMarkup as renderMarkup } from 'react-dom/server'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'

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

const { BackgroundTasksSection } = await import('./OverviewTasks')
const { SessionLivenessIndicator } = await import('./SessionLivenessIndicator')

const render = (element) => renderMarkup(
  createElement(QueryClientProvider, { client: new QueryClient() }, element),
)

const session = { id: 'thread', slug: 'thread', status: 'idle' }
const task = (id, state, extra = {}) => ({
  id,
  name: `Task ${id}`,
  state,
  can_stop: true,
  show_in_transcript: false,
  at: new Date().toISOString(),
  ...extra,
})

test('the overview lists live background tasks with a stop action', () => {
  const markup = render(createElement(BackgroundTasksSection, {
    session,
    tasks: [task('a', 'running'), task('b', 'completed', { can_stop: false })],
  }))
  expect(markup).toContain('Background tasks')
  expect(markup).toContain('Task a')
  expect(markup).toContain('Task b')
  expect(markup).toContain('Stop Task a')
  expect(markup).not.toContain('Stop Task b')
})

test('a task whose connection was lost offers no stop and no success claim', () => {
  const markup = render(createElement(BackgroundTasksSection, {
    session,
    // A live task keeps the section expanded so the rows render.
    tasks: [task('live', 'running'), task('a', 'running', { connection_lost: true })],
  }))
  expect(markup).not.toContain('Stop Task a')
  expect(markup).not.toContain('Finished')
  expect(markup).toContain('Connection lost')
})

test('only a completed task shows as finished; stopped and unknown stay honest', () => {
  const markup = render(createElement(BackgroundTasksSection, {
    session,
    tasks: [
      task('live', 'running'),
      task('done', 'completed', { can_stop: false }),
      task('stopped', 'stopped', { can_stop: false }),
      task('failed', 'failed', { can_stop: false }),
    ],
  }))
  expect(markup.match(/Finished/g)).toHaveLength(1)
  expect(markup).toContain('Stopped')
  expect(markup).toContain('Failed')
})

test('an idle thread waiting on background tasks says so', () => {
  const waiting = render(createElement(SessionLivenessIndicator, {
    running: false,
    connected: true,
    updatedAt: new Date().toISOString(),
    backgroundTasks: 2,
  }))
  expect(waiting).toContain('Waiting for 2 background tasks')
  const single = render(createElement(SessionLivenessIndicator, {
    running: false,
    connected: true,
    updatedAt: new Date().toISOString(),
    backgroundTasks: 1,
  }))
  expect(single).toContain('Waiting for a background task')
  const idle = render(createElement(SessionLivenessIndicator, {
    running: false,
    connected: true,
    updatedAt: new Date().toISOString(),
  }))
  expect(idle).not.toContain('Waiting')
})
