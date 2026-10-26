import { useState, type FormEvent } from 'react'

import { ApiError } from '@/api/client'
import { useSession } from '@/auth/session'
import { ApiErrorAlert } from '@/components/api-error-alert'
import { TextField } from '@/components/form-field'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'

const FIELD = 'loginUrl'

/**
 * ログイン画面（01-architecture 3.4）。evaloomd が起動時に出す一回限りの URL を
 * ブラウザで開けばそのままログインできる。貼り付けでもログインできるようにする。
 */
export function LoginPage() {
  const { login } = useSession()
  const [value, setValue] = useState('')
  const [fieldErrors, setFieldErrors] = useState<string[]>([])
  const [error, setError] = useState<unknown>(null)
  const [submitting, setSubmitting] = useState(false)

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    setError(null)
    if (value.trim() === '') {
      setFieldErrors(['ログイン URL を入力してください'])
      return
    }
    setFieldErrors([])
    setSubmitting(true)
    try {
      await login(value)
    } catch (cause) {
      if (cause instanceof ApiError && cause.status === 401) {
        setFieldErrors(['ログイン URL が無効か、すでに使われています。'])
      } else if (cause instanceof ApiError && cause.status === 400) {
        setFieldErrors(['ログイン URL に token が含まれていません。'])
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
            <ApiErrorAlert error={error} />
            <TextField
              name={FIELD}
              label="ログイン URL"
              placeholder="http://127.0.0.1:8080/auth/login?token=…"
              autoComplete="off"
              spellCheck={false}
              value={value}
              onChange={(event) => setValue(event.target.value)}
              errors={fieldErrors}
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
