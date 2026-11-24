import { describe, expect, test } from 'bun:test'
import { agentTaskLive, agentTasksFromSources } from './agentTasks'

function taskEvent(id, state, at, extra = {}) {
  return {
    session_id: 'thread',
    type: 'agent_task',
    projection_key: `agent_task:${id}`,
    projection_op: 'replace',
    agent_task: { id, name: `Task ${id}`, state, can_stop: state === 'running', show_in_transcript: false, ...extra },
    at,
  }
}

describe('agentTasksFromSources', () => {
  test('merges stored and live reports of one task, latest wins', () => {
    const stored = [taskEvent('a', 'running', '2026-11-24T10:00:00Z')]
    const live = [taskEvent('a', 'completed', '2026-11-24T10:05:00Z')]
    const tasks = agentTasksFromSources(stored, live)
    expect(tasks).toHaveLength(1)
    expect(tasks[0].state).toBe('completed')
    expect(tasks[0].at).toBe('2026-11-24T10:05:00Z')
  })

  test('orders live tasks before finished ones, latest report first', () => {
    const tasks = agentTasksFromSources(undefined, [
      taskEvent('done', 'completed', '2026-11-24T10:09:00Z'),
      taskEvent('old', 'running', '2026-11-24T10:01:00Z'),
      taskEvent('new', 'paused', '2026-11-24T10:03:00Z'),
    ])
    expect(tasks.map((task) => task.id)).toEqual(['new', 'old', 'done'])
  })

  test('ignores other events and tasks without an id', () => {
    const tasks = agentTasksFromSources(undefined, [
      { session_id: 'thread', type: 'acp_message', content: 'hi', at: '2026-11-24T10:00:00Z' },
      { session_id: 'thread', type: 'agent_task', agent_task: { name: 'nameless' }, at: '2026-11-24T10:00:00Z' },
    ])
    expect(tasks).toEqual([])
  })
})

describe('agentTaskLive', () => {
  test('running and paused tasks are live, finished are not', () => {
    expect(agentTaskLive({ id: 'a', name: '', state: 'running', can_stop: false, show_in_transcript: false })).toBe(true)
    expect(agentTaskLive({ id: 'a', name: '', state: 'paused', can_stop: false, show_in_transcript: false })).toBe(true)
    expect(agentTaskLive({ id: 'a', name: '', state: 'completed', can_stop: false, show_in_transcript: false })).toBe(false)
  })

  test('a task whose connection was lost is not live', () => {
    expect(agentTaskLive({ id: 'a', name: '', state: 'running', can_stop: false, show_in_transcript: false, connection_lost: true })).toBe(false)
  })
})
