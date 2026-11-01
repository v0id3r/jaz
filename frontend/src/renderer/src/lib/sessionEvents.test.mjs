import { describe, expect, test } from 'bun:test'
import { coalesceSessionEvents, liveAfterCursor, mergeSessionEvent } from './sessionEvents'

function text(seq, content, projectionOp) {
  return {
    seq,
    projection_key: 'acp_text:thread:agent:acp_message:message:one:1',
    projection_op: projectionOp,
    session_id: 'thread',
    type: 'acp_message',
    content,
    acp: {
      id: 'agent',
      slug: 'agent',
      agent: 'codex',
      session_id: 'agent-session',
      state: 'running',
      text_run_id: 'message:one',
    },
    at: new Date(seq * 1000).toISOString(),
  }
}

function status(seq, state, extra = {}) {
  return {
    ...text(seq, ''),
    type: 'acp',
    acp: { ...text(seq, '').acp, state, ...extra },
  }
}

describe('session event text projection', () => {
  test('folds live deltas once and ignores their replay', () => {
    const live = [text(1, 'Hel', 'append'), text(2, 'lo', 'append'), text(2, 'lo', 'append')].reduce(mergeSessionEvent, [])

    expect(live).toHaveLength(1)
    expect(live[0].content).toBe('Hello')
    expect(live[0].seq).toBe(2)
  })

  test('cuts a live run exactly at the snapshot cursor', () => {
    const live = ['A', 'B', 'C', 'D', 'E'].map((chunk, i) => text(i + 1, chunk, 'append')).reduce(mergeSessionEvent, [])
    const tail = liveAfterCursor(live, 3)
    const events = coalesceSessionEvents([text(3, 'ABC', 'replace'), ...mergeSessionEvent(tail, text(6, 'F', 'append'))])

    expect(tail[0].content).toBe('DE')
    expect(liveAfterCursor(live, 5)).toEqual([])
    expect(liveAfterCursor(live, 0)).toBe(live)
    expect(events).toHaveLength(1)
    expect(events[0].content).toBe('ABCDEF')
  })

  test('a server projection holds its whole run, whatever arrived around it', () => {
    const events = coalesceSessionEvents([
      text(2, 'He', 'replace'),
      text(3, 'Hel', 'replace'),
      text(5, 'Hello', 'replace'),
      text(3, 'l', 'append'),
      text(4, 'l', 'append'),
      text(6, ' world', 'append'),
    ])

    expect(events).toHaveLength(1)
    expect(events[0].content).toBe('Hello world')
  })

  test('retains an older distinct event during reconnect replay', () => {
    const older = { ...status(1, 'running'), projection_key: 'acp_status:agent' }
    const events = mergeSessionEvent([text(2, 'current')], older)

    expect(events).toHaveLength(2)
    expect(events[1]).toBe(older)
  })

  test('replaces semantic projections using backend identity', () => {
    const tool = (seq, toolStatus) => status(seq, 'running', {
      tool_calls: [{ id: 'tool-1', title: 'Read', status: toolStatus }],
    })
    const events = coalesceSessionEvents([
      { ...tool(1, 'pending'), type: 'acp_tool', projection_key: 'acp_tool:agent:tool-1' },
      {
        ...tool(2, 'completed'),
        type: 'acp_tool',
        projection_key: 'acp_tool:agent:tool-1',
        projection_op: 'replace',
      },
    ])

    expect(events).toHaveLength(1)
    expect(events[0].acp.tool_calls[0].status).toBe('completed')
  })

  test('takes an in-place update at the same seq by its stream position', () => {
    const tool = (streamSeq, toolStatus) => ({
      ...status(1, 'running', { tool_calls: [{ id: 'tool-1', title: 'Read', status: toolStatus }] }),
      type: 'acp_tool',
      stream_seq: streamSeq,
      projection_key: 'acp_tool:agent:tool-1',
      projection_op: 'replace',
    })
    const live = [tool(1, 'pending'), text(2, 'Hi'), tool(3, 'completed'), tool(1, 'pending')].reduce(mergeSessionEvent, [])
    const history = coalesceSessionEvents([tool(1, 'pending'), text(2, 'Hi'), ...live])

    for (const events of [live, history]) {
      expect(events.map((event) => event.seq)).toEqual([1, 2])
      expect(events[0].acp.tool_calls[0].status).toBe('completed')
    }
  })

  test('combines a compact snapshot with later append operations', () => {
    const events = coalesceSessionEvents([text(2, 'Hello'), text(3, ' world', 'append')])

    expect(events).toHaveLength(1)
    expect(events[0].content).toBe('Hello world')
  })
})
