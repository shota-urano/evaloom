import type { ApiClient } from './client'
import { listAll } from './registry'

// 本人の判断待ちの API（01-architecture 5.1、02-data-model 3.4 owner_decisions・5.6、
// backend/internal/api/decisions.go）の形。detail・target・contentId は不透明な値として、そのまま表示する（13 3.1）。

/** 02 5.6 の判断の種類。 */
export type DecisionKind = 'route' | 'approval' | 'blocked' | 'question'
export const DECISION_KINDS: readonly DecisionKind[] = ['route', 'approval', 'blocked', 'question']

/** 13 1章の「本人の判断」の呼び名。 */
export const DECISION_KIND_LABELS: Record<DecisionKind, string> = {
  route: '振り分けの承認',
  approval: '操作の承認',
  blocked: '止まった仕事の続行',
  question: '作業中の AI の質問',
}

/** owner_decisions.status。 */
export type DecisionStatus = 'open' | 'answered' | 'withdrawn'
export const DECISION_STATUSES: readonly DecisionStatus[] = ['open', 'answered', 'withdrawn']
export const DECISION_STATUS_LABELS: Record<DecisionStatus, string> = {
  open: '判断待ち',
  answered: '判断済み',
  withdrawn: '取り下げ',
}

/** 02 5.6 の原因。kind=blocked は check_blocked 以下の4つ、kind=question は question。 */
export type DecisionCause = 'check_blocked' | 'publish_unknown' | 'context_conflict' | 'workspace_missing' | 'question'

/** owner_decisions.withdrawn_reason（02 3.4）。 */
export type WithdrawnReason = 'cancelled' | 'superseded' | 'expired'

/** GET /api/decisions の要素（owner_decisions の列）。 */
export type Decision = {
  id: string
  kind: DecisionKind
  taskId: string
  executionId: string | null
  subjectId: string | null
  cause: DecisionCause | null
  basis: string | null
  /** 画面に出す内容（02 3.4）。形は kind・cause ごとに違う。 */
  detail: Record<string, unknown> | null
  status: DecisionStatus
  withdrawnReason: WithdrawnReason | null
  answer: Record<string, unknown> | null
  answeredAt: string | null
  createdAt: string
}

/** route_proposals の行（backend/internal/store/owner_decisions.go GetRouteProposalJSON）。 */
export type RouteCandidate = { label: string; kind: string; id: string; description: string | null }
export type RouteProposal = {
  id: string
  taskId: string
  trigger: 'no_label' | 'unknown_label' | 'multiple_labels'
  seenLabels: string[]
  candidates: RouteCandidate[]
  status: string
  runnerId: string | null
  proposedLabel: string | null
  reason: string | null
  errorClass: string | null
  decisionId: string | null
  createdAt: string
  finishedAt: string | null
}

/** kind=approval の詳細に付く approvals の行（backend/internal/api/approvals.go approvalView）。 */
export type DecisionApproval = {
  id: string
  decisionId: string
  taskId: string
  executionId: string
  requestedStage: string
  operation: string
  target: unknown
  summary: string | null
  contentId: string
  status: string
  invalidatedReason: string | null
  decidedAt: string | null
  expiresAt: string | null
  createdAt: string
}

/** GET /api/decisions/{id}。 */
export type DecisionDetail = Decision & { approval?: DecisionApproval; routeProposal?: RouteProposal }

/** 02 5.6 の回答の形。 */
export type DecisionAnswer =
  | { action: 'approve'; label: string; basis: string }
  | { action: 'reject' }
  | { action: 'approve'; contentId: string; ttlSec?: number }
  | { action: 'reject'; contentId: string }
  | { action: 'continue' | 'abort'; contentId: string }
  | { action: 'continue' }
  | { action: 'abort' }
  | { action: 'answer'; text: string; option: string | null }

/** Profile 定義（契約 5章）のうち、この画面が読む部分。 */
export type ProfileDefinition = { description?: string; approvals?: { operation: string; description: string; waivable?: boolean }[] }

/** Profile 定義から操作の description を探す。無ければ null。 */
export function operationDescription(definition: ProfileDefinition | null | undefined, operation: string): string | null {
  return definition?.approvals?.find((a) => a.operation === operation)?.description ?? null
}

