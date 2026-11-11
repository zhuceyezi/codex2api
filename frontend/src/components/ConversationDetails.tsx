import { useEffect, useState } from 'react'
import { api } from '../api'
import Modal from './Modal'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { MessageSquareText, Settings2 } from 'lucide-react'

type Conversation = { request_id: string; endpoint: string; request_body: string; response_body: string; content_type: string; truncated: boolean; created_at: number }
type ObjectValue = Record<string, unknown>
function parse(text: string): unknown { try { return JSON.parse(text) } catch { return text } }
function object(value: unknown): ObjectValue | undefined { return value && typeof value === 'object' && !Array.isArray(value) ? value as ObjectValue : undefined }
function pretty(value: unknown): string { return typeof value === 'string' ? value : JSON.stringify(value, null, 2) ?? '' }
function text(value: unknown): string {
  if (typeof value === 'string') return value
  if (Array.isArray(value)) return value.map(text).filter(Boolean).join('\n')
  const item = object(value)
  if (item) {
    if (typeof item.text === 'string') return item.text
    if (item.content !== undefined) return text(item.content)
    if (typeof item.type === 'string' && /image|audio|video|file/.test(item.type)) return `[${item.type}]`
  }
  return pretty(value)
}
function requestText(body: string): string {
  const outer = object(parse(body)); const value = object(outer?.response) ?? outer
  if (!value) return body
  const sections: string[] = []
  if (value.system) sections.push(`system\n${text(value.system)}`)
  if (value.instructions) sections.push(`instructions\n${text(value.instructions)}`)
  const messages = value.messages ?? value.input
  if (Array.isArray(messages)) for (const message of messages) {
    const item = object(message)
    sections.push(item ? `${item.role ?? item.type ?? 'input'}\n${text(item.content ?? item)}` : text(message))
  } else if (messages !== undefined) sections.push(text(messages))
  return sections.join('\n\n') || pretty(value)
}
function responseText(body: string): string {
  const full = object(parse(body)); const frames: ObjectValue[] = full ? [full] : []
  if (!full) for (const line of body.split('\n')) {
    const frame = object(parse(line.startsWith('data:') ? line.slice(5).trim() : line.trim()))
    if (frame) frames.push(frame)
  }
  const chunks: string[] = []; const tools: string[] = []; let fallback = ''
  for (const frame of frames) {
    if (Array.isArray(frame.choices)) for (const choice of frame.choices) {
      const item = object(choice); const delta = object(item?.delta) ?? object(item?.message)
      if (typeof delta?.content === 'string') chunks.push(delta.content)
      if (delta?.tool_calls) tools.push(pretty(delta.tool_calls))
    }
    if (frame.type === 'response.output_text.delta' && typeof frame.delta === 'string') chunks.push(frame.delta)
    if (frame.type === 'content_block_delta') {
      const delta = object(frame.delta)
      if (typeof delta?.text === 'string') chunks.push(delta.text)
      if (typeof delta?.partial_json === 'string') tools.push(delta.partial_json)
    }
    if (frame.type === 'response.function_call_arguments.delta' && typeof frame.delta === 'string') tools.push(frame.delta)
    const response = object(frame.response)
    if (response?.output) fallback = text(response.output)
    if (frame.output) fallback = text(frame.output)
    if (frame.content) fallback = text(frame.content)
    if (frame.error) tools.push(`error: ${pretty(frame.error)}`)
  }
  return [chunks.join('') || fallback, tools.length ? `工具调用 / 错误\n${tools.join('\n')}` : ''].filter(Boolean).join('\n\n') || pretty(parse(body))
}
function PayloadSection({ title, value }: { title: string; value: string }) {
  const [copyStatus, setCopyStatus] = useState('复制')
  return <section className="min-w-0 space-y-2">
    <div className="flex items-center justify-between gap-3"><h3 className="font-semibold">{title}</h3><Button variant="ghost" size="sm" onClick={() => { void navigator.clipboard.writeText(value).then(() => setCopyStatus('已复制'), () => setCopyStatus('请手动复制')) }}>{copyStatus}</Button></div>
    <pre className="max-h-[40vh] overflow-y-auto whitespace-pre-wrap break-words rounded-lg border bg-muted/40 p-3 font-mono text-xs leading-relaxed [overflow-wrap:anywhere]">{value || '没有正文'}</pre>
  </section>
}
export function ConversationDetails({ logId }: { logId: number }) {
  const [open, setOpen] = useState(false); const [data, setData] = useState<Conversation | null>(null)
  const [error, setError] = useState(''); const [loading, setLoading] = useState(false); const [raw, setRaw] = useState(false)
  useEffect(() => {
    if (!open) return
    let active = true
    setData(null); setError(''); setLoading(true); setRaw(false)
    api.getUsageConversation(logId).then(value => { if (active) setData(value) }, err => { if (active) setError(err instanceof Error ? err.message : '加载失败') }).finally(() => { if (active) setLoading(false) })
    return () => { active = false }
  }, [open, logId])
  return <>
    <Button variant="ghost" size="sm" className="h-8 gap-1 whitespace-nowrap px-2 text-xs" onClick={() => setOpen(true)} aria-label={`查看请求 ${logId} 的对话`}><MessageSquareText className="size-3.5" />查看对话</Button>
    <Modal show={open} title={`请求 #${logId} · 对话详情`} onClose={() => setOpen(false)} contentClassName="sm:max-w-4xl" bodyClassName="space-y-5">
      {loading && <p role="status">正在读取对话…</p>}{error && <p role="alert" className="rounded-lg bg-muted p-4 text-sm">{error}</p>}
      {data && <>
        <div className="flex flex-wrap items-center justify-between gap-2 text-xs text-muted-foreground"><span>{data.endpoint} · {new Date(data.created_at * 1000).toLocaleString()}</span><Button variant="outline" size="sm" aria-pressed={raw} onClick={() => setRaw(!raw)}>{raw ? '阅读视图' : '原始数据'}</Button></div>
        {data.truncated && <p role="status" className="rounded-lg border p-3 text-sm">内容超过单侧 8 MiB 的留存上限，显示的是已保存部分。</p>}
        <PayloadSection title="输入 / Prompt" value={raw ? pretty(parse(data.request_body)) : requestText(data.request_body)} />
        <PayloadSection title="回答" value={raw ? data.response_body : responseText(data.response_body)} />
      </>}
    </Modal>
  </>
}
export function ConversationRetentionSettings() {
  const [open, setOpen] = useState(false); const [days, setDays] = useState('7')
  const [loading, setLoading] = useState(false); const [saving, setSaving] = useState(false); const [error, setError] = useState('')
  useEffect(() => {
    if (!open) return
    let active = true
    setError(''); setLoading(true)
    api.getConversationRetention().then(value => { if (active) setDays(String(value.retention_days)) }, err => { if (active) setError(err instanceof Error ? err.message : '加载失败') }).finally(() => { if (active) setLoading(false) })
    return () => { active = false }
  }, [open])
  const valid = /^\d+$/.test(days) && Number(days) >= 1 && Number(days) <= 365
  const save = async () => {
    if (!valid || loading || saving) return
    setSaving(true); setError('')
    try { await api.updateConversationRetention(Number(days)); setOpen(false) } catch (err) { setError(err instanceof Error ? err.message : '保存失败') } finally { setSaving(false) }
  }
  return <>
    <Button variant="outline" onClick={() => setOpen(true)}><Settings2 className="size-3.5" />对话留存</Button>
    <Modal show={open} title="对话留存设置" onClose={() => { if (!saving) setOpen(false) }} footer={<Button onClick={() => void save()} disabled={!valid || loading || saving}>{saving ? '保存中…' : '保存'}</Button>}>
      <div className="space-y-4 text-sm"><p>默认记录所有 API Key 的输入和回答，仅管理员可查看。正文可能包含代码、文件片段和敏感信息；认证请求头不会留存。</p>
        <div className="space-y-2"><label htmlFor="conversation-retention-days" className="font-medium">保留天数</label><Input id="conversation-retention-days" type="number" min={1} max={365} step={1} value={days} disabled={loading || saving} onChange={event => setDays(event.target.value)} aria-describedby="conversation-retention-help" /><p id="conversation-retention-help" className="text-xs text-muted-foreground">1–365 天，默认 7 天。过期正文立即停止展示，后台每 30 分钟清理。缩短期限后，超期正文将无法恢复。</p></div>
        {loading && <p role="status">加载中…</p>}{error && <p role="alert" className="text-destructive">{error}</p>}
      </div>
    </Modal>
  </>
}
