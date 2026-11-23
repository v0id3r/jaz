import { queryOptions } from '@tanstack/react-query'
import { get } from '@/lib/api/client'

export const subscriptionAgents = ['codex', 'claude', 'kimi'] as const
export type SubscriptionAgent = typeof subscriptionAgents[number]

export interface SubscriptionWindow {
  id: string
  label: string
  used_percent: number
  resets_at?: string
}

export interface SubscriptionUsage {
  status: 'ready' | 'stale' | 'not_connected' | 'unavailable'
  observed_at?: string
  windows?: SubscriptionWindow[]
}

export const subscriptionUsageQuery = (agent: SubscriptionAgent) => queryOptions({
  queryKey: ['subscription-usage', agent],
  queryFn: ({ signal }) => get<SubscriptionUsage>(`/v1/usage/subscriptions/${agent}`, { signal }),
  staleTime: 30_000,
  refetchInterval: 60_000,
})
