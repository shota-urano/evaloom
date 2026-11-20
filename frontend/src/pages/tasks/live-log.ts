import { useEffect, useState } from 'react'

import type { ApiClient } from '@/api/client'
import { followExecutionEvents, type StreamState } from '@/api/event-stream'
import { LEGACY_LOG_STREAMS, LOG_STREAMS, SAVED_LOG_KEYS, type ExecutionLogs, type LogStream } from '@/api/tasks'

export type LogLine = { key: string; stream: LogStream; text: string }

/** log イベントの stream を 13 3.3 の名前にする。旧名（harness / setup / test）は読み替え、知らない名前は null。 */
function streamOf(value: unknown): LogStream | null {
  if (typeof value !== 'string') return null
  if (LOG_STREAMS.includes(value as LogStream)) return value as LogStream
  return LEGACY_LOG_STREAMS[value] ?? null
}

/**
 * 実行中の Execution のログを SSE で受け取り、届いた順に足していく（13 3.3）。
 * 切断時の再接続と Last-Event-ID からの再開、重複の除去は followExecutionEvents が行う。
 */
export function useLiveLog(api: ApiClient, executionId: string, enabled: boolean) {
  const [lines, setLines] = useState<LogLine[]>([])
  const [state, setState] = useState<{ value: StreamState; error?: unknown }>({ value: 'connecting' })
  useEffect(() => {
    if (!enabled) return
    return followExecutionEvents(api, executionId, {
      onEvent: (event) => {
        if (event.type !== 'log') return
        let payload: { stream?: unknown; text?: unknown }
        try {
          payload = JSON.parse(event.data) as typeof payload
        } catch {
          return
        }
        const stream = streamOf(payload.stream)
        if (stream === null || typeof payload.text !== 'string') return
        const line: LogLine = { key: String(event.id ?? Math.random()), stream, text: payload.text }
        setLines((previous) => [...previous, line])
      },
      onState: (value, error) => setState({ value, error }),
    })
  }, [api, executionId, enabled])
  return { lines, state }
}

/** 保存済みログ（各 stream の本文）を行に分ける。応答のキーは旧名（SAVED_LOG_KEYS）。末尾の改行で空行を作らない。 */
export function savedLines(logs: ExecutionLogs): LogLine[] {
  const lines: LogLine[] = []
  for (const stream of LOG_STREAMS) {
    const text = logs[SAVED_LOG_KEYS[stream]] ?? ''
    if (text === '') continue
    const parts = text.replace(/\r?\n$/, '').split(/\r?\n/)
    parts.forEach((part, index) => lines.push({ key: `${stream}:${index}`, stream, text: part }))
  }
  return lines
}
