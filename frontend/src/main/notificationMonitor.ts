import {
  diffThreadCompletions,
  parseThreadCompletions,
  parseThreadNotificationConfig,
  type ThreadCompletion,
  type ThreadNotificationConfig,
} from '../shared/notifications'
import { deviceAuthHeaders } from '../shared/deviceProof'

const POLL_INTERVAL_MS = 15_000
const REQUEST_TIMEOUT_MS = 10_000

type EnabledConfig = Extract<ThreadNotificationConfig, { enabled: true }>

export function createThreadCompletionMonitor(
  onCompletion: (completion: ThreadCompletion) => void,
  fetcher: typeof fetch,
  proof: (method: string, url: string, token: string) => Promise<string>,
  // Resolves the token to present; a session whose device key is the
  // credential has none in its config and gets access tokens instead.
  credential: (baseUrl: string, token: string, rejected?: string) => Promise<string> = async (_baseUrl, token) => token,
) {
  let config: EnabledConfig | null = null
  let history: Map<string, number> | null = null
  let polling = false
  let timer: NodeJS.Timeout | null = null
  let request: AbortController | null = null
  let rejected: string | undefined

  async function poll(): Promise<void> {
    if (polling || !config) return
    const requestConfig = config
    polling = true
    const controller = new AbortController()
    request = controller
    try {
      const url = `${requestConfig.baseUrl}/v1/feed/completions`
      const token = await credential(requestConfig.baseUrl, requestConfig.token, rejected)
      const response = await fetcher(url, {
        headers: token ? deviceAuthHeaders(token, await proof('GET', url, token)) : undefined,
        signal: AbortSignal.any([controller.signal, AbortSignal.timeout(REQUEST_TIMEOUT_MS)]),
      })
      rejected = response.status === 401 ? token : undefined
      if (!response.ok) return
      const items = parseThreadCompletions(await response.json())
      if (!items || config !== requestConfig) return
      const next = diffThreadCompletions(history, items)
      history = next.history
      for (const item of next.added) onCompletion(item)
    } catch {
      return
    } finally {
      polling = false
      if (request === controller) request = null
      if (config && config !== requestConfig) void poll()
    }
  }

  function stop(): void {
    config = null
    request?.abort()
    request = null
    history = null
    if (timer) clearInterval(timer)
    timer = null
  }

  return {
    async configure(value: unknown): Promise<boolean> {
      const next = parseThreadNotificationConfig(value)
      if (!next) {
        stop()
        return false
      }
      if (!next.enabled) {
        stop()
        return true
      }
      if (config && config.baseUrl === next.baseUrl && config.token === next.token) return true
      const backendChanged = !config || config.baseUrl !== next.baseUrl
      config = next
      if (backendChanged) history = null
      if (!timer) {
        timer = setInterval(() => void poll(), POLL_INTERVAL_MS)
        timer.unref()
      }
      await poll()
      return true
    },
    poll,
    stop,
  }
}
