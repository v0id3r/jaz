import type { SessionEvent } from '@/lib/api/types'
import { taskSurfaceFromEvent } from '@/lib/taskSurface'

// A child agent's run as seen from its parent. Bare child status belongs in
// Overview > Threads; child task surfaces can still stay inline when actionable.
export function isParentChildACPEvent(event: SessionEvent): boolean {
  return Boolean(
    event.acp?.parent_id &&
      event.acp.parent_id === event.session_id &&
      event.acp.id !== event.session_id,
  )
}

export type SessionEventPlacement = 'transcript' | 'overview' | 'side_chat' | 'internal'

export function sessionEventPlacement(event: SessionEvent): SessionEventPlacement {
  switch (event.type) {
    case 'voice_diagnostic':
      return 'internal'
    case 'acp':
      return isParentChildACPEvent(event) && !taskSurfaceFromEvent(event) ? 'overview' : 'transcript'
    case 'agent_session':
    case 'agent_task':
    case 'provider_subagent':
      return 'overview'
    case 'goal_update':
    case 'goal_clear':
      return 'overview'
    case 'side_chat_message':
      return 'side_chat'
    default:
      return 'transcript'
  }
}

export function sessionEventCoalesceKey(event: SessionEvent): string {
  return event.projection_key ?? ''
}

// A live text run remembers where each folded delta starts, so a snapshot
// cursor can cut it exactly.
export type LiveSessionEvent = SessionEvent & { live_chunks?: { stream_seq: number; start: number }[] }

// The live tail holds one entry per projection: a replace supersedes the held
// copy, a text delta folds into its run.
export function mergeSessionEvent(prev: LiveSessionEvent[], event: LiveSessionEvent): LiveSessionEvent[] {
  const key = sessionEventCoalesceKey(event)
  const index = prev.findLastIndex((candidate) =>
    (event.seq && candidate.session_id === event.session_id && candidate.seq === event.seq) ||
    (key && sessionEventCoalesceKey(candidate) === key),
  )
  if (index === -1) return [...prev, event.projection_op === 'append' ? appendLiveText(undefined, event) : event]
  if (event.seq && streamSeq(prev[index]) >= streamSeq(event)) return prev
  const next = [...prev]
  next[index] = event.projection_op === 'append' ? appendLiveText(prev[index], event) : event
  return next
}

// The part of the live tail a snapshot taken at the cursor does not hold.
export function liveAfterCursor(events: LiveSessionEvent[], cursor: number): LiveSessionEvent[] {
  let changed = false
  const next = events.flatMap((event) => {
    const kept = eventAfterCursor(event, cursor)
    changed ||= kept !== event
    return kept ? [kept] : []
  })
  return changed ? next : events
}

// Whether a snapshot taken at the stream cursor misses this event.
export function isAfterCursor(event: SessionEvent, cursor: number): boolean {
  return !event.seq || streamSeq(event) > cursor
}

function eventAfterCursor(event: LiveSessionEvent, cursor: number): LiveSessionEvent | undefined {
  if (!isAfterCursor(event, cursor)) return undefined
  const chunks = event.live_chunks
  if (!chunks) return event
  const keep = chunks.findIndex((chunk) => chunk.stream_seq > cursor)
  const start = chunks[keep].start
  if (start === 0) return event
  const text = liveText(event).slice(start)
  return {
    ...event,
    ...(event.type === 'acp_thought' ? { acp: { ...event.acp!, thought: text } } : { content: text }),
    live_chunks: chunks.slice(keep).map((chunk) => ({ stream_seq: chunk.stream_seq, start: chunk.start - start })),
  }
}

function appendLiveText(held: LiveSessionEvent | undefined, event: LiveSessionEvent): LiveSessionEvent {
  const start = held ? liveText(held).length : 0
  const chunks = event.live_chunks ?? [{ stream_seq: streamSeq(event), start: 0 }]
  return {
    ...(held ? appendProjection(held, event) : event),
    live_chunks: [
      ...(held?.live_chunks ?? []),
      ...chunks.map((chunk) => ({ stream_seq: chunk.stream_seq, start: chunk.start + start })),
    ],
  }
}

function liveText(event: SessionEvent): string {
  return (event.type === 'acp_thought' ? event.acp?.thought : event.content) ?? ''
}

export function coalesceSessionEvents(events: SessionEvent[]): SessionEvent[] {
  const ordered = events
    .map((event, index) => ({ event, index }))
    .sort((a, b) => compareEvents(a.event, b.event) || a.index - b.index)
  const projected: SessionEvent[] = []
  const seenSeq = new Set<string>()
  const byKey = new Map<string, number>()
  for (const { event } of ordered) {
    const seqKey = event.seq ? `${event.session_id}:${event.seq}` : ''
    if (seqKey && seenSeq.has(seqKey)) continue
    if (seqKey) seenSeq.add(seqKey)
    const key = sessionEventCoalesceKey(event)
    const index = key ? byKey.get(key) : undefined
    if (index === undefined) {
      if (key) byKey.set(key, projected.length)
      projected.push(event)
    } else {
      projected[index] = event.projection_op === 'append' ? appendProjection(projected[index], event) : event
    }
  }
  return projected.sort(compareEvents)
}

function appendProjection(prev: SessionEvent, event: SessionEvent): SessionEvent {
  return {
    ...prev,
    ...event,
    content:
      event.type === 'acp_message'
        ? `${prev.content ?? ''}${event.content ?? ''}`
        : (event.content ?? prev.content),
    acp: event.acp
      ? {
          ...prev.acp,
          ...event.acp,
          thought:
            event.type === 'acp_thought'
              ? `${prev.acp?.thought ?? ''}${event.acp.thought ?? ''}`
              : (event.acp.thought ?? prev.acp?.thought),
        }
      : event.acp,
  }
}

export function streamSeq(event: SessionEvent): number {
  return event.stream_seq ?? event.seq ?? 0
}

// Copies of one event order newest first, so deduplication keeps the latest update.
function compareEvents(a: SessionEvent, b: SessionEvent): number {
  const seqA = a.seq ?? 0
  const seqB = b.seq ?? 0
  if (seqA && seqB && a.session_id === b.session_id) return seqA - seqB || streamSeq(b) - streamSeq(a)
  const timeA = Date.parse(a.at)
  const timeB = Date.parse(b.at)
  return (Number.isNaN(timeA) ? 0 : timeA) - (Number.isNaN(timeB) ? 0 : timeB) || seqA - seqB
}
