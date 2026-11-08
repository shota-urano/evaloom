import type { ApiClient } from './client'

// 通知失敗の一覧 API（01-architecture 5.1、12-notification 5「通知失敗の一覧 API（Q41）」、
// backend/internal/api/notification_failures.go）の形。Dashboard のエラー状況はこの API で表示する（13 3.2）。
// 全 Execution の notification_failed と、Execution の無い契機の Linear コメントの送信の失敗
// （linear_outbox の行、16 3.4.2、Q82）を、記録した時刻の新しい順に、01 5.3 の共通ページネーションで返す。
// limit は送らず、サーバーの既定（01 5.3）に任せる。続きは nextCursor で取る。

/**
 * 一覧の1項目。execution_events から返す項目は eventId と executionId が入り outboxId が null、
 * linear_outbox から返す項目は eventId と executionId が null で outboxId が入る（12 5、Q82）。
 */
export type NotificationFailure = {
  eventId: number | null
  executionId: string | null
  outboxId?: string | null
  taskId: string
  taskTitle: string
  kind: string
  channel: string
  reason: string
  createdAt: string
}

/** 最後のページでは nextCursor が null（12 5）。 */
export type NotificationFailurePage = { items: NotificationFailure[]; nextCursor: string | null }

/** 12 3.4 の reason の説明（macOS 通知のヘルパーの失敗）。 */
export const NOTIFICATION_FAILURE_REASONS: Readonly<Record<string, string>> = {
  permission_denied: '通知が許可されていない',
  launch_failed: '通知ヘルパーを起動できない',
  timeout: '通知ヘルパーの結果待ちが時間切れ',
  other: 'その他の失敗',
}

/** channel=linear の reason の説明（16 3.4.2 の表、Q85）。12 3.4 の分類は Linear の失敗には使わない。 */
export const LINEAR_NOTIFICATION_FAILURE_REASONS: Readonly<Record<string, string>> = {
  auth: 'Linear API の認証エラー',
  issue_removed: 'Issue が削除・アーカイブされていた',
  retries_exhausted: '一時的な失敗が続き、5回目も失敗した',
  other: 'その他の失敗',
}

/** 項目の reason の説明。channel で表を選ぶ。どの表にも無い値は undefined。 */
export function notificationFailureReasonLabel(failure: Pick<NotificationFailure, 'channel' | 'reason'>): string | undefined {
  const table = failure.channel === 'linear' ? LINEAR_NOTIFICATION_FAILURE_REASONS : NOTIFICATION_FAILURE_REASONS
  return Object.hasOwn(table, failure.reason) ? table[failure.reason] : undefined
}

/**
 * 一覧の中で項目を見分けるキー。eventId と outboxId は出所ごとに別の ID のため、出所を前に付ける
 * （linear_outbox の項目は eventId が null なので、eventId だけでは見分けられない。13 3.2）。
 */
export function notificationFailureKey(failure: NotificationFailure): string {
  return failure.outboxId != null ? `outbox:${failure.outboxId}` : `event:${failure.eventId}`
}

export function notificationFailuresPath(cursor?: string | null): string {
  return cursor ? `/api/notification-failures?cursor=${encodeURIComponent(cursor)}` : '/api/notification-failures'
}

export const notificationFailures = {
  /** 1ページ分を読む。cursor を渡すと、その続きを読む。 */
  list: (api: ApiClient, cursor?: string | null, signal?: AbortSignal) =>
    api.get<NotificationFailurePage>(notificationFailuresPath(cursor), { signal }),
}
