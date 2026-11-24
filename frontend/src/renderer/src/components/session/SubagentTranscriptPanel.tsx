import { Bot, Wrench } from 'lucide-react'
import { useEffect, useMemo, useRef } from 'react'
import { MessageMarkdown } from '@/components/session/MessageMarkdown'
import type { SessionEvent } from '@/lib/api/types'
import { subagentTranscriptEntries } from '@/lib/providerSubagents'

// The read-only transcript of one provider subagent, from the session
// journal's subagent_message events; it keeps updating while the subagent
// works.
export function SubagentTranscriptPanel({ subagent, title, events, visible }: {
  subagent: string
  title?: string
  events: SessionEvent[]
  visible: boolean
}) {
  const entries = useMemo(() => subagentTranscriptEntries(events, subagent), [events, subagent])
  const scrollRef = useRef<HTMLDivElement>(null)
  const latest = entries.length ? entries[entries.length - 1] : undefined
  useEffect(() => {
    const scroll = scrollRef.current
    if (visible && scroll) scroll.scrollTop = scroll.scrollHeight
  }, [visible, entries.length, latest?.content.length])
  return (
    <div className="flex h-full min-h-0 flex-col" data-thread-find-shortcuts="off">
      <div className="flex h-10 shrink-0 items-center gap-2 border-b border-border/60 px-3">
        <Bot size={14} className="shrink-0 text-ink-3" aria-hidden />
        <span className="truncate text-xs font-medium text-ink">{title?.trim() || 'Subagent'}</span>
        <span className="shrink-0 text-[11px] text-ink-3">read-only</span>
      </div>
      <div ref={scrollRef} className="min-h-0 flex-1 overflow-y-auto px-3 py-2">
        {!entries.length ? (
          <p className="px-1 py-2 text-xs text-ink-3">Nothing in the transcript yet.</p>
        ) : (
          <ol className="flex flex-col gap-2">
            {entries.map((entry, index) => (
              <li key={index} className="min-w-0">
                {entry.role === 'tool' ? (
                  <p className="flex items-center gap-1.5 px-1 text-[11px] text-ink-3">
                    <Wrench size={11} className="shrink-0" aria-hidden />
                    <span className="truncate">{entry.content || 'Tool call'}</span>
                    {entry.status ? <span className="shrink-0">· {entry.status}</span> : null}
                  </p>
                ) : entry.role === 'thought' ? (
                  <p className="whitespace-pre-wrap px-1 text-[11px] leading-snug text-ink-3 italic">{entry.content}</p>
                ) : entry.role === 'user' ? (
                  <div className="rounded-lg bg-surface-2 px-2.5 py-1.5">
                    <p className="whitespace-pre-wrap text-xs leading-snug text-ink">{entry.content}</p>
                  </div>
                ) : (
                  <MessageMarkdown text={entry.content} />
                )}
              </li>
            ))}
          </ol>
        )}
      </div>
    </div>
  )
}
