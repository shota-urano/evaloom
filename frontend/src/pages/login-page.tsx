import { useState, type FormEvent } from 'react'

import { ApiError, isPasswordNotSet } from '@/api/client'
import { useSession } from '@/auth/session'
import { ApiErrorAlert } from '@/components/api-error-alert'
import { TextField } from '@/components/form-field'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { useApiErrors } from '@/hooks/use-api-errors'

const FIELDS = ['id', 'password'] as const
const FIELD_LABELS = { id: 'ID', password: 'パスワード' }

/** パスワードを設定する CLI（18 3.3・5章）。 */
export const SET_PASSWORD_COMMAND = 'evaloomd -db <DB> owner set-password -id <ID>'

type Failure = 'invalid_credentials' | 'password_not_set' | 'cooldown' | null

/**
 * ログイン画面（18 3.4）。本人の ID とパスワードで POST /auth/login を呼ぶ。
 * どの画面にいてもセッションが無ければこの画面になり、ログインすると URL の画面がそのまま出る。
 */
export function LoginPage() {
  const { login } = useSession()
  const [id, setId] = useState('')
  const [password, setPassword] = useState('')
  const [localErrors, setLocalErrors] = useState<Partial<Record<(typeof FIELDS)[number], string[]>>>({})
  const [failure, setFailure] = useState<Failure>(null)
  const { error, setError, clear, fieldErrors, unmatched } = useApiErrors(FIELDS)
  const [submitting, setSubmitting] = useState(false)

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    clear()
    setFailure(null)
    const missing: typeof localErrors = {}
    if (id.trim() === '') missing.id = ['ID を入力してください']
    if (password === '') missing.password = ['パスワードを入力してください']
    setLocalErrors(missing)
    if (Object.keys(missing).length > 0) return
    setSubmitting(true)
    try {
      await login(id, password)
    } catch (cause) {
      if (isPasswordNotSet(cause)) {
        setFailure('password_not_set')
      } else if (cause instanceof ApiError && cause.status === 401) {
        // どちらが違うかはサーバーも返さない（18 3.4）。
        setFailure('invalid_credentials')
        setPassword('')
      } else if (cause instanceof ApiError && cause.status === 429) {
        setFailure('cooldown')
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
          <CardDescription>本人の ID とパスワードを入力してください。</CardDescription>
        </CardHeader>
        <CardContent>
          <form className="grid gap-4" noValidate onSubmit={handleSubmit}>
            <ApiErrorAlert error={error} unmatchedDetails={unmatched} fieldLabels={FIELD_LABELS} />
            {failure === 'invalid_credentials' ? (
              <Alert variant="destructive">
                <AlertTitle>ID かパスワードが違います</AlertTitle>
                <AlertDescription>
                  <p>入力した内容を確かめて、もう一度ログインしてください。</p>
                </AlertDescription>
              </Alert>
            ) : null}
            {failure === 'cooldown' ? (
              <Alert variant="destructive">
                <AlertTitle>しばらくログインできません</AlertTitle>
                <AlertDescription>
                  <p>続けてログインに失敗したため、一時的にログインを受け付けていません。1分ほど待ってから、もう一度ログインしてください。</p>
                </AlertDescription>
              </Alert>
            ) : null}
            {failure === 'password_not_set' ? (
              <Alert variant="warning">
                <AlertTitle>パスワードが設定されていません</AlertTitle>
                <AlertDescription>
                  <p>Mac mini のターミナルで次のコマンドを実行して、ID とパスワードを設定してください。設定した後で、この画面からログインできます。</p>
                  <code className="bg-muted rounded px-2 py-1 font-mono text-xs">{SET_PASSWORD_COMMAND}</code>
                  <p>パスワードは画面に表示されずに2回たずねられます。管理画面からは設定できません。</p>
                </AlertDescription>
              </Alert>
            ) : null}
            <TextField
              name="id"
              label="ID"
              autoComplete="username"
              autoCapitalize="none"
              spellCheck={false}
              value={id}
              onChange={(event) => setId(event.target.value)}
              errors={[...(localErrors.id ?? []), ...(fieldErrors.id ?? [])]}
            />
            <TextField
              name="password"
              label="パスワード"
              type="password"
              autoComplete="current-password"
              value={password}
              onChange={(event) => setPassword(event.target.value)}
              errors={[...(localErrors.password ?? []), ...(fieldErrors.password ?? [])]}
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
