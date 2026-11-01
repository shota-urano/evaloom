import type { ApiClient } from './client'

// 通知失敗の一覧 API（01-architecture 5.1、12-notification 5「通知失敗の一覧 API（Q41）」、
// backend/internal/api/notification_failures.go）の形。Dashboard のエラー状況はこの API で表示する（13 3.2）。
// 全 Execution の notification_failed を新しい順に、01 5.3 の共通ページネーションで返す。
// limit は送らず、サーバーの既定（01 5.3）に任せる。続きは nextCursor で取る。

export type NotificationFailure = {
  eventId: number
  executionId: string
  taskId: string
  taskTitle: string
  kind: string
  channel: string
  reason: string
  createdAt: string
}

/** 最後のページでは nextCursor が null（12 5）。 */
export type NotificationFailurePage = { items: NotificationFailure[]; nextCursor: string | null }

/** 12 3.4 の reason の説明。 */
export const NOTIFICATION_FAILURE_REASONS: Readonly<Record<string, string>> = {
  permission_denied: '通知が許可されていない',
  launch_failed: '通知ヘルパーを起動できない',
  timeout: '通知ヘルパーの結果待ちが時間切れ',
  other: 'その他の失敗',
}

export function notificationFailuresPath(cursor?: string | null): string {
  return cursor ? `/api/notification-failures?cursor=${encodeURIComponent(cursor)}` : '/api/notification-failures'
}

export const notificationFailures = {
  /** 1ページ分を読む。cursor を渡すと、その続きを読む。 */
  list: (api: ApiClient, cursor?: string | null, signal?: AbortSignal) =>
    api.get<NotificationFailurePage>(notificationFailuresPath(cursor), { signal }),
}
