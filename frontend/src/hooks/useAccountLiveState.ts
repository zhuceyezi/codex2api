import { useEffect, useMemo, useRef } from 'react'
import { api } from '../api'
import type { AccountLiveStateResponse } from '../types'
import { mergeAccountLiveState } from '../lib/accountLiveState'
import { useAutoRefresh } from './useAutoRefresh'

export { mergeAccountLiveState }

export function useAccountLiveState(
  ids: number[],
  apply: (response: AccountLiveStateResponse) => void,
  enabled = true,
) {
  const idsKey = useMemo(() => ids.join(','), [ids])
  const controllerRef = useRef<AbortController | null>(null)
  const refresh = async () => {
    if (!enabled || !idsKey || document.hidden) return
    controllerRef.current?.abort()
    const controller = new AbortController()
    controllerRef.current = controller
    const timeout = window.setTimeout(() => controller.abort(), 5000)
    try {
      const response = await api.getAccountLiveState(idsKey.split(',').map(Number), controller.signal)
      if (!controller.signal.aborted) apply(response)
    } catch { /* Keep the last known counters until the next update. */ }
    finally { window.clearTimeout(timeout) }
  }
  useEffect(() => {
    void refresh()
    return () => controllerRef.current?.abort()
  }, [enabled, idsKey, apply])
  useAutoRefresh(refresh, 60_000, enabled && Boolean(idsKey), { topics: ['runtime', 'accounts'], minEventIntervalMs: 1000 })
}
