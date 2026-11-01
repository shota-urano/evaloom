import { useEffect, useRef } from 'react'

import type { ApiClient, EventSourceLike, EventSourceListener } from '@/api/client'

export type StatusChanged = { executionId: string; from?: string; to?: string; reason?: string }
/** blocked イベント（02-data-model 5.3。payload は cause と blockId）。 */
export type BlockedEvent = { executionId: string; cause?: string; blockId?: string }

/**
 * Execution ごとに SSE（GET /api/executions/{id}/events）を購読し、status_changed を受け取る（13 3.5）。
 * - executionIds に残っている間は購読を続け、一覧の再取得では張り直さない（張り直すと過去のイベントを最初から受け直すため）。
 * - executionIds から外れたもの・アンマウント時は閉じる。
 * - 切断はブラウザの EventSource が自動で再接続し、Last-Event-ID から再開する（13 6節）。
 * - onBlocked を渡すと、同じ購読で blocked イベント（Block の ID と原因）も受け取る（13 3.4）。
 */
export function useExecutionStatusEvents(
  api: ApiClient,
  executionIds: readonly string[],
  onStatusChanged: (event: StatusChanged) => void,
  onBlocked?: (event: BlockedEvent) => void,
) {
  const handler = useRef(onStatusChanged)
  const blockedHandler = useRef(onBlocked)
  useEffect(() => {
    handler.current = onStatusChanged
    blockedHandler.current = onBlocked
  })
  const sources = useRef(new Map<string, { source: EventSourceLike; listener: EventSourceListener; blocked: EventSourceListener }>())
  const key = [...executionIds].sort().join(',')

  useEffect(() => {
    const wanted = new Set(key === '' ? [] : key.split(','))
    const current = sources.current
    for (const [id, { source, listener, blocked }] of current) {
      if (!wanted.has(id)) {
        source.removeEventListener('status_changed', listener)
        source.removeEventListener('blocked', blocked)
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
      const blocked: EventSourceListener = (event) => {
        let payload: { cause?: unknown; blockId?: unknown } = {}
        try {
          payload = JSON.parse(event.data) as typeof payload
        } catch {
          return
        }
        blockedHandler.current?.({
          executionId: id,
          cause: typeof payload.cause === 'string' ? payload.cause : undefined,
          blockId: typeof payload.blockId === 'string' ? payload.blockId : undefined,
        })
      }
      source.addEventListener('status_changed', listener)
      source.addEventListener('blocked', blocked)
      current.set(id, { source, listener, blocked })
    }
  }, [api, key])

  useEffect(() => {
    const current = sources.current
    return () => {
      for (const { source, listener, blocked } of current.values()) {
        source.removeEventListener('status_changed', listener)
        source.removeEventListener('blocked', blocked)
        source.close()
      }
      current.clear()
    }
  }, [])
}
