import { useEffect, useRef } from 'react'

import type { ApiClient, EventSourceLike, EventSourceListener } from '@/api/client'

export type StatusChanged = { executionId: string; from?: string; to?: string; reason?: string }

/**
 * Execution ごとに SSE（GET /api/executions/{id}/events）を購読し、status_changed を受け取る（13 3.5）。
 * - executionIds に残っている間は購読を続け、一覧の再取得では張り直さない（張り直すと過去のイベントを最初から受け直すため）。
 * - executionIds から外れたもの・アンマウント時は閉じる。
 * - 切断はブラウザの EventSource が自動で再接続し、Last-Event-ID から再開する（13 6節）。
 */
export function useExecutionStatusEvents(api: ApiClient, executionIds: readonly string[], onStatusChanged: (event: StatusChanged) => void) {
  const handler = useRef(onStatusChanged)
  useEffect(() => {
    handler.current = onStatusChanged
  })
  const sources = useRef(new Map<string, { source: EventSourceLike; listener: EventSourceListener }>())
  const key = [...executionIds].sort().join(',')

  useEffect(() => {
    const wanted = new Set(key === '' ? [] : key.split(','))
    const current = sources.current
    for (const [id, { source, listener }] of current) {
      if (!wanted.has(id)) {
        source.removeEventListener('status_changed', listener)
        source.close()
        current.delete(id)
      }
    }
    for (const id of wanted) {
      if (current.has(id)) continue
      const source = api.events(`/api/executions/${encodeURIComponent(id)}/events`)
      const listener: EventSourceListener = (event) => {
        let payload: Omit<StatusChanged, 'executionId'> = {}
        try {
          payload = JSON.parse(event.data) as typeof payload
        } catch {
          // 形式が違っても状態が変わったことは確かなので、通知だけは行う。
        }
        handler.current({ ...payload, executionId: id })
      }
      source.addEventListener('status_changed', listener)
      current.set(id, { source, listener })
    }
  }, [api, key])

  useEffect(() => {
    const current = sources.current
    return () => {
      for (const { source, listener } of current.values()) {
        source.removeEventListener('status_changed', listener)
        source.close()
      }
      current.clear()
    }
  }, [])
}
