import type { TaskStatus } from '@/api/tasks'

export const TASKS_PATH = '/tasks'
/** タスク ID は task-<数字> なので、「_」を含むこのパスはタスク詳細（/tasks/:id）と衝突しない。 */
export const TASK_NEW_PATH = '/tasks/_new'

export function taskDetailPath(id: string) {
  return `${TASKS_PATH}/${encodeURIComponent(id)}`
}

/** 02-data-model 5.1 の表示名。 */
export const TASK_STATUS_LABELS: Record<TaskStatus, string> = {
  received: 'Received',
  routing: 'Routing',
  queued: 'Queued',
  running: 'Running',
  waiting_approval: 'Waiting Approval',
  blocked: 'Blocked',
  completed: 'Completed',
  failed: 'Failed',
  cancelled: 'Cancelled',
  timed_out: 'Timed Out',
}

/** 絞り込みの選択肢。02-data-model 5.1 の10状態をその順に出す（バックエンドはこれ以外の値を 400 にする）。 */
export const TASK_STATUS_FILTERS: readonly TaskStatus[] = Object.keys(TASK_STATUS_LABELS) as TaskStatus[]

export function taskStatusLabel(status: string): string {
  return TASK_STATUS_LABELS[status as TaskStatus] ?? status
}

// タスク作成の各項目の表示名（13 3.2「タスク作成」、04 3.1）。サーバーの details[].field に合わせる。
export const TASK_LABELS = {
  destination: '振り分け先',
  projectId: '振り分け先（Project）',
  harnessId: '振り分け先（スペシャルハーネス）',
  title: '件名',
  body: '本文',
  acceptanceCriteria: '受け入れ条件',
} as const

/** 04 3.7 の再実行の種類の表示名。 */
export const RETRY_MODE_LABELS: Record<string, string> = { continue: '継続', fresh: '最初から' }

/** 中止できるタスク状態（04 3.2 の遷移表、本人が中止）。判定の正本はサーバー（対象外は 409）。 */
export const CANCELLABLE_TASK_STATUSES: ReadonlySet<string> = new Set(['received', 'routing', 'queued', 'running', 'waiting_approval', 'blocked'])
/** 再実行できるタスク状態（04 3.2 の遷移表・3.7）。判定の正本はサーバー（対象外は 409）。 */
export const RETRYABLE_TASK_STATUSES: ReadonlySet<string> = new Set(['failed', 'timed_out', 'cancelled', 'completed', 'blocked'])
