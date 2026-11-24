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

const { ToolCallDetail } = await import('./ToolCallContent')
const { ToolCancelContext } = await import('./ToolCancel')

const render = (element) => renderMarkup(
  createElement(QueryClientProvider, { client: new QueryClient() }, element),
)

const call = (extra = {}) => ({
  id: 'call-1',
  title: 'Run tests',
  tool_name: 'Bash',
  status: 'in_progress',
  ...extra,
})

test('a running tool call the agent cancels shows its stop control', () => {
  const markup = render(createElement(ToolCancelContext, {
    value: async () => {},
    children: createElement(ToolCallDetail, { call: call({ cancellable: true }), active: true }),
  }))
  expect(markup).toContain('Stop this tool call')
})

test('no stop control without the capability, off v2, or once the call ended', () => {
  const withoutCapability = render(createElement(ToolCancelContext, {
    value: async () => {},
    children: createElement(ToolCallDetail, { call: call(), active: true }),
  }))
  expect(withoutCapability).not.toContain('Stop this tool call')
  const outsideSession = render(createElement(ToolCallDetail, { call: call({ cancellable: true }), active: true }))
  expect(outsideSession).not.toContain('Stop this tool call')
  const finished = render(createElement(ToolCancelContext, {
    value: async () => {},
    children: createElement(ToolCallDetail, { call: call({ cancellable: true, status: 'completed' }), active: true }),
  }))
  expect(finished).not.toContain('Stop this tool call')
})
