import { useQuery } from '@tanstack/react-query'
import { Gauge } from 'lucide-react'
import { useState } from 'react'
import { SettingsCard } from '@/components/settings/SettingsCard'
import { Popover } from '@/components/ui/Popover'
import { AgentLogo } from '@/components/acp/AgentLogo'
import { agentLabel } from '@/lib/agentLabel'
import { subscriptionAgents, subscriptionUsageQuery } from '@/lib/api/subscriptions'
import type { SubscriptionAgent, SubscriptionUsage, SubscriptionWindow } from '@/lib/api/subscriptions'
import type { Session } from '@/lib/api/types'

export function SubscriptionUsageSettings() {
  return (
    <section aria-labelledby="subscription-limits">
      <h3 id="subscription-limits" className="mb-2 text-sm font-medium text-ink">Subscription limits</h3>
      <div className="grid gap-2 md:grid-cols-3">
        {subscriptionAgents.map((agent) => <SubscriptionUsageCard key={agent} agent={agent} />)}
      </div>
    </section>
  )
}

function SubscriptionUsageCard({ agent }: { agent: SubscriptionAgent }) {
  const usage = useQuery(subscriptionUsageQuery(agent))
  return (
    <SettingsCard className="min-w-0 space-y-3 p-3">
      <h4 className="flex items-center gap-2 text-xs font-medium text-ink">
        <AgentLogo agent={agent} size={15} /> {agentLabel(agent)}
      </h4>
      <SubscriptionWindows usage={usage.data} pending={usage.isPending} />
    </SettingsCard>
  )
}

export function ThreadSubscriptionUsage({ session }: { session: Session }) {
  const agent = subscriptionAgents.find((name) => name === session.runtime_ref?.agent)
  if (!agent) return null
  if (agent === 'codex' && session.model_provider && !['openai', 'codex'].includes(session.model_provider)) return null
  return <SubscriptionUsagePopover key={agent} agent={agent} />
}

function SubscriptionUsagePopover({ agent }: { agent: SubscriptionAgent }) {
  const [open, setOpen] = useState(false)
  const usage = useQuery(subscriptionUsageQuery(agent))
  if (usage.data?.status === 'not_connected') return null

  const label = `${agentLabel(agent)} subscription limits`
  return (
    <Popover open={open} onClose={() => setOpen(false)} placement="below" trigger={
      <button type="button" aria-label={label} title={label} aria-expanded={open}
        className="grid size-7 shrink-0 place-items-center rounded-lg text-ink-3 transition-transform hover:bg-surface hover:text-ink active:scale-[0.96]"
        onClick={() => {
          if (!open) void usage.refetch()
          setOpen(!open)
        }}>
        <Gauge size={14} />
      </button>
    }>
      <div className="w-60 space-y-3 px-2 py-1.5">
        <p className="text-xs font-medium text-ink">{label}</p>
        <SubscriptionWindows usage={usage.data} pending={usage.isPending} />
      </div>
    </Popover>
  )
}

function SubscriptionWindows({ usage, pending }: { usage?: SubscriptionUsage; pending: boolean }) {
  if (!usage?.windows?.length) {
    const label = pending ? 'Checking limits…' : usage?.status === 'not_connected' ? 'Subscription account not connected' : 'Limits unavailable'
    return <p className="text-xs text-ink-3" role="status">{label}</p>
  }

  return (
    <div className="space-y-2">
      {usage.windows.map((window) => <SubscriptionWindowRow key={window.id} window={window} />)}
      {usage.observed_at && (
        <p className="text-[11px] text-ink-3">
          {usage.status === 'stale' ? 'Last known · ' : 'Updated '}
          <time dateTime={usage.observed_at}>{localTime(usage.observed_at)}</time>
        </p>
      )}
    </div>
  )
}

function SubscriptionWindowRow({ window }: { window: SubscriptionWindow }) {
  const used = Math.max(0, Math.min(100, window.used_percent))
  const expired = window.resets_at !== undefined && Date.parse(window.resets_at) <= Date.now()
  return (
    <div className="space-y-1">
      <div className="flex items-baseline justify-between gap-2 text-xs">
        <span className="text-ink-2">{window.label}</span>
        <span className="shrink-0 tabular-nums text-ink">{Math.round(100 - used)}% left</span>
      </div>
      <div role="meter" aria-label={window.label} aria-valuemin={0} aria-valuemax={100} aria-valuenow={used}
        aria-valuetext={`${Math.round(used)}% used`} className="h-1 overflow-hidden rounded-full bg-surface-2">
        <div className={`h-full rounded-full ${used >= 90 ? 'bg-danger' : 'bg-primary'}`} style={{ width: `${used}%` }} />
      </div>
      {window.resets_at && (
        <p className="text-[11px] text-ink-3">
          {expired ? 'Scheduled reset passed · ' : 'Resets '}
          <time dateTime={window.resets_at}>{localTime(window.resets_at)}</time>
        </p>
      )}
    </div>
  )
}

function localTime(value: string) {
  return new Date(value).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' })
}
