import { describe, expect, test } from 'bun:test'
import { providerSubagentsFromSources, subagentRunning, subagentTranscriptEntries } from './providerSubagents'

function subagent(owner, name) {
  return {
    session_id: 'parent',
    type: 'provider_subagent',
    projection_key: `provider_subagent:${owner}:codex:worker`,
    provider_subagent: {
      provider: 'codex',
      id: 'worker',
      name,
      status: 'running',
    },
    at: '2026-07-19T12:00:00Z',
  }
}

describe('provider subagent projections', () => {
  test('uses backend identity when provider-local ids collide', () => {
    const subagents = providerSubagentsFromSources(undefined, [
      subagent('first-child', 'Newton'),
      subagent('second-child', 'Noether'),
    ])

    expect(subagents).toHaveLength(2)
    expect(subagents.map((item) => item.name).sort()).toEqual(['Newton', 'Noether'])
  })

  test('keeps complete stored history while merging sparse live updates', () => {
    const subagents = providerSubagentsFromSources(
      [{
        key: 'provider_subagent:codex:/root/newton',
        seq: 10,
        provider: 'codex',
        id: '/root/newton',
        name: 'Newton',
        task: 'Audit the proof',
        status: 'running',
        updated_at: '2026-07-19T12:00:00Z',
      }],
      [{
        session_id: 'parent',
        type: 'provider_subagent',
        provider_subagent: { provider: 'codex', id: '/root/newton', status: 'completed' },
        at: '2026-07-19T13:00:00Z',
      }],
    )

    expect(subagents).toHaveLength(1)
    expect(subagents[0]).toMatchObject({ name: 'Newton', task: 'Audit the proof', status: 'completed' })
  })

  test('takes a live update of the stored row at the same seq', () => {
    const subagents = providerSubagentsFromSources(
      [{ ...subagent('parent', 'Newton').provider_subagent, key: 'provider_subagent:parent:codex:worker', seq: 10, updated_at: '2026-07-19T12:00:00Z' }],
      [{
        ...subagent('parent', 'Newton'),
        seq: 10,
        provider_subagent: { provider: 'codex', id: 'worker', status: 'completed' },
        at: '2026-07-19T13:00:00Z',
      }],
    )

    expect(subagents[0]).toMatchObject({ name: 'Newton', status: 'completed' })
  })

  test('does not let an older transcript event regress the complete projection', () => {
    const subagents = providerSubagentsFromSources(
      [{
        key: 'provider_subagent:codex:/root/newton',
        seq: 20,
        provider: 'codex',
        id: '/root/newton',
        name: 'Newton',
        status: 'completed',
        updated_at: '2026-07-19T13:00:00Z',
      }],
      [{
        session_id: 'parent',
        type: 'provider_subagent',
        provider_subagent: { provider: 'codex', id: '/root/newton', status: 'running' },
        seq: 10,
        at: '2026-07-19T12:00:00Z',
      }],
    )

    expect(subagents).toHaveLength(1)
    expect(subagents[0]).toMatchObject({ name: 'Newton', status: 'completed' })
    expect(subagents[0].updated_at).toBe('2026-07-19T13:00:00Z')
  })

  test('shows legacy events without projection keys', () => {
    const subagents = providerSubagentsFromSources(undefined, [{
      session_id: 'parent',
      type: 'provider_subagent',
      provider_subagent: { provider: 'codex', id: '/root/legacy', name: 'Legacy' },
      at: '2026-07-19T12:00:00Z',
    }])

    expect(subagents).toHaveLength(1)
    expect(subagents[0].name).toBe('Legacy')
  })

  test('lists the subagents still at work first and counts only those as running', () => {
    const report = (id, status, at) => ({
      session_id: 'parent',
      type: 'provider_subagent',
      provider_subagent: { provider: 'codex', id, status },
      at,
    })
    const subagents = providerSubagentsFromSources(undefined, [
      report('reader', 'running', '2026-07-19T10:00:00Z'),
      report('newcomer', undefined, '2026-07-19T11:00:00Z'),
      report('writer', 'completed', '2026-07-19T12:00:00Z'),
      report('checker', 'failed', '2026-07-19T13:00:00Z'),
      report('closer', 'cancelled', '2026-07-19T14:00:00Z'),
    ])

    expect(subagents.map((item) => item.id)).toEqual(['newcomer', 'reader', 'closer', 'checker', 'writer'])
    expect(subagents.filter(subagentRunning).map((item) => item.id)).toEqual(['newcomer', 'reader'])
  })
})

describe('subagent transcript entries', () => {
  const entry = (id, role, content, status) => ({
    session_id: 'parent',
    type: 'subagent_message',
    subagent_message: { id, role, content, ...(status ? { status } : {}) },
    at: '2026-11-24T08:00:00Z',
  })

  test('keeps one subagent\'s entries, coalescing consecutive chunks of a role', () => {
    const entries = subagentTranscriptEntries([
      entry('child-1', 'user', 'go research'),
      entry('child-2', 'assistant', 'other child'),
      entry('child-1', 'assistant', 'child '),
      entry('child-1', 'assistant', 'findings'),
      entry('child-1', 'tool', 'Bash', 'completed'),
      entry('child-1', 'assistant', 'done'),
    ], 'child-1')

    expect(entries).toEqual([
      { role: 'user', content: 'go research' },
      { role: 'assistant', content: 'child findings' },
      { role: 'tool', content: 'Bash', status: 'completed' },
      { role: 'assistant', content: 'done' },
    ])
  })
})
