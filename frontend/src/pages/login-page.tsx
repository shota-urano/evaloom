import { useState, type FormEvent } from 'react'

import { ApiError, extractLoginToken } from '@/api/client'
import { useSession } from '@/auth/session'
import { ApiErrorAlert } from '@/components/api-error-alert'
import { TextField } from '@/components/form-field'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { useApiErrors } from '@/hooks/use-api-errors'

const FIELD = 'loginUrl'
const FIELDS = [FIELD] as const
const MISSING_TOKEN = 'ログイン URL に token が含まれていません。'

/**
 * ログイン画面（01-architecture 3.4）。evaloomd が起動時に出す一回限りの URL を
 * ブラウザで開けばそのままログインできる。貼り付けでもログインできるようにする。
 */
export function LoginPage() {
  const { login } = useSession()
  const [value, setValue] = useState('')
  // 画面側の検証と 401 の文言。サーバーの 400 details は useApiErrors で項目に割り当てる（13 6節）。
  const [localErrors, setLocalErrors] = useState<string[]>([])
  const { error, setError, clear, fieldErrors, unmatched } = useApiErrors(FIELDS)
  const [submitting, setSubmitting] = useState(false)

  // 400 なのに details が1件も無いときは、項目が分からないのでログイン URL の項目に既定の文言を出す。
  const detailless = error instanceof ApiError && error.status === 400 && error.details.length === 0
  const errors = [...localErrors, ...(fieldErrors[FIELD] ?? []), ...(detailless ? [MISSING_TOKEN] : [])]

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    clear()
    if (value.trim() === '') {
      setLocalErrors(['ログイン URL を入力してください'])
      return
    }
    if (extractLoginToken(value) === '') {
      setLocalErrors([MISSING_TOKEN])
      return
    }
    setLocalErrors([])
    setSubmitting(true)
    try {
      await login(value)
    } catch (cause) {
      if (cause instanceof ApiError && cause.status === 401) {
        setLocalErrors(['ログイン URL が無効か、すでに使われています。'])
      } else {
        setError(cause)
      }
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <main className="bg-muted/40 flex min-h-svh items-center justify-center p-6">
      <Card className="w-full max-w-md">
        <CardHeader>
          <CardTitle className="text-xl">Evaloom にログイン</CardTitle>
          <CardDescription>
            evaloomd の起動時にターミナルへ表示された「owner login URL」をこのブラウザで開くか、下に貼り付けてください。URL は一度だけ使えます。
          </CardDescription>
        </CardHeader>
        <CardContent>
          <form className="grid gap-4" noValidate onSubmit={handleSubmit}>
            <ApiErrorAlert error={error} unmatchedDetails={unmatched} />
            <TextField
              name={FIELD}
              label="ログイン URL"
              placeholder="http://127.0.0.1:8080/auth/login?token=…"
              autoComplete="off"
              spellCheck={false}
              value={value}
              onChange={(event) => setValue(event.target.value)}
              errors={errors}
              hint="URL を使い切った場合は、evaloomd を再起動すると新しい URL が表示されます。"
            />
            <Button type="submit" disabled={submitting}>
              {submitting ? 'ログインしています…' : 'ログイン'}
            </Button>
          </form>
        </CardContent>
      </Card>
    </main>
  )
}
