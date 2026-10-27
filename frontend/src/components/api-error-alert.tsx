import { describeError, formatDetail } from '@/api/errors'
import { ApiError, type ErrorDetail } from '@/api/client'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'

type Props = {
  error: unknown
  /** 409 のとき「最新を読み直す」ボタンで呼ぶ。 */
  onReload?: () => void
  /** フォームの項目に割り当てられなかった details。 */
  unmatchedDetails?: readonly ErrorDetail[]
  /** details[].field を画面の項目名で出すための対応表。 */
  fieldLabels?: Readonly<Record<string, string>>
  /** 画面の文脈で説明を言い換えるとき（409 の内容など）に使う。無ければ状態コードごとの既定の説明。 */
  description?: string
}

/** API エラーの共通表示（13-management-ui 6節）。error が null/undefined なら何も出さない。 */
export function ApiErrorAlert({ error, onReload, unmatchedDetails = [], fieldLabels, description }: Props) {
  if (error === null || error === undefined) return null
  const display = describeError(error)
  // 400 の details はフォーム項目に出すので、ここには割り当てられなかった分だけを出す。
  // 422 などの details（パス検証の失敗理由など）はここで一覧する。
  const details = error instanceof ApiError && error.status !== 400 ? error.details : unmatchedDetails
  return (
    <Alert variant={display.action === 'reload' ? 'warning' : 'destructive'} data-testid="api-error">
      <AlertTitle>{display.title}</AlertTitle>
      <AlertDescription>
        <p>{description ?? display.description}</p>
        {details.length > 0 ? (
          <ul className="list-disc pl-5">
            {details.map((detail) => (
              <li key={`${detail.field}:${detail.reason}`}>{formatDetail(detail, fieldLabels)}</li>
            ))}
          </ul>
        ) : null}
        {display.action === 'reload' && onReload ? (
          <Button type="button" variant="outline" size="sm" className="mt-2" onClick={onReload}>
            最新を読み直す
          </Button>
        ) : null}
      </AlertDescription>
    </Alert>
  )
}
