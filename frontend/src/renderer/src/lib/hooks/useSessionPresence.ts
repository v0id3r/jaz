import { useEffect } from 'react'
import { put } from '@/lib/api/client'

export function useSessionPresence(sessionId: string | null): void {
  useEffect(() => sessionId ? startSessionPresence(sessionId) : undefined, [sessionId])
}

export function startSessionPresence(sessionId: string): () => void {
  let active = true
  let queue = Promise.resolve()
  const report = (activeSessionId: string) => {
    queue = queue.then(async () => {
      if (active || !activeSessionId) {
        await put('/v1/mobile/presence', { session_id: activeSessionId })
      }
    }).catch(() => {})
  }
  const update = () => report(document.visibilityState === 'visible' && document.hasFocus() ? sessionId : '')
  const clear = () => report('')
  update()
  const heartbeat = window.setInterval(() => {
    if (document.visibilityState === 'visible' && document.hasFocus()) report(sessionId)
  }, 20_000)
  document.addEventListener('visibilitychange', update)
  window.addEventListener('focus', update)
  window.addEventListener('blur', update)
  window.addEventListener('pagehide', clear)
  return () => {
    active = false
    window.clearInterval(heartbeat)
    document.removeEventListener('visibilitychange', update)
    window.removeEventListener('focus', update)
    window.removeEventListener('blur', update)
    window.removeEventListener('pagehide', clear)
    clear()
  }
}
