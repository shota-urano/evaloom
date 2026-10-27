import { useEffect, useRef } from 'react'

import type { ApiClient } from '@/api/client'
import type { TaskSummary } from '@/api/tasks'
import { useExecutionStatusEvents, type StatusChanged } from '@/hooks/use-execution-events'

// 02-data-model 5.2 の終端の Execution 状態。これ以外（blocked を含む）は状態が進みうるので見張る。
const FINAL_EXECUTION_STATUSES = new Set(['succeeded', 'failed', 'interrupted', 'cancelled', 'timed_out'])
// 購読の優先順（タスク状態）。実行中のものを先にし、待機中は古いもの（先に claim されるもの）から。
const WATCH_PRIORITY: Record<string, number> = { running: 0, verifying: 1, blocked: 2, received: 3, queued: 4 }
/**
 * 同時に張る SSE の上限。evaloomd は HTTP/1.1 なので、ブラウザの同一オリジン6接続の枠を SSE で使い切らないよう抑える。
 * 上限から漏れたタスクがある間は、TASK_LIST_POLL_MS ごとに一覧を取り直して遷移を拾う。
 */
export const MAX_TASK_SUBSCRIPTIONS = 4
export const TASK_LIST_POLL_MS = 5000

/** 状態が進みうる（最新 Execution が終端でない）タスクを、購読の優先順に並べる。 */
function progressing(items: readonly TaskSummary[]): TaskSummary[] {
  return items
    .filter((task) => task.latestExecution && !FINAL_EXECUTION_STATUSES.has(task.latestExecution.status))
    .map((task, index) => ({ task, index }))
    .sort((a, b) => (WATCH_PRIORITY[a.task.status] ?? 5) - (WATCH_PRIORITY[b.task.status] ?? 5) || b.index - a.index)
    .map(({ task }) => task)
}

/**
 * 一覧を最新に保つ（13 3.5）。画面の再読み込みはしない。
 * - 状態が進みうるタスクの最新 Execution を SSE で購読し、status_changed を受けたら一覧を取り直す。
 * - 購読の上限から漏れたタスクがある間は、一定間隔で一覧を取り直す（購読していない Execution の遷移も拾うため）。
 */
export function useLiveTaskList(api: ApiClient, items: readonly TaskSummary[] | undefined, reload: () => void) {
  const shown = useRef(items)
  const reloadRef = useRef(reload)
  useEffect(() => {
    shown.current = items
    reloadRef.current = reload
  })
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)
  useEffect(() => () => clearTimeout(timer.current), [])

  const candidates = items ? progressing(items) : []
  const watched = candidates.slice(0, MAX_TASK_SUBSCRIPTIONS).map((task) => task.latestExecution!.id)
  const overflow = candidates.length > MAX_TASK_SUBSCRIPTIONS

  useEffect(() => {
    if (!overflow) return
    const interval = setInterval(() => reloadRef.current(), TASK_LIST_POLL_MS)
    return () => clearInterval(interval)
  }, [overflow])

  useExecutionStatusEvents(api, watched, (event: StatusChanged) => {
    // 購読を始めると過去のイベントも最初から届く。表示中の最新 Execution の状態と同じ遷移先なら取り直さない。
    // タスク状態との一致では判断しない（preparing → running のように、タスクは running のまま Execution だけ進むため。02 5.2）。
    // タスク作成のイベント（reason=task_created、遷移先はタスク状態）は一覧に出ている時点で反映済み。
    const execution = shown.current?.find((t) => t.latestExecution?.id === event.executionId)?.latestExecution
    if (execution && (event.to === execution.status || event.reason === 'task_created')) return
    // 続けて届いた遷移はまとめて1回だけ取り直す。
    clearTimeout(timer.current)
    timer.current = setTimeout(() => reloadRef.current(), 100)
  })
}
