import { Square } from 'lucide-react'
import { createContext, useContext, useState } from 'react'
import { useToast } from '@/components/ui/toast'
import type { ACPToolCall } from '@/lib/api/types'
import { isRunningToolStatus } from './toolPresentation'

/** Cancels one running tool call without cancelling its turn; provided by the
 * thread view, absent where tool calls render outside a session. */
export const ToolCancelContext = createContext<((toolCallId: string) => Promise<void>) | undefined>(undefined)

/** The stop control of a running tool call the agent offered to cancel. */
export function ToolCancelButton({ call, active }: { call: ACPToolCall; active: boolean }) {
  const cancel = useContext(ToolCancelContext)
  const toast = useToast()
  const [pending, setPending] = useState(false)
  if (!cancel || !call.cancellable || !active || !isRunningToolStatus(call.status)) return null
  return (
    <button
      type="button"
      aria-label="Stop this tool call"
      title="Stop this tool call (the turn keeps running)"
      disabled={pending}
      onClick={(event) => {
        event.stopPropagation()
        setPending(true)
        cancel(call.id)
          .catch(() => toast('Could not stop the tool call', 'danger'))
          .finally(() => setPending(false))
      }}
      className="grid size-5 shrink-0 cursor-pointer place-items-center rounded text-ink-3 transition-colors duration-150 hover:bg-surface-2 hover:text-ink disabled:opacity-50"
    >
      <Square size={10} aria-hidden />
    </button>
  )
}
