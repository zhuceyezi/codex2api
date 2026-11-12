import { useEffect, useRef } from 'react'

// Keep polling callbacks current without restarting the timer on each render.
// Hidden/offline pages stop polling; returning to the page refreshes immediately.
export function useAutoRefresh(
  refresh: () => Promise<unknown>,
  intervalMs = 15_000,
  enabled = true,
) {
  const refreshRef = useRef(refresh)
  refreshRef.current = refresh
  const runningRef = useRef(false)

  useEffect(() => {
    if (!enabled) return
    let stopped = false
    let timer: number | undefined
    const available = () => !document.hidden && navigator.onLine !== false
    const schedule = () => {
      window.clearTimeout(timer)
      if (!stopped && available()) timer = window.setTimeout(tick, intervalMs)
    }
    const tick = async () => {
      if (stopped || !available()) return
      if (runningRef.current) {
        schedule()
        return
      }
      runningRef.current = true
      try {
        await refreshRef.current()
      } catch {
        // Retain the last successful snapshot and retry next time.
      } finally {
        runningRef.current = false
        schedule()
      }
    }
    const resume = () => {
      window.clearTimeout(timer)
      if (available()) void tick()
    }
    schedule()
    document.addEventListener('visibilitychange', resume)
    window.addEventListener('online', resume)
    window.addEventListener('offline', resume)
    window.addEventListener('focus', resume)
    return () => {
      stopped = true
      window.clearTimeout(timer)
      document.removeEventListener('visibilitychange', resume)
      window.removeEventListener('online', resume)
      window.removeEventListener('offline', resume)
      window.removeEventListener('focus', resume)
    }
  }, [enabled, intervalMs])
}
