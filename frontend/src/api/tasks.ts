import type { ApiClient } from './client'
import type { CheckResult, Decision, DecisionApproval } from './decisions'
import { decisions } from './decisions'
import { listAll } from './registry'

// タスクの本人用 API（01-architecture 5.1、04-task-lifecycle 3.1、backend/internal/api/tasks.go・tasks.md）の形。

/** 02-data-model 5.1 のタスク状態（10個）。requirements 8.3 の planning・verifying は使わない（02 Q119）。 */
export type TaskStatus =
  | 'received'
  | 'routing'
  | 'queued'
  | 'running'
  | 'waiting_approval'
  | 'completed'
  | 'failed'
  | 'cancelled'
  | 'timed_out'
  | 'blocked'

export type Task = {
  id: string
  /** 振り分け先は projectId と harnessId のどちらか一方（02 3.3）。もう一方は空文字列（backend/internal/api/tasks.md）。 */
  projectId: string
  harnessId: string
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

/** 出す段が報告した外に出したもの（02 3.2 executions.outputs、契約）。不透明な値なので、画面は label と url をそのまま出す（13 3.1）。 */
export type ExecutionOutput = { kind: string; label: string; url: string }

/** 最新の Execution（seq が最大のもの）。状態の値は 02-data-model 5.2。 */
export type ExecutionSummary = { id: string; seq: number; mode: string; status: string; errorClass: string | null }

/** GET /api/tasks の latestExecution。一覧だけが outputs を返す（無ければ空配列、backend/internal/api/tasks.go）。 */
export type LatestExecutionSummary = ExecutionSummary & { outputs?: ExecutionOutput[] }

/** GET /api/tasks の要素。 */
export type TaskSummary = Task & {
  latestExecution: LatestExecutionSummary | null
  /** 15-linear-intake 5章。`manual` / `linear`。 */
  source?: 'manual' | 'linear'
  /** 15-linear-intake 3.6 の保留の理由。保留していなければ null。 */
  intakeBlockCause?: string | null
  /** Linear 起因のタスクの Issue の識別子と URL。手動作成のタスクは null。 */
  linear?: { identifier: string; url: string } | null
}

/** 振り分け先の絞り込みは projectId と harnessId のどちらか一方だけを使う。 */
export type TaskFilter = { status?: string; projectId?: string; harnessId?: string }

/** 04 3.1 の振り分け先。Project かスペシャルハーネスのどちらか一方だけを送る（01 5.1 POST /api/tasks）。 */
export type TaskDestination = { projectId: string } | { harnessId: string }

/** 04 3.1 の入力（13 3.2「タスク作成」）。 */
export type TaskInput = TaskDestination & {
  title: string
  body: string
  acceptanceCriteria: string
}

export function taskListPath(filter: TaskFilter = {}): string {
  const params = new URLSearchParams()
  if (filter.status) params.set('status', filter.status)
  if (filter.projectId) params.set('projectId', filter.projectId)
  if (filter.harnessId) params.set('harnessId', filter.harnessId)
  const query = params.toString()
  return query ? `/api/tasks?${query}` : '/api/tasks'
}

/** 送信ごとに作る Idempotency-Key（13 3.1「二重送信」）。 */
export function newIdempotencyKey(): string {
  return globalThis.crypto.randomUUID()
}

/** GET /api/tasks/{id} の executions の要素（backend/internal/api/task_detail.go）。seq の昇順。outputs は無ければ空配列。 */
export type ExecutionHistoryItem = ExecutionSummary & {
  outputs?: ExecutionOutput[]
  profileVersion: string | null
  profileCommitSha: string | null
  startedAt: string | null
  finishedAt: string | null
}

/** GET /api/tasks/{id}。タスク一覧の要素に Execution の履歴を加えたもの。 */
export type TaskDetail = TaskSummary & { executions: ExecutionHistoryItem[] }

export type ContextRevision = { id: string; layer: string; kind: string; sourcePath: string; sourceCommitSha: string; contentHash: string }

/** 02 3.4 stage_runs の段。 */
export type Stage = 'prepare' | 'work' | 'check' | 'publish'

/** GET /api/executions/{id} の stageRuns の要素。message と summary は結果 JSON（契約 6.5）から読んだ値。 */
export type StageRun = {
  id: string
  stage: Stage
  attempt: number
  status: string
  errorClass: string | null
  startedAt: string
  finishedAt: string | null
  message: string | null
  summary: string | null
}

/** GET /api/executions/{id} の checkResults の要素（check_results の行）。 */
export type ExecutionCheckResult = Omit<CheckResult, 'createdAt'>

/** GET /api/executions/{id} の artifacts の要素（artifacts の行のうち本文を除いたもの）。 */
export type ExecutionArtifact = { id: string; kind: string; stage: string; name: string }

/** GET /api/executions/{id}（backend/internal/api/execution_detail.go）。 */
export type ExecutionDetail = ExecutionHistoryItem & {
  taskId: string
  runnerId: string | null
  role: string | null
  harnessId: string | null
  profileId: string | null
  contextRevisions: ContextRevision[]
  params: unknown
  contractVersion: string | null
  resumeStage: string | null
  stageRuns: StageRun[]
  checkResults: ExecutionCheckResult[]
  artifacts: ExecutionArtifact[]
}

/** 13 3.3 のログの stream。段（prepare / work / check / publish）と Runner 自身のログ。 */
export const LOG_STREAMS = ['prepare', 'work', 'check', 'publish', 'runner'] as const
export type LogStream = (typeof LOG_STREAMS)[number]

/**
 * 保存済みログの応答のキー（backend/internal/api/execution_logs.md）。応答は旧名のキーを使うので、stream 名との対応をここに置く。
 * work=harness、prepare=setup、check=test。
 */
export const SAVED_LOG_KEYS: Record<LogStream, string> = { prepare: 'setup', work: 'harness', check: 'test', publish: 'publish', runner: 'runner' }

/** Runner が旧名で送った log イベントの stream を、13 3.3 の stream 名に読み替える（backend/internal/api/runner_logs.go は両方を受け付ける）。 */
export const LEGACY_LOG_STREAMS: Record<string, LogStream> = { harness: 'work', setup: 'prepare', test: 'check' }

/** GET /api/executions/{id}/logs。未保存の stream は空文字列。キーは SAVED_LOG_KEYS の旧名。 */
export type ExecutionLogs = Record<string, string>

/** 04 3.7 の再実行の種類。continue=継続、fresh=最初から。 */
export type RetryMode = 'continue' | 'fresh'

/** 02-data-model 5.2 の終端の Execution 状態。 */
export const FINAL_EXECUTION_STATUSES: ReadonlySet<string> = new Set(['succeeded', 'failed', 'interrupted', 'cancelled', 'timed_out'])

/** 成果物の取得（GET /api/executions/{id}/artifacts/{artifactId}）の URL。 */
export function artifactPath(executionId: string, artifactId: string) {
  return `/api/executions/${encodeURIComponent(executionId)}/artifacts/${encodeURIComponent(artifactId)}`
}

/** GET /api/approvals の要素（backend/internal/api/approvals.go approvalView）。 */
export type TaskApproval = DecisionApproval & { consumedAt: string | null }

/** 外に出したもの（outputs）の要素のうち、label と url が文字列のものだけ（不透明な値なので形を確かめてから出す）。 */
export function outputLinks(outputs: unknown): ExecutionOutput[] {
  if (!Array.isArray(outputs)) return []
  return outputs.filter((o): o is ExecutionOutput => o !== null && typeof o === 'object' && typeof o.label === 'string' && typeof o.url === 'string')
}

const taskPath = (id: string) => `/api/tasks/${encodeURIComponent(id)}`
const executionPath = (id: string) => `/api/executions/${encodeURIComponent(id)}`

export const tasks = {
  get: (api: ApiClient, id: string, signal?: AbortSignal) => api.get<TaskDetail>(taskPath(id), { signal }),
  getExecution: (api: ApiClient, id: string, signal?: AbortSignal) => api.get<ExecutionDetail>(executionPath(id), { signal }),
  getLogs: (api: ApiClient, id: string, signal?: AbortSignal) => api.get<ExecutionLogs>(`${executionPath(id)}/logs`, { signal }),
  /** このタスクの承認（GET /api/approvals をすべて読み、taskId で絞る。サーバーに taskId の絞り込みは無い）。 */
  approvals: async (api: ApiClient, id: string, signal?: AbortSignal) =>
    (await listAll<TaskApproval>(api, '/api/approvals', signal)).filter((approval) => approval.taskId === id),
  /** このタスクの開いている判断待ち（GET /api/decisions?status=open をすべて読み、taskId で絞る）。 */
  openDecisions: async (api: ApiClient, id: string, signal?: AbortSignal): Promise<Decision[]> =>
    (await decisions.list(api, 'open', undefined, signal)).filter((decision) => decision.taskId === id),
  /** 成功は 204。対象外の状態は 409。 */
  cancel: (api: ApiClient, id: string) => api.post<void>(`${taskPath(id)}/cancel`),
  /** 新しい Execution を作る（成功は 204）。対象外の状態は 409。 */
  retry: (api: ApiClient, id: string, mode: RetryMode) => api.post<void>(`${taskPath(id)}/retry`, { mode }),
  list: (api: ApiClient, filter: TaskFilter = {}, signal?: AbortSignal) => listAll<TaskSummary>(api, taskListPath(filter), signal),
  /** 同じ Idempotency-Key の再送には、サーバーが最初に作ったタスクを返す（新規は 201、再送は 200）。 */
  create: (api: ApiClient, input: TaskInput, idempotencyKey: string) =>
    api.post<Task>('/api/tasks', input, { headers: { 'Idempotency-Key': idempotencyKey } }),
}
