import type { ApiClient } from './client'
import { listAll } from './registry'
import type { GateResult } from './tasks'

// 承認の本人用 API（17-approval 5章、backend/internal/api/approvals.go・approvals.md）の形。

/** 17 3.9 の approvals.status。 */
export type ApprovalStatus = 'pending' | 'approved' | 'rejected' | 'consumed' | 'invalidated' | 'expired'

export const APPROVAL_STATUSES: readonly ApprovalStatus[] = ['pending', 'approved', 'rejected', 'consumed', 'invalidated', 'expired']

/** 17 3.4 の PR マージの target。 */
export type ApprovalTarget = { repository?: string; prNumber?: number; baseBranch?: string; headBranch?: string }

/** GET /api/approvals の要素（approvals の列）。 */
export type Approval = {
  id: string
  taskId: string
  executionId: string
  operation: string
  target: ApprovalTarget
  /** PR の head commit SHA（17 3.4）。 */
  contentVersion: string
  paramsHash: string
  status: ApprovalStatus
  invalidatedReason: string | null
  decidedBy: string | null
  decidedAt: string | null
  expiresAt: string | null
  maxUses: number
  useCount: number
  consumedAt: string | null
  result: unknown
  createdAt: string
}

/** 差分の成果物への参照。本文は GET /api/executions/{id}/artifacts/{artifactId} で読む。 */
export type ApprovalArtifact = { id: string; sha256: string; sizeBytes: number; createdAt: string }

/** GET /api/approvals/{id}。 */
export type ApprovalDetail = Approval & { prUrl: string | null; gateResults: GateResult[]; diffArtifacts: ApprovalArtifact[] }

/** 17 5章の decision の値。 */
export type ApprovalDecision = 'approve' | 'reject'

/** POST /api/approvals/{id}/decision の入力。contentVersion は画面に表示していた値（17 3.4）。ttlSec は送らずサーバーの既定に任せる。 */
export type ApprovalDecisionInput = { decision: ApprovalDecision; contentVersion: string }

export function approvalListPath(status?: ApprovalStatus): string {
  return status ? `/api/approvals?status=${encodeURIComponent(status)}` : '/api/approvals'
}

const approvalPath = (id: string) => `/api/approvals/${encodeURIComponent(id)}`

/**
 * 期限切れとして扱える承認か（再承認を求める操作の対象。backend/internal/api/approvals.md「期限切れの判定」）。
 * status=expired、または status=approved で expires_at を過ぎた未使用のもの。判定の正本はサーバー（対象外は 409）。
 */
export function isExpired(approval: Approval, now: number = Date.now()): boolean {
  if (approval.status === 'expired') return true
  return (
    approval.status === 'approved' &&
    approval.useCount < approval.maxUses &&
    approval.expiresAt !== null &&
    Date.parse(approval.expiresAt) <= now
  )
}

export const approvals = {
  list: (api: ApiClient, status?: ApprovalStatus, signal?: AbortSignal) => listAll<Approval>(api, approvalListPath(status), signal),
  get: (api: ApiClient, id: string, signal?: AbortSignal) => api.get<ApprovalDetail>(approvalPath(id), { signal }),
  /** 成功は 200 で判断後の詳細。判断済み・無効・期限切れは 409、contentVersion の不一致は 422（17 5章）。 */
  decide: (api: ApiClient, id: string, input: ApprovalDecisionInput) => api.post<ApprovalDetail>(`${approvalPath(id)}/decision`, input),
  /** 期限切れの承認から新しい承認待ちを作る（17 3.6）。成功は 201 で新しい承認の詳細。 */
  renew: (api: ApiClient, id: string) => api.post<ApprovalDetail>(`${approvalPath(id)}/renew`),
}
