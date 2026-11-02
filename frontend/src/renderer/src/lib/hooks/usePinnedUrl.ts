import { useState } from 'react'
import { embeddedUrlKey, withCurrentTicket } from '@/lib/api/client'

// Embedded API URLs carry a resource ticket that rotates while the resource
// stays the same. Keep the URL an element first loaded, so rerenders do not
// refetch it; retry() swaps in the current ticket once after a failed load and
// reports whether there was anything new to try.
export function usePinnedUrl(url: string): [string, () => boolean] {
  const key = embeddedUrlKey(url)
  const [pinned, setPinned] = useState({ key, url, retried: false })
  let current = pinned
  if (pinned.key !== key) {
    current = { key, url, retried: false }
    setPinned(current)
  }
  const retry = () => {
    const next = withCurrentTicket(current.url)
    if (current.retried || next === current.url) return false
    setPinned({ key, url: next, retried: true })
    return true
  }
  return [current.url, retry]
}
