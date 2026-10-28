import { ApiError, type ApiClient } from './client'

// Execution の SSE（GET /api/executions/{id}/events、01-architecture 5.4）を fetch で読む。
// - 切断されたら自動で再接続し、最後に受け取ったイベントの ID を Last-Event-ID として送って続きから受け取る（13 6節）。
// - 再接続の前後で同じイベントが重ねて届いても、ID が受け取り済み以下のものは捨てる（表示が重複しない）。
// ブラウザの EventSource でも再接続はされるが、サーバーが 4xx/5xx を返すとそこで止まり、再開位置も指定できない。

export type StreamEvent = { id: number | null; type: string; data: string }

/** text/event-stream の行を組み立ててイベントにする（HTML Living Standard の解釈規則のうち id・event・data を扱う）。 */
export class EventStreamParser {
  private buffer = ''
  private data: string[] = []
  private type = ''
  private id: string | null = null
  private sawCR = false

  push(chunk: string): StreamEvent[] {
    const events: StreamEvent[] = []
    let text = chunk
    // 前の塊が CR で終わっていたら、続く LF は同じ改行の一部。
    if (this.sawCR && text.startsWith('\n')) text = text.slice(1)
    this.sawCR = text.endsWith('\r')
    this.buffer += text
    const lines = this.buffer.split(/\r\n|\r|\n/)
    this.buffer = lines.pop() ?? ''
    for (const line of lines) {
      if (line === '') {
        if (this.data.length > 0) {
          events.push({ id: this.id === null || this.id === '' ? null : toId(this.id), type: this.type || 'message', data: this.data.join('\n') })
        }
        this.data = []
        this.type = ''
        continue
      }
      if (line.startsWith(':')) continue
      const colon = line.indexOf(':')
      const field = colon === -1 ? line : line.slice(0, colon)
      let value = colon === -1 ? '' : line.slice(colon + 1)
      if (value.startsWith(' ')) value = value.slice(1)
      if (field === 'data') this.data.push(value)
      else if (field === 'event') this.type = value
      else if (field === 'id' && !value.includes('\0')) this.id = value
    }
    return events
  }
}

function toId(value: string): number | null {
  return /^[0-9]+$/.test(value) ? Number(value) : null
}

/** 再接続までの待ち時間。失敗が続くと倍にし、上限で止める。接続できたら最初の値に戻す。テストで短くする。 */
export const streamTiming = { reconnectDelayMs: 1000, maxReconnectDelayMs: 5000 }

export type StreamState = 'connecting' | 'open' | 'reconnecting' | 'closed'

export type FollowHandlers = {
  onEvent: (event: StreamEvent) => void
  /** closed のときは error に止まった理由（4xx の ApiError）が入る。 */
  onState?: (state: StreamState, error?: unknown) => void
}

export function executionEventsPath(executionId: string) {
  return `/api/executions/${encodeURIComponent(executionId)}/events`
}

/**
 * Execution のイベントを購読し続ける。戻り値を呼ぶと止める。
 * - 本文が終わった・通信が切れた・5xx のときは待ってから再接続する。
 * - 4xx（Execution が無い、ログインが切れた等）は再接続しても直らないので止める。
 */
export function followExecutionEvents(api: ApiClient, executionId: string, handlers: FollowHandlers): () => void {
  const controller = new AbortController()
  const { signal } = controller
  let lastId: number | null = null
  let failures = 0

  const deliver = (event: StreamEvent) => {
    if (event.id !== null) {
      if (lastId !== null && event.id <= lastId) return
      lastId = event.id
    }
    handlers.onEvent(event)
  }

  async function readOnce() {
    const headers: Record<string, string> = lastId === null ? {} : { 'Last-Event-ID': String(lastId) }
    const response = await api.stream(executionEventsPath(executionId), { headers, signal })
    failures = 0
    handlers.onState?.('open')
    const body = response.body
    if (!body) return
    const reader = body.getReader()
    const decoder = new TextDecoder()
    const parser = new EventStreamParser()
    try {
      for (;;) {
        const { value, done } = await reader.read()
        if (done) break
        for (const event of parser.push(decoder.decode(value, { stream: true }))) {
          if (signal.aborted) return
          deliver(event)
        }
      }
    } finally {
      reader.releaseLock()
    }
  }

  async function run() {
    let first = true
    while (!signal.aborted) {
      handlers.onState?.(first ? 'connecting' : 'reconnecting')
      first = false
      try {
        await readOnce()
      } catch (error) {
        if (signal.aborted) return
        if (error instanceof ApiError && error.status >= 400 && error.status < 500) {
          handlers.onState?.('closed', error)
          return
        }
      }
      if (signal.aborted) return
      handlers.onState?.('reconnecting')
      failures += 1
      const delay = Math.min(streamTiming.reconnectDelayMs * 2 ** (failures - 1), streamTiming.maxReconnectDelayMs)
      await new Promise<void>((resolve) => {
        const timer = setTimeout(resolve, delay)
        signal.addEventListener('abort', () => {
          clearTimeout(timer)
          resolve()
        }, { once: true })
      })
    }
  }

  void run()
  return () => controller.abort()
}
