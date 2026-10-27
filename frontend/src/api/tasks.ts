import type { ApiClient } from './client'
import { listAll } from './registry'

// タスクの本人用 API（01-architecture 5.1、04-task-lifecycle 3.1、backend/internal/api/tasks.go・tasks.md）の形。

/** 02-data-model 5.1 のタスク状態。 */
export type TaskStatus =
  | 'received'
  | 'planning'
  | 'queued'
  | 'running'
  | 'verifying'
  | 'waiting_approval'
  | 'completed'
  | 'failed'
  | 'cancelled'
  | 'timed_out'
  | 'blocked'

export type Task = {
  id: string
  projectId: string
  title: string
  body: string
  acceptanceCriteria: string
  shortName: string
  taskType: string
  collaborationMode: string
  status: TaskStatus
  createdAt: string
  updatedAt: string
}

/** 最新の Execution（seq が最大のもの）。状態の値は 02-data-model 5.2。 */
export type ExecutionSummary = { id: string; seq: number; mode: string; status: string; errorClass: string | null }

/** GET /api/tasks の要素。prUrl は PR を作った Execution のうち最新のものの URL。 */
export type TaskSummary = Task & { latestExecution: ExecutionSummary | null; prUrl: string | null }

export type TaskFilter = { status?: string; projectId?: string }

/** 04 3.1 の入力。shortName は省略するとサーバーが `task` にする。 */
export type TaskInput = {
  projectId: string
  title: string
  body: string
  acceptanceCriteria: string
  shortName?: string
}

export function taskListPath(filter: TaskFilter = {}): string {
  const params = new URLSearchParams()
  if (filter.status) params.set('status', filter.status)
  if (filter.projectId) params.set('projectId', filter.projectId)
  const query = params.toString()
  return query ? `/api/tasks?${query}` : '/api/tasks'
}

/** 送信ごとに作る Idempotency-Key（13 3.1「二重送信」）。 */
export function newIdempotencyKey(): string {
  return globalThis.crypto.randomUUID()
}

export const tasks = {
  list: (api: ApiClient, filter: TaskFilter = {}, signal?: AbortSignal) => listAll<TaskSummary>(api, taskListPath(filter), signal),
  /** 同じ Idempotency-Key の再送には、サーバーが最初に作ったタスクを返す（新規は 201、再送は 200）。 */
  create: (api: ApiClient, input: TaskInput, idempotencyKey: string) =>
    api.post<Task>('/api/tasks', input, { headers: { 'Idempotency-Key': idempotencyKey } }),
}
