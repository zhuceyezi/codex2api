import { api, getAdminKey } from '../api'

export type LiveDataTopic = 'usage' | 'accounts' | 'keys' | 'proxies' | 'settings' | 'runtime' | 'requests'
type EventListener = (topics: LiveDataTopic[], immediate?: boolean) => void
type StatusListener = (connected: boolean) => void
interface Listener { event: EventListener; status: StatusListener }

// One authenticated socket per session in this tab, shared by page and widgets.
class LiveDataConnection {
  listeners = new Set<Listener>()
  connected = false
  private socket: WebSocket | null = null
  private controller: AbortController | null = null
  private retry: number | undefined
  private watchdog: number | undefined
  private disposal: number | undefined
  private attempts = 0
  private stopped = false
  constructor(private publicKey: string | undefined, private onDispose: () => void) {
    document.addEventListener('visibilitychange', this.resume)
    window.addEventListener('online', this.resume)
    window.addEventListener('offline', this.resume)
    window.addEventListener('focus', this.resume)
    void this.connect()
  }
  add(listener: Listener) {
    window.clearTimeout(this.disposal)
    this.listeners.add(listener)
    listener.status(this.connected)
    return () => {
      this.listeners.delete(listener)
      // Allow React effect replacements to reuse the existing connection.
      if (this.listeners.size === 0) this.disposal = window.setTimeout(() => this.dispose(), 100)
    }
  }
  private status(connected: boolean) {
    if (this.connected === connected) return
    this.connected = connected
    for (const listener of this.listeners) listener.status(connected)
  }
  private available() { return !this.stopped && !document.hidden && navigator.onLine !== false }
  private resume = () => {
    if (!this.available()) {
      this.controller?.abort()
      this.socket?.close()
      this.status(false)
      return
    }
    if (!this.socket && !this.controller) {
      window.clearTimeout(this.retry)
      void this.connect()
    }
  }
  private async connect() {
    if (!this.available() || this.socket || this.controller) return
    const controller = new AbortController()
    this.controller = controller
    const timeout = window.setTimeout(() => controller.abort(), 8000)
    try {
      const { ticket } = this.publicKey
        ? await api.getPublicLiveDataTicket(this.publicKey, controller.signal)
        : await api.getLiveDataTicket(controller.signal)
      if (!this.available() || controller.signal.aborted) return
      const url = new URL('/api/live-data', window.location.href)
      url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'
      url.searchParams.set('ticket', ticket)
      const socket = new WebSocket(url)
      this.socket = socket
      const armWatchdog = () => {
        window.clearTimeout(this.watchdog)
        this.watchdog = window.setTimeout(() => socket.close(), 40_000)
      }
      armWatchdog()
      socket.onmessage = (event) => {
        try {
          const message = JSON.parse(event.data)
          if (!['ready', 'invalidate'].includes(message.type) || !Array.isArray(message.topics)) return
          armWatchdog()
          this.attempts = 0
          this.status(true)
          for (const listener of this.listeners) listener.event(message.topics, message.immediate === true)
        } catch { /* Ignore malformed frames; watchdog reconnects a stale socket. */ }
      }
      socket.onclose = () => {
        if (this.socket !== socket) return
        this.socket = null
        window.clearTimeout(this.watchdog)
        this.status(false)
        this.reconnect()
      }
      socket.onerror = () => socket.close()
    } catch {
      this.status(false)
    } finally {
      window.clearTimeout(timeout)
      if (this.controller === controller) this.controller = null
      if (this.available() && !this.socket) this.reconnect()
    }
  }
  private reconnect() {
    window.clearTimeout(this.retry)
    if (!this.available()) return
    const delay = Math.min(30_000, 1000 * 2 ** Math.min(this.attempts++, 5))
    this.retry = window.setTimeout(() => void this.connect(), delay + Math.random() * 500)
  }
  private dispose() {
    this.stopped = true
    window.clearTimeout(this.retry)
    window.clearTimeout(this.watchdog)
    this.controller?.abort()
    this.socket?.close()
    document.removeEventListener('visibilitychange', this.resume)
    window.removeEventListener('online', this.resume)
    window.removeEventListener('offline', this.resume)
    window.removeEventListener('focus', this.resume)
    this.onDispose()
  }
}
const connections = new Map<string, LiveDataConnection>()
export function subscribeLiveData(event: EventListener, status: StatusListener, apiKey?: string) {
  const session = apiKey === undefined ? `admin:${getAdminKey()}` : `key:${apiKey}`
  let connection = connections.get(session)
  if (!connection) {
    connection = new LiveDataConnection(apiKey, () => connections.delete(session))
    connections.set(session, connection)
  }
  return connection.add({ event, status })
}
