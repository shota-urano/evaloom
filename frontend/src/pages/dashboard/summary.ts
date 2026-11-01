import type { Runner } from '@/api/registry'
import type { TaskStatus, TaskSummary } from '@/api/tasks'

// Dashboard の区分（13 3.2「実行中タスク、待機中タスク、Blocked のタスク、エラー状況」）とタスク状態の対応。
// 02-data-model 5.2 で、動いている Execution（claimed〜publishing）に対応するタスク状態は running と verifying。
// claim 待ち（pending）の Execution に対応するタスク状態は queued。

export type TaskGroup = 'running' | 'waiting' | 'blocked' | 'error'

export const TASK_GROUP_STATUSES: Readonly<Record<TaskGroup, readonly TaskStatus[]>> = {
  running: ['running', 'verifying'],
  waiting: ['queued'],
  blocked: ['blocked'],
  error: ['failed', 'timed_out'],
}

export const TASK_GROUP_LABELS: Readonly<Record<TaskGroup, string>> = {
  running: '実行中',
  waiting: '待機中',
  blocked: 'Blocked',
  error: '失敗',
}

export const TASK_GROUPS: readonly TaskGroup[] = ['running', 'waiting', 'blocked', 'error']

export type DashboardSummary = {
  runners: { online: number; offline: number }
  tasks: Record<TaskGroup, TaskSummary[]>
}

export function summarize(runners: readonly Runner[], items: readonly TaskSummary[]): DashboardSummary {
  const tasks = { running: [], waiting: [], blocked: [], error: [] } as Record<TaskGroup, TaskSummary[]>
  for (const task of items) {
    const group = TASK_GROUPS.find((g) => TASK_GROUP_STATUSES[g].includes(task.status))
    if (group) tasks[group].push(task)
  }
  return {
    runners: {
      online: runners.filter((r) => r.status === 'online').length,
      offline: runners.filter((r) => r.status !== 'online').length,
    },
    tasks,
  }
}
