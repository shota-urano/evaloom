import type { TaskStatus } from '@/api/tasks'

export const TASKS_PATH = '/tasks'
/** タスク ID は task-<数字> なので、「_」を含むこのパスはタスク詳細（/tasks/:id）と衝突しない。 */
export const TASK_NEW_PATH = '/tasks/_new'

/** 02-data-model 5.1 の表示名。 */
export const TASK_STATUS_LABELS: Record<TaskStatus, string> = {
  received: 'Received',
  planning: 'Planning',
  queued: 'Queued',
  running: 'Running',
  verifying: 'Verifying',
  waiting_approval: 'Waiting Approval',
  completed: 'Completed',
  failed: 'Failed',
  cancelled: 'Cancelled',
  timed_out: 'Timed Out',
  blocked: 'Blocked',
}

/** 絞り込みの選択肢。Phase 1 で起こらない planning・waiting_approval（02 5.1）は出さない。 */
export const TASK_STATUS_FILTERS: readonly TaskStatus[] = [
  'received',
  'queued',
  'running',
  'verifying',
  'blocked',
  'completed',
  'failed',
  'cancelled',
  'timed_out',
]

export function taskStatusLabel(status: string): string {
  return TASK_STATUS_LABELS[status as TaskStatus] ?? status
}

// タスク作成の各項目の表示名（13 3.2「タスク作成」、04 3.1）。
export const TASK_LABELS = {
  projectId: 'プロジェクト',
  title: '件名',
  body: '本文',
  acceptanceCriteria: '受け入れ条件',
  shortName: '短い名前',
} as const

/** 04 3.1 の短い名前（backend/internal/api/tasks.go の shortNamePattern）。 */
export const SHORT_NAME_PATTERN = /^[a-z0-9-]{1,40}$/