/** routes の行（GET /api/routes、backend/internal/store/routes.go）。 */
export type Route = { label: string; projectId: string | null; harnessId: string | null; createdAt: string; updatedAt: string }

export function decisionListPath(status: DecisionStatus, kind?: DecisionKind): string {
  const params = new URLSearchParams({ status })
  if (kind) params.set('kind', kind)
  return `/api/decisions?${params.toString()}`
}

const decisionPath = (id: string) => `/api/decisions/${encodeURIComponent(id)}`

/**
 * 続行できる判断待ちか。context_conflict と workspace_missing は中止だけ（02 5.6、13 3.4）。
 */
export function canContinue(cause: DecisionCause | null): boolean {
  return cause === 'check_blocked' || cause === 'publish_unknown'
}

/** 取り下げの理由（13 3.4、02 3.4 withdrawn_reason）を文字にする。superseded は kind=route なら Linear のラベル。 */
export function withdrawnReasonLabel(decision: Pick<Decision, 'kind' | 'withdrawnReason'>): string {
  switch (decision.withdrawnReason) {
    case 'cancelled':
      return 'タスクの中止'
    case 'superseded':
      return decision.kind === 'route' ? 'Linear で本人が振り分けのラベルを付けた' : '新しい判断待ちに置き換えた'
    case 'expired':
      return '期限切れ'
    default:
      return '理由の記録がありません'
  }
}

/** kind=blocked・question の種類名を cause まで含めて出す。 */
export function decisionTitle(decision: Pick<Decision, 'kind' | 'cause'>): string {
  const base = DECISION_KIND_LABELS[decision.kind] ?? decision.kind
  return decision.kind === 'blocked' && decision.cause ? `${base}（${decision.cause}）` : base
}

/** detail の文字列項目。無ければ null。 */
export function detailText(detail: Record<string, unknown> | null | undefined, key: string): string | null {
  const value = detail?.[key]
  return typeof value === 'string' ? value : null
}

/** 契約 6.5 の finding。 */
export type Finding = { rule: string; severity: string; message: string; location?: { path: string; line: number } | null }

/** detail.findings を取り出す。配列でなければ空。 */
export function detailFindings(detail: Record<string, unknown> | null | undefined): Finding[] {
  const value = detail?.findings
  return Array.isArray(value) ? (value as Finding[]) : []
}

/** detail.options（question.json の選択肢）。 */
export function detailOptions(detail: Record<string, unknown> | null | undefined): string[] {
  const value = detail?.options
  return Array.isArray(value) ? value.filter((o): o is string => typeof o === 'string') : []
}

/** 結果 JSON の target（不透明な値）から表示用の label と url を取り出す（13 3.4）。 */
export function targetView(target: unknown): { label: string | null; url: string | null } {
  if (target === null || typeof target !== 'object') return { label: null, url: null }
  const record = target as Record<string, unknown>
  return {
    label: typeof record.label === 'string' ? record.label : null,
    url: typeof record.url === 'string' ? record.url : null,
  }
}

export const decisions = {
  /** 新しい順（id の降順）。 */
  list: (api: ApiClient, status: DecisionStatus, kind?: DecisionKind, signal?: AbortSignal) =>
    listAll<Decision>(api, decisionListPath(status, kind), signal),
  get: (api: ApiClient, id: string, signal?: AbortSignal) => api.get<DecisionDetail>(decisionPath(id), { signal }),
  /** 成功は 200 で回答後の行。判断済み・取り下げ済みは 409、basis の不一致は 422（02 5.6）。 */
  answer: (api: ApiClient, id: string, answer: DecisionAnswer) => api.post<Decision>(`${decisionPath(id)}/answer`, answer),
  routes: (api: ApiClient, signal?: AbortSignal) => listAll<Route>(api, '/api/routes', signal),
  /** 操作の Profile の description を引くために、Execution に固定した版を読む。 */
  executionProfileId: async (api: ApiClient, executionId: string, signal?: AbortSignal) =>
    (await api.get<{ profileId: string | null }>(`/api/executions/${encodeURIComponent(executionId)}`, { signal })).profileId,
  profile: (api: ApiClient, profileId: string, signal?: AbortSignal) =>
    api.get<{ id: string; version: string; definition: ProfileDefinition }>(`/api/harness-profiles/${encodeURIComponent(profileId)}`, { signal }),
}
