import { del, get, put } from '@/lib/api/client'
import { isIOS } from '@/lib/ios'

export type WebPushState = { available: boolean; subscribed: boolean }

export async function webPushState(): Promise<WebPushState> {
  const installedIOS = !isIOS() || window.matchMedia('(display-mode: standalone)').matches
  const available = 'serviceWorker' in navigator && 'PushManager' in window && 'Notification' in window && installedIOS
  if (!available) return { available, subscribed: false }
  const registration = await navigator.serviceWorker.getRegistration('/sw.js')
  const subscription = await registration?.pushManager.getSubscription()
  return { available, subscribed: Boolean(subscription) }
}

export async function subscribeWebPush(): Promise<void> {
  if (await Notification.requestPermission() !== 'granted') {
    throw new Error('Allow notifications to enable Web Push')
  }
  const registration = await navigator.serviceWorker.register('/sw.js')
  await navigator.serviceWorker.ready
  const status = await get<{ web_push_public_key?: string }>('/v1/mobile/push')
  const publicKey = status.web_push_public_key
  if (!publicKey) {
    throw new Error('Web Push is not configured on this server')
  }
  const subscription = await registration.pushManager.getSubscription() ?? await registration.pushManager.subscribe({
    userVisibleOnly: true,
    applicationServerKey: decodeBase64URL(publicKey),
  })
  try {
    await put<never>('/v1/mobile/web-push', { ...subscription.toJSON(), declarative: isIOS() })
  } catch (error) {
    await subscription.unsubscribe()
    throw error
  }
}

export async function unsubscribeWebPush(): Promise<void> {
  const registration = await navigator.serviceWorker.getRegistration('/sw.js')
  const subscription = await registration?.pushManager.getSubscription()
  if (!subscription) return
  try {
    await del('/v1/mobile/web-push')
  } finally {
    await subscription.unsubscribe()
  }
}

function decodeBase64URL(value: string): ArrayBuffer {
  const normalized = value.replaceAll('-', '+').replaceAll('_', '/')
  const bytes = atob(normalized.padEnd(Math.ceil(normalized.length / 4) * 4, '='))
  const output = new ArrayBuffer(bytes.length)
  new Uint8Array(output).set(Array.from(bytes, (char) => char.charCodeAt(0)))
  return output
}
