import { useEffect, useRef, useState } from 'react'
import { useAutoRefresh } from '../hooks/useAutoRefresh'
import { AdminAPIError, api } from '../api'
import APIKeyConcurrencyBadge from './APIKeyConcurrencyBadge'

const REFRESH_TIMEOUT_MS = 4_000
const MAX_CONSECUTIVE_FAILURES = 3

export default function PublicAPIKeyConcurrencyBadge({ apiKey, limit }: { apiKey: string; limit?: number }) {
  const [current, setCurrent] = useState<number>()
  const refreshRef = useRef<() => Promise<void>>(async () => {})
  useEffect(() => {
    let cancelled = false
    let pending = false
    let failures = 0
    setCurrent(undefined)
    const refresh = async () => {
      if (pending || document.hidden) return
      pending = true
      const controller = new AbortController()
      const timeout = window.setTimeout(() => controller.abort(), REFRESH_TIMEOUT_MS)
      try {
        const response = await api.getPublicAPIKeyConcurrency(apiKey, { signal: controller.signal })
        failures = 0
        if (!cancelled) setCurrent(response.current)
      } catch (err) {
        // 密钥失效或功能关闭时立即清空快照；其他暂时性失败保留上次快照，连续失败多次后也清空，避免展示过期数据。
        failures += 1
        const unavailable = err instanceof AdminAPIError && (err.status === 401 || err.status === 404)
        if (!cancelled && (unavailable || failures >= MAX_CONSECUTIVE_FAILURES)) {
          setCurrent(undefined)
        }
      } finally {
        window.clearTimeout(timeout)
        pending = false
      }
    }
    refreshRef.current = refresh
    void refresh()
    document.addEventListener('visibilitychange', refresh)
    return () => {
      cancelled = true
      document.removeEventListener('visibilitychange', refresh)
    }
  }, [apiKey])
  useAutoRefresh(() => refreshRef.current(), 60_000, Boolean(apiKey), { apiKey, topics: ['runtime', 'keys'], minEventIntervalMs: 1000 })
  return <APIKeyConcurrencyBadge current={current} limit={limit} />
}
