import { Bot } from 'lucide-react'
import { createContext, useContext } from 'react'
import type { ACPToolCall } from '@/lib/api/types'

export interface SubagentTranscriptAccess {
  open: (subagent: string, title?: string) => void
  /** Subagents whose transcript the session journal holds. */
  transcripts: ReadonlySet<string>
}

/** Opens a subagent's transcript; provided by the thread view, absent where
 * tool calls render outside a session. */
export const SubagentTranscriptContext = createContext<SubagentTranscriptAccess | undefined>(undefined)

/** The card of a tool call the agent linked to a subagent opens that
 * subagent's transcript, when the session holds one. */
export function SubagentTranscriptButton({ call }: { call: ACPToolCall }) {
  const access = useContext(SubagentTranscriptContext)
  if (!access || !call.subagent_id || !access.transcripts.has(call.subagent_id)) return null
  return (
    <button
      type="button"
      aria-label="Open the subagent's transcript"
      title="Open the subagent's transcript"
      onClick={(event) => {
        event.stopPropagation()
        access.open(call.subagent_id!)
      }}
      className="grid size-5 shrink-0 cursor-pointer place-items-center rounded text-ink-3 transition-colors duration-150 hover:bg-surface-2 hover:text-ink"
    >
      <Bot size={12} aria-hidden />
    </button>
  )
}
