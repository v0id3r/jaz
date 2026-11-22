import type { Session } from '@/lib/api/types'

type SessionListItem = { session: Session; child: boolean }

export type SessionView = { at: number; count: number }
export type SessionViewHistory = Record<string, SessionView>

export type SessionFocusOptions = {
  now: number
  currentSessionID?: string
  views?: SessionViewHistory
}

export const SESSION_FOCUS_THRESHOLD = 0.12
export const COLLAPSED_RECENT_LIMIT = 3

const HALF_LIFE_MS = 24 * 60 * 60 * 1000
const ACTIVITY_WEIGHT = 0.45
const VIEW_WEIGHT = 0.4
const FREQUENCY_WEIGHT = 0.15

function timestamp(value: string): number {
  const ms = Date.parse(value)
  return Number.isNaN(ms) ? 0 : ms
}

function decay(at: number | undefined, now: number): number {
  if (!at) return 0
  return 0.5 ** (Math.max(0, now - at) / HALF_LIFE_MS)
}

function focusRank(item: SessionListItem, currentSessionID?: string): number {
  const { session } = item
  if (session.status === 'error') return 0
  if (session.unread && session.status !== 'running') return 1
  if (session.status === 'running') return 2
  if (session.id === currentSessionID) return 3
  return 4
}

export function sessionFocusScore(item: SessionListItem, options: SessionFocusOptions): number {
  const { session } = item
  const activity = timestamp(session.last_attention_at || session.updated_at)
  const view = options.views?.[session.id]
  const viewed = decay(view?.at, options.now)
  const frequency = view ? 1 - Math.exp(-view.count / 3) : 0
  return (
    ACTIVITY_WEIGHT * decay(activity, options.now) +
    VIEW_WEIGHT * viewed +
    FREQUENCY_WEIGHT * frequency * viewed
  )
}

export function compareSessionFocus(
  a: SessionListItem,
  b: SessionListItem,
  options: SessionFocusOptions,
): number {
  return (
    focusRank(a, options.currentSessionID) - focusRank(b, options.currentSessionID) ||
    sessionFocusScore(b, options) - sessionFocusScore(a, options) ||
    timestamp(b.session.last_attention_at || b.session.updated_at) -
      timestamp(a.session.last_attention_at || a.session.updated_at) ||
    a.session.id.localeCompare(b.session.id)
  )
}

function bestItem(items: SessionListItem[], options: SessionFocusOptions): SessionListItem {
  return items.reduce((best, item) => compareSessionFocus(item, best, options) < 0 ? item : best)
}

function focusUnits(items: SessionListItem[]): SessionListItem[][] {
  const units: SessionListItem[][] = []
  for (const item of items) {
    if (item.child && units.length) units[units.length - 1].push(item)
    else units.push([item])
  }
  return units
}

export function sortSessionItems(
  items: SessionListItem[],
  options: SessionFocusOptions,
): SessionListItem[] {
  return focusUnits(items)
    .sort((a, b) => compareSessionFocus(bestItem(a, options), bestItem(b, options), options))
    .flat()
}

export function sessionFocusItems(
  items: SessionListItem[],
  options: SessionFocusOptions,
  recentLimit = COLLAPSED_RECENT_LIMIT,
): SessionListItem[] {
  const units = focusUnits(sortSessionItems(items, options))
  const forced = new Set<number>()
  units.forEach((unit, index) => {
    if (unit.some((item) => focusRank(item, options.currentSessionID) < 4)) forced.add(index)
  })
  const recent = units
    .map((unit, index) => ({ index, score: sessionFocusScore(bestItem(unit, options), options) }))
    .filter(({ index, score }) => !forced.has(index) && score >= SESSION_FOCUS_THRESHOLD)
    .slice(0, recentLimit)
  const visible = new Set([...forced, ...recent.map(({ index }) => index)])
  return units.filter((_, index) => visible.has(index)).flat()
}

export function bestSessionItem(
  items: SessionListItem[],
  options: SessionFocusOptions,
): SessionListItem | undefined {
  return items.length ? bestItem(items, options) : undefined
}
