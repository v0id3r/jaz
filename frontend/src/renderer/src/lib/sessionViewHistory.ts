import type { SessionViewHistory } from '@/lib/sessionFocus'

const STORAGE_KEY = 'jaz.sidebar.sessionViews'
const HISTORY_LIMIT = 100

export function readSessionViewHistory(): SessionViewHistory {
  try {
    const value: unknown = JSON.parse(localStorage.getItem(STORAGE_KEY) ?? '{}')
    if (!value || typeof value !== 'object' || Array.isArray(value)) return {}
    return Object.fromEntries(
      Object.entries(value).filter(([, view]) =>
        view && typeof view === 'object' &&
        'at' in view && typeof view.at === 'number' && Number.isFinite(view.at) && view.at >= 0 &&
        'count' in view && typeof view.count === 'number' && Number.isFinite(view.count) && view.count > 0,
      ),
    )
  } catch {
    return {}
  }
}

export function recordSessionView(id: string, now = Date.now()): SessionViewHistory {
  const history = readSessionViewHistory()
  const previous = history[id]
  const next = {
    ...history,
    [id]: { at: now, count: (previous?.count ?? 0) + 1 },
  }
  const trimmed = Object.fromEntries(
    Object.entries(next)
      .sort(([, a], [, b]) => b.at - a.at)
      .slice(0, HISTORY_LIMIT),
  )
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(trimmed))
  } catch {
    // The in-memory history still works when storage is unavailable.
  }
  return trimmed
}
