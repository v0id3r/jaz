import { beforeEach, describe, expect, test } from 'bun:test'
import { readSessionViewHistory, recordSessionView } from '@/lib/sessionViewHistory'

beforeEach(() => {
  const values = new Map()
  globalThis.localStorage = {
    getItem: (key) => values.get(key) ?? null,
    setItem: (key, value) => values.set(key, value),
  }
})

describe('session view history', () => {
  test('persists repeat views and keeps the hundred most recent sessions', () => {
    for (let index = 0; index < 105; index++) recordSessionView(`session-${index}`, index + 1)
    recordSessionView('session-104', 106)
    recordSessionView('session-104', 107)

    const history = readSessionViewHistory()
    expect(Object.keys(history)).toHaveLength(100)
    expect(history['session-104']).toEqual({ at: 107, count: 3 })
    expect(history['session-0']).toBeUndefined()
  })

  test('ignores malformed or negative persisted values', () => {
    localStorage.setItem('jaz.sidebar.sessionViews', JSON.stringify({
      valid: { at: 10, count: 2 },
      negative: { at: -10, count: 2 },
      zero: { at: 10, count: 0 },
      malformed: { at: 'today', count: 2 },
    }))

    expect(readSessionViewHistory()).toEqual({ valid: { at: 10, count: 2 } })
  })
})
