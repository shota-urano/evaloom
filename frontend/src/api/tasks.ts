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

/** GET /api/tasks/{id} の executions の要素（backend/internal/api/task_detail.go）。seq の昇順。 */
export type ExecutionHistoryItem = ExecutionSummary & {
  profileVersion: string | null
  profileCommitSha: string | null
  startedAt: string | null
  finishedAt: string | null
}

/** GET /api/tasks/{id}。タスク一覧の要素に Execution の履歴を加えたもの。 */
export type TaskDetail = TaskSummary & { executions: ExecutionHistoryItem[] }

/** 02-data-model 5.5 の Finding。 */
export type Finding = { rule: string; path: string; line: number; message: string }

export type ContextRevision = { id: string; layer: string; kind: string; sourcePath: string; sourceCommitSha: string; contentHash: string }

export type GateResult = { id: string; gate: number; result: string; baseSha: string; targetSha: string; findings: Finding[]; createdAt: string }

/** GET /api/executions/{id}（backend/internal/api/execution_detail.go）。 */
export type ExecutionDetail = ExecutionHistoryItem & {
  taskId: string
  runnerId: string | null
  role: string | null
  harnessId: string | null
  profileId: string | null
  prUrl: string | null
  contextRevisions: ContextRevision[]
  gateResults: GateResult[]
}

/** 05-runner 3.4 のログの stream。 */
export const LOG_STREAMS = ['harness', 'setup', 'test', 'runner'] as const
export type LogStream = (typeof LOG_STREAMS)[number]

/** GET /api/executions/{id}/logs（backend/internal/api/execution_logs.md）。未保存の stream は空文字列。 */
export type ExecutionLogs = Record<LogStream, string>

/** 04 3.7 の再実行の種類。continue=継続、fresh=最初から。 */
export type RetryMode = 'continue' | 'fresh'

/** 02-data-model 5.2 の終端の Execution 状態。 */
export const FINAL_EXECUTION_STATUSES: ReadonlySet<string> = new Set(['succeeded', 'failed', 'interrupted', 'cancelled', 'timed_out'])

/** 成果物の取得（GET /api/executions/{id}/artifacts/{artifactId}）の URL。 */
export function artifactPath(executionId: string, artifactId: string) {
  return `/api/executions/${encodeURIComponent(executionId)}/artifacts/${encodeURIComponent(artifactId)}`
}

const taskPath = (id: string) => `/api/tasks/${encodeURIComponent(id)}`
const executionPath = (id: string) => `/api/executions/${encodeURIComponent(id)}`

export const tasks = {
  get: (api: ApiClient, id: string, signal?: AbortSignal) => api.get<TaskDetail>(taskPath(id), { signal }),
  getExecution: (api: ApiClient, id: string, signal?: AbortSignal) => api.get<ExecutionDetail>(executionPath(id), { signal }),
  getLogs: (api: ApiClient, id: string, signal?: AbortSignal) => api.get<ExecutionLogs>(`${executionPath(id)}/logs`, { signal }),
  /** 成功は 204。対象外の状態は 409。 */
  cancel: (api: ApiClient, id: string) => api.post<void>(`${taskPath(id)}/cancel`),
  /** 新しい Execution を作る（成功は 204）。対象外の状態は 409。 */
  retry: (api: ApiClient, id: string, mode: RetryMode) => api.post<void>(`${taskPath(id)}/retry`, { mode }),
  list: (api: ApiClient, filter: TaskFilter = {}, signal?: AbortSignal) => listAll<TaskSummary>(api, taskListPath(filter), signal),
  /** 同じ Idempotency-Key の再送には、サーバーが最初に作ったタスクを返す（新規は 201、再送は 200）。 */
  create: (api: ApiClient, input: TaskInput, idempotencyKey: string) =>
    api.post<Task>('/api/tasks', input, { headers: { 'Idempotency-Key': idempotencyKey } }),
}
