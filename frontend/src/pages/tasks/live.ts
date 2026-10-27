import { useEffect, useRef } from 'react'

import type { ApiClient } from '@/api/client'
import type { TaskSummary } from '@/api/tasks'
import { useExecutionStatusEvents, type StatusChanged } from '@/hooks/use-execution-events'

// 02-data-model 5.2 の終端の Execution 状態。これ以外（blocked を含む）は状態が進みうるので購読する。
const FINAL_EXECUTION_STATUSES = new Set(['succeeded', 'failed', 'interrupted', 'cancelled', 'timed_out'])
// 購読の優先順（タスク状態）。実行中のものを先にし、待機中は古いもの（先に claim されるもの）から。
const WATCH_PRIORITY: Record<string, number> = { running: 0, verifying: 1, blocked: 2, received: 3, queued: 4 }
/**
 * 同時に張る SSE の上限。evaloomd は HTTP/1.1 なので、ブラウザの同一オリジン6接続の枠を SSE で使い切らないよう抑える。
 * 上限から漏れたタスクも、購読中のどれかの status_changed で一覧を取り直すときに最新になる。
 */
export const MAX_TASK_SUBSCRIPTIONS = 4

/** 一覧のうち、状態が進みうる最新 Execution の ID（優先順に上限まで）。 */
export function executionsToWatch(items: readonly TaskSummary[]): string[] {
  return items
    .filter((task) => task.latestExecution && !FINAL_EXECUTION_STATUSES.has(task.latestExecution.status))
    .map((task, index) => ({ task, index }))
    .sort((a, b) => (WATCH_PRIORITY[a.task.status] ?? 5) - (WATCH_PRIORITY[b.task.status] ?? 5) || b.index - a.index)
    .slice(0, MAX_TASK_SUBSCRIPTIONS)
    .map(({ task }) => task.latestExecution!.id)
}

/** status_changed を受けたら一覧を取り直す（13 3.5）。画面の再読み込みはしない。 */
export function useLiveTaskList(api: ApiClient, items: readonly TaskSummary[] | undefined, reload: () => void) {
  const shown = useRef(items)
  useEffect(() => {
    shown.current = items
  })
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)
  useEffect(() => () => clearTimeout(timer.current), [])
  useExecutionStatusEvents(api, items ? executionsToWatch(items) : [], (event: StatusChanged) => {
    // 購読を始めると過去のイベントも最初から届く。表示中の状態と同じ遷移先なら取り直さない。
    const task = shown.current?.find((t) => t.latestExecution?.id === event.executionId)
    if (task && event.to !== undefined && (event.to === task.status || event.to === task.latestExecution?.status)) return
    // 続けて届いた遷移はまとめて1回だけ取り直す。
    clearTimeout(timer.current)
    timer.current = setTimeout(reload, 100)
  })
}
