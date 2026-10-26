import { ApiError, NetworkError, type ErrorDetail } from './client'

// 13-management-ui 6節「エラー処理」の表示内容。

export type ErrorDisplay = {
  title: string
  description: string
  /** 409 は最新を読み直させる。401 はログインへ戻す。 */
  action?: 'reload' | 'login'
}

const REASON_LABELS: Record<string, string> = {
  required: '入力してください',
  invalid: '値が正しくありません',
  invalid_format: '形式が正しくありません',
  invalid_json: '送信内容の形式が正しくありません',
  too_long: '長すぎます',
  not_absolute: '絶対パスで入力してください',
  not_found: '見つかりません',
  // 03-registry のパス検証（internal/registry）で使われる理由
  within_worktree_root: 'Runner の worktree 置き場の中は指定できません',
  not_git_repository: 'Git リポジトリではありません',
  remote_mismatch: 'Git URL がリポジトリの remote と一致しません',
}

/** details の reason（機械向けの語）を画面の文言にする。未知の語は原文を添えて出す。 */
export function reasonLabel(reason: string): string {
  return REASON_LABELS[reason] ?? `値が正しくありません（${reason}）`
}

export function formatDetail(detail: ErrorDetail): string {
  return `${detail.field}: ${reasonLabel(detail.reason)}`
}

export function describeError(error: unknown): ErrorDisplay {
  if (error instanceof NetworkError) {
    return { title: 'evaloomd に接続できません', description: 'evaloomd が起動しているか確認してください。' }
  }
  if (!(error instanceof ApiError)) {
    return { title: '予期しないエラーが発生しました', description: error instanceof Error ? error.message : String(error) }
  }
  switch (error.status) {
    case 400:
      return { title: '入力内容を確認してください', description: '正しくない項目があります。' }
    case 401:
      return {
        title: 'ログインが必要です',
        description: 'evaloomd の起動時にターミナルへ表示されたログイン URL を開いてください。',
        action: 'login',
      }
    case 403:
      return {
        title: 'この操作は実行できません',
        description: '権限が無いか、CSRF トークンの確認に失敗しました。画面を読み直してから、もう一度操作してください。',
      }
    case 404:
      return { title: '対象が見つかりません', description: '削除されたか、ID が正しくありません。' }
    case 409:
      return {
        title: '状態が変わりました',
        description: 'ほかの操作で内容が変わったため、この操作は受け付けられませんでした。最新の内容を読み直してください。',
        action: 'reload',
      }
    case 422:
      return {
        title: '前提条件が満たされていません',
        description: error.message || '操作の前提となる条件が満たされていません。',
      }
    default:
      return { title: 'サーバーでエラーが発生しました', description: '時間をおいて、もう一度操作してください。' }
  }
}

export type FieldErrors = Record<string, string[]>

/**
 * 400 validation_failed の details をフォームの項目ごとの文言に分ける。
 * fields に無い項目の detail は unmatched に入れ、フォーム上部にまとめて出す。
 */
export function splitFieldErrors(
  error: unknown,
  fields: readonly string[],
): { fieldErrors: FieldErrors; unmatched: ErrorDetail[] } {
  const fieldErrors: FieldErrors = {}
  const unmatched: ErrorDetail[] = []
  if (!(error instanceof ApiError) || error.status !== 400) return { fieldErrors, unmatched }
  for (const detail of error.details) {
    if (fields.includes(detail.field)) {
      ;(fieldErrors[detail.field] ??= []).push(reasonLabel(detail.reason))
    } else {
      unmatched.push(detail)
    }
  }
  return { fieldErrors, unmatched }
}
