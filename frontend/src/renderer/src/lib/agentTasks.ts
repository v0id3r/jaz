import type { AgentTask, SessionEvent } from '@/lib/api/types'

export interface AgentTaskView extends AgentTask {
  /** When the agent last reported the task. */
  at: string
}

// A background task is live while the agent holds it; connection_lost marks
// tasks whose agent process ended before reporting their end.
export function agentTaskLive(task: AgentTask): boolean {
  return (task.state === 'running' || task.state === 'paused') && !task.connection_lost
}

/** The session's background tasks from the overview's stored events and the
 * live stream, one row per task, live first and then latest report first. */
export function agentTasksFromSources(
  stored: SessionEvent[] | undefined,
  events: SessionEvent[],
): AgentTaskView[] {
  const byId = new Map<string, AgentTaskView>()
  for (const event of [...(stored ?? []), ...events]) {
    if (event.type !== 'agent_task' || !event.agent_task?.id) continue
    byId.set(event.agent_task.id, { ...event.agent_task, at: event.at })
  }
  return [...byId.values()].sort((a, b) => {
    const live = Number(agentTaskLive(b)) - Number(agentTaskLive(a))
    if (live) return live
    return Date.parse(b.at) - Date.parse(a.at)
  })
}
