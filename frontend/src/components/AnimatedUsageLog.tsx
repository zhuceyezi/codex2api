import { useEffect, useRef, useState, type ReactNode } from 'react'
import type { UsageLog } from '../types'

const numericFields = [
  'input_tokens', 'output_tokens', 'completion_tokens', 'total_tokens', 'reasoning_tokens',
  'cached_tokens', 'cache_write_5m_tokens', 'cache_write_1h_tokens', 'image_input_tokens',
  'image_output_tokens', 'cached_image_input_tokens', 'account_billed', 'user_billed',
  'total_cost', 'input_cost', 'output_cost', 'cache_read_cost', 'cache_write_5m_cost',
  'cache_write_1h_cost', 'tokens_per_second',
] as const

// Animate observed samples, with continuous time and stall-aware speed. No
// extrapolated token counts or charges are added between server observations.
export default function AnimatedUsageLog({ log, children }: {
  log: UsageLog
  children: (display: UsageLog) => ReactNode
}) {
  const [display, setDisplay] = useState(log)
  const current = useRef(log)
  const wasRunning = useRef(log.request_state === 'running')
  useEffect(() => {
    const reduce = window.matchMedia('(prefers-reduced-motion: reduce)').matches
    const running = log.request_state === 'running'
    const animate = !reduce && (running || wasRunning.current)
    wasRunning.current = running
    if (!animate) {
      current.current = log; setDisplay(log)
      if (!running) return
      const timer = window.setInterval(() => setDisplay({ ...log, duration_ms: Math.max(0, Date.now() - Date.parse(log.created_at)) }), 1000)
      return () => window.clearInterval(timer)
    }
    const from = current.current
    const start = performance.now()
    const duration = running ? 1100 : 350
    let frame = 0
    let previousPaint = 0
    const paint = (now: number) => {
      if (now - previousPaint >= 30 || now - start >= duration && !running) {
        previousPaint = now
        const fraction = Math.min(1, (now - start) / duration)
        // Linear movement keeps the meter turning when successive samples arrive.
        const next = { ...log }
        for (const key of numericFields) {
          const value = Number(log[key]) || 0
          ;(next as unknown as Record<string, unknown>)[key] = (Number(from[key]) || 0) + (value - (Number(from[key]) || 0)) * fraction
        }
        if (running) {
          next.duration_ms = Math.max(0, Date.now() - Date.parse(log.created_at))
          const age = log.progress_updated_at ? Date.now() - Date.parse(log.progress_updated_at) : 0
          if (age > 2500) next.tokens_per_second = (next.tokens_per_second || 0) * Math.max(0, 1 - (age - 2500) / 1500)
        }
        current.current = next
        setDisplay(next)
      }
      if (running || now - start < duration) frame = requestAnimationFrame(paint)
    }
    frame = requestAnimationFrame(paint)
    return () => cancelAnimationFrame(frame)
  }, [log])
  // Metadata switches immediately. Only numeric values interpolate.
  return children({ ...log, ...Object.fromEntries(numericFields.map((key) => [key, display[key]])), duration_ms: display.duration_ms })
}
