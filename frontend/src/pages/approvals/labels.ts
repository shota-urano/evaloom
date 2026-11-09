import type { Approval } from '@/api/approvals'

export const APPROVALS_PATH = '/approvals'

export function approvalDetailPath(id: string) {
  return `${APPROVALS_PATH}/${encodeURIComponent(id)}`
}

/**
 * 対象の PR の短い表記（17 3.4 の target）。例: owner/repo#12。
 * target に値が無いときは operation をそのまま出す。
 */
export function approvalTargetLabel(approval: Pick<Approval, 'operation' | 'target'>): string {
  const { repository, prNumber } = approval.target ?? {}
  if (repository && prNumber) return `${repository}#${prNumber}`
  return approval.operation
}

/** 日時の表示。値が無ければ「-」。 */
export function formatTime(value: string | null | undefined): string {
  return value ? new Date(value).toLocaleString('ja-JP') : '-'
}
