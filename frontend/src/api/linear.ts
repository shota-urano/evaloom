import type { ApiClient } from './client'

// Linear の取り込みの状況（15-linear-intake 5章 GET /api/linear/status、backend/internal/api/linear.go・linear.md）。
// API キーの値と Linear の応答本文は含まれない。

/** linear_poll_teams の失敗の分類（15 3.8）。成功したら null。 */
export type LinearPollError = 'auth' | 'rate_limited' | 'network' | 'other'

export type LinearTeamStatus = {
  teamId: string
  /** 最後に照会を読み切れた時刻。まだ一度も成功していなければ null。 */
  lastSuccessAt: string | null
  lastError: LinearPollError | string | null
  consecutiveFailures: number
}

export type LinearIssueStatus = {
  linearIssueId: string
  issueIdentifier: string
  teamId: string
  lastDecision: string | null
  lastDecisionAt: string | null
}

/**
 * 照会先 Team（uslab）が決まらない理由（15 3.2・6章、Q216・Q225）。決まっていれば null。
 * message は Control Plane が作る画面用の文で、そのまま出す。
 */
export type LinearTeamProblem = { reason: 'not_found' | 'multiple' | string; message: string }

/**
 * 振り分けの提案を動かす Runner が無い理由（15 5章・6章、Q229）。あれば null。
 * Dashboard はこの項目だけで表示し、GET /api/settings と GET /api/runners から自分で導かない。
 */
export type LinearRoutingRunnerProblem = { reason: 'unset' | 'not_found' | 'offline' | string; message: string }

export type LinearStatus = {
  teams: LinearTeamStatus[]
  /** 古い応答（項目が無い）でも表示できるよう省略を許す。省略は null と同じ扱い。 */
  teamProblem?: LinearTeamProblem | null
  routingRunnerProblem?: LinearRoutingRunnerProblem | null
  issues: LinearIssueStatus[]
}

export const linearStatus = {
  get: (api: ApiClient, signal?: AbortSignal) => api.get<LinearStatus>('/api/linear/status', { signal }),
}

/**
 * 続けて失敗した回数がこの値に達したら Dashboard のエラー状況に出す（15 6章、2026-11-08 決定の5回）。
 * Control Plane の macOS 通知と同じ境目（backend/internal/linearintake の FailureAlertThreshold）。
 */
export const LINEAR_FAILURE_ALERT_THRESHOLD = 5

export const LINEAR_POLL_ERRORS: Readonly<Record<string, string>> = {
  auth: '認証エラー',
  rate_limited: 'レート制限',
  network: '通信エラー',
  other: 'その他のエラー',
}

/** 失敗の分類を画面の文言にする。未知の値は原文のまま出す。 */
export function pollErrorLabel(error: string): string {
  return LINEAR_POLL_ERRORS[error] ?? error
}

/** linear_issue_states.last_decision（15 3.8）の表示名。 */
export const LINEAR_DECISIONS: Readonly<Record<string, string>> = {
  started: 'タスクを作成',
  held: '保留',
  cancelled: '中止',
  skipped_active_task: '終端でないタスクがあるため作成せず',
  skipped_initial: '最初の照会のため記録のみ',
  skipped_cancelled: 'キャンセル済みのため記録のみ',
}

export function decisionLabel(decision: string): string {
  return LINEAR_DECISIONS[decision] ?? decision
}

/** 照会先 Team が決まらない理由（teamProblem.reason）の表示名。未知の値は原文のまま出す。 */
export const LINEAR_TEAM_PROBLEMS: Readonly<Record<string, string>> = {
  not_found: '照会先 Team が見つからない',
  multiple: '照会先 Team が複数ある',
}

/** 振り分けの提案を動かす Runner が無い理由（routingRunnerProblem.reason、Q229）の表示名。 */
export const ROUTING_RUNNER_PROBLEMS: Readonly<Record<string, string>> = {
  unset: '未設定',
  not_found: 'Runner が無い',
  offline: 'offline',
}

export function teamProblemLabel(reason: string): string {
  return LINEAR_TEAM_PROBLEMS[reason] ?? reason
}

export function routingRunnerProblemLabel(reason: string): string {
  return ROUTING_RUNNER_PROBLEMS[reason] ?? reason
}

export function isPollFailing(team: LinearTeamStatus): boolean {
  return team.consecutiveFailures >= LINEAR_FAILURE_ALERT_THRESHOLD
}
