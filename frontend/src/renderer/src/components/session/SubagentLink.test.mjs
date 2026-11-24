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
const { SubagentTranscriptContext } = await import('./SubagentLink')

const render = (element) => renderMarkup(
  createElement(QueryClientProvider, { client: new QueryClient() }, element),
)

const call = { id: 'spawn-1', title: 'spawnAgent', status: 'completed', subagent_id: 'child-1' }
const access = (ids) => ({ open: () => {}, transcripts: new Set(ids) })

test('a tool call linked to a subagent with a transcript opens it from the card', () => {
  const markup = render(createElement(SubagentTranscriptContext, {
    value: access(['child-1']),
    children: createElement(ToolCallDetail, { call, active: false }),
  }))
  expect(markup).toContain('Open the subagent&#x27;s transcript')
})

test('no transcript control without a link or without a held transcript', () => {
  const unlinked = render(createElement(SubagentTranscriptContext, {
    value: access(['child-1']),
    children: createElement(ToolCallDetail, { call: { ...call, subagent_id: undefined }, active: false }),
  }))
  expect(unlinked).not.toContain('subagent')
  // A link the session holds no transcript for (for example Claude on v2)
  // stays unopenable from the card; Overview remains the way in.
  const untranscribed = render(createElement(SubagentTranscriptContext, {
    value: access([]),
    children: createElement(ToolCallDetail, { call, active: false }),
  }))
  expect(untranscribed).not.toContain('subagent')
  const outsideSession = render(createElement(ToolCallDetail, { call, active: false }))
  expect(outsideSession).not.toContain('subagent')
})
