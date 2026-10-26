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
}

/** API エラーの共通表示（13-management-ui 6節）。error が null/undefined なら何も出さない。 */
export function ApiErrorAlert({ error, onReload, unmatchedDetails = [] }: Props) {
  if (error === null || error === undefined) return null
  const display = describeError(error)
  // 400 の details はフォーム項目に出すので、ここには割り当てられなかった分だけを出す。
  // 422 などの details（パス検証の失敗理由など）はここで一覧する。
  const details = error instanceof ApiError && error.status !== 400 ? error.details : unmatchedDetails
  return (
    <Alert variant={display.action === 'reload' ? 'warning' : 'destructive'} data-testid="api-error">
      <AlertTitle>{display.title}</AlertTitle>
      <AlertDescription>
        <p>{display.description}</p>
        {details.length > 0 ? (
          <ul className="list-disc pl-5">
            {details.map((detail) => (
              <li key={`${detail.field}:${detail.reason}`}>{formatDetail(detail)}</li>
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
