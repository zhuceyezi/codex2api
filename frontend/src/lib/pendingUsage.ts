import type { UsageLog } from '../types'

// Pending snapshots bridge the interval between completion and batched commit.
// A committed row always wins; each non-retry request keeps the same React key.
export function mergePendingUsage(logs: UsageLog[], pending: UsageLog[]): UsageLog[] {
  const committed = new Set(logs.filter((log) => !log.is_retry_attempt).map((log) => log.request_id).filter(Boolean))
  const active = pending.filter((log) => log.request_state === 'running' || !committed.has(log.request_id))
  const activeIDs = new Set(active.map((log) => log.request_id))
  return [...active, ...logs.filter((log) => log.is_retry_attempt || !activeIDs.has(log.request_id))]
    .sort((a, b) => Date.parse(b.created_at) - Date.parse(a.created_at) || b.id - a.id)
}
export function usageRowKeys(logs: UsageLog[]): Map<number, string> {
  const seen = new Set<string>()
  return new Map(logs.map((log) => {
    const request = log.request_id
    if (request && !log.is_retry_attempt && !seen.has(request)) {
      seen.add(request)
      return [log.id, `request:${request}`]
    }
    return [log.id, `log:${log.id}`]
  }))
}
