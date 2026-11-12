import { useEffect, useRef } from 'react'
import { subscribeLiveData, type LiveDataTopic } from '../lib/liveData'

interface LiveRefreshOptions {
  topics?: readonly LiveDataTopic[]
  apiKey?: string
  minEventIntervalMs?: number
}
const DEFAULT_TOPICS: readonly LiveDataTopic[] = ['usage', 'accounts', 'keys', 'proxies', 'settings']

// WebSocket invalidations drive refresh. HTTP polling is only a disconnected
// fallback; overlapping events collapse into one trailing refresh.
export function useAutoRefresh(
  refresh: () => Promise<unknown>,
  intervalMs = 15_000,
  enabled = true,
  options: LiveRefreshOptions = {},
) {
  const refreshRef = useRef(refresh)
  refreshRef.current = refresh
  const optionsRef = useRef(options)
  optionsRef.current = options
  const runningRef = useRef(false)
  const topicKey = (options.topics ?? DEFAULT_TOPICS).join(',')

  useEffect(() => {
    if (!enabled) return
    let stopped = false
    let connected = false
    let pending = false
    let timer: number | undefined
    let eventTimer: number | undefined
    let lastRefresh = 0
    const available = () => !stopped && !document.hidden && navigator.onLine !== false
    const fallbackMs = Math.max(60_000, intervalMs)
    const schedule = () => {
      window.clearTimeout(timer)
      if (available() && !connected) timer = window.setTimeout(() => requestRefresh(), fallbackMs)
    }
    const run = async () => {
      window.clearTimeout(eventTimer)
      eventTimer = undefined
      if (!available()) return
      if (runningRef.current) {
        eventTimer = window.setTimeout(run, 250)
        return
      }
      pending = false
      runningRef.current = true
      lastRefresh = Date.now()
      try { await refreshRef.current() } catch { /* Retain the last successful snapshot. */ }
      finally {
        runningRef.current = false
        schedule()
        if (pending && !stopped) requestRefresh()
      }
    }
    const requestRefresh = () => {
      pending = true
      if (!available() || eventTimer !== undefined) return
      const minInterval = optionsRef.current.minEventIntervalMs ?? 1500
      eventTimer = window.setTimeout(() => {
        eventTimer = undefined
        void run()
      }, Math.max(0, lastRefresh + minInterval - Date.now()))
    }
    const unsubscribe = subscribeLiveData((topics) => {
      if (topics.some((topic) => topicKey.split(',').includes(topic))) requestRefresh()
    }, (ready) => {
      connected = ready
      schedule()
    }, options.apiKey)
    const resume = () => {
      window.clearTimeout(timer)
      if (available()) requestRefresh()
    }
    schedule()
    document.addEventListener('visibilitychange', resume)
    window.addEventListener('online', resume)
    window.addEventListener('offline', resume)
    window.addEventListener('focus', resume)
    return () => {
      stopped = true
      unsubscribe()
      window.clearTimeout(timer)
      window.clearTimeout(eventTimer)
      document.removeEventListener('visibilitychange', resume)
      window.removeEventListener('online', resume)
      window.removeEventListener('offline', resume)
      window.removeEventListener('focus', resume)
    }
  }, [enabled, intervalMs, options.apiKey, topicKey])
}
