import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Check, CircleAlert, CircleHelp, LoaderCircle, Square } from 'lucide-react'
import { useEffect, useRef, useState } from 'react'
import { useToast } from '@/components/ui/toast'
import { stopSessionAgentTask } from '@/lib/api/sessions'
import type { Session } from '@/lib/api/types'
import { agentTaskLive, tasksSectionOpen, type AgentTaskView } from '@/lib/agentTasks'
import { relativeTime } from '@/lib/format/time'
import { keys } from '@/lib/query/keys'
import { OverviewDisclosureSection } from './OverviewRuns'

function taskLabel(task: AgentTaskView): string {
  return task.name || task.description || task.type || task.id
}

function TaskStatus({ task }: { task: AgentTaskView }) {
  if (agentTaskLive(task)) {
    const paused = task.state === 'paused'
    return (
      <span className="inline-flex size-5 shrink-0 items-center justify-center text-running" title={paused ? 'Paused' : 'Running'}>
        <LoaderCircle size={14} className={paused ? '' : 'animate-spin'} aria-hidden />
      </span>
    )
  }
  // A task whose agent connection died has no known outcome; only a reported
  // completion earns the green check.
  if (task.connection_lost) {
    return (
      <span className="inline-flex size-5 shrink-0 items-center justify-center text-ink-3" title="Connection lost — outcome unknown">
        <CircleHelp size={14} aria-hidden />
      </span>
    )
  }
  if (task.state === 'failed') {
    return (
      <span className="inline-flex size-5 shrink-0 items-center justify-center text-danger" title="Failed">
        <CircleAlert size={14} aria-hidden />
      </span>
    )
  }
  if (task.state === 'stopped' || task.state === 'cancelled' || task.state === 'canceled') {
    return (
      <span className="inline-flex size-5 shrink-0 items-center justify-center text-ink-3" title="Stopped">
        <Square size={11} aria-hidden />
      </span>
    )
  }
  const completed = task.state === 'completed'
  const Icon = completed ? Check : CircleHelp
  return (
    <span className={`inline-flex size-5 shrink-0 items-center justify-center ${completed ? 'text-ok' : 'text-ink-3'}`} title={completed ? 'Finished' : task.state || 'Ended'}>
      <Icon size={14} aria-hidden />
    </span>
  )
}

function TaskRow({ session, task }: { session: Session; task: AgentTaskView }) {
  const queryClient = useQueryClient()
  const toast = useToast()
  const stop = useMutation({
    mutationFn: () => stopSessionAgentTask(session.id, task.id),
    onSettled: () => queryClient.invalidateQueries({ queryKey: keys.sessionOverview(session.id) }),
    onError: () => toast('Could not stop the task', 'danger'),
  })
  const detail = [task.type, task.description].filter(Boolean).join(' · ')
  return (
    <li className="flex h-8 min-w-0 items-center gap-2 rounded-md px-2 transition-colors duration-150 hover:bg-surface-2">
      <TaskStatus task={task} />
      <span
        className={`min-w-0 flex-1 truncate text-[13px] ${agentTaskLive(task) ? 'text-ink' : 'text-ink-3'}`}
        title={detail ? `${taskLabel(task)}\n${detail}` : taskLabel(task)}
      >
        {taskLabel(task)}
      </span>
      <span className="shrink-0 text-[11px] tabular-nums text-ink-3">{relativeTime(task.at)}</span>
      {agentTaskLive(task) && task.can_stop ? (
        <button
          type="button"
          aria-label={`Stop ${taskLabel(task)}`}
          title="Stop task"
          disabled={stop.isPending}
          onClick={() => stop.mutate()}
          className="grid size-6 shrink-0 cursor-pointer place-items-center rounded text-ink-3 transition-colors duration-150 hover:bg-surface hover:text-ink disabled:opacity-50"
        >
          <Square size={11} aria-hidden />
        </button>
      ) : null}
    </li>
  )
}

/** The agent's background tasks: opens when a task goes live, finished ones folded away. */
export function BackgroundTasksSection({ session, tasks }: { session: Session; tasks: AgentTaskView[] }) {
  const live = tasks.filter(agentTaskLive).length
  const [open, setOpen] = useState(live > 0)
  const wasLive = useRef(live > 0)
  useEffect(() => {
    setOpen((current) => tasksSectionOpen(current, wasLive.current, live > 0))
    wasLive.current = live > 0
  }, [live])
  return (
    <OverviewDisclosureSection
      title="Background tasks"
      meta={live || tasks.length}
      open={open}
      onToggle={() => setOpen((value) => !value)}
    >
      <ul className="mt-2 flex flex-col">
        {tasks.map((task) => <TaskRow key={task.id} session={session} task={task} />)}
      </ul>
    </OverviewDisclosureSection>
  )
}
