import { SessionProvider, useSession } from '@/auth/session'
import { ApiErrorAlert } from '@/components/api-error-alert'
import { Button } from '@/components/ui/button'
import type { ApiClient } from '@/api/client'
import { LoginPage } from '@/pages/login-page'

function AppShell() {
  return (
    <div className="min-h-svh">
      <header className="border-b">
        <div className="mx-auto flex h-14 max-w-6xl items-center px-6">
          <span className="font-semibold">Evaloom</span>
          <span className="text-muted-foreground ml-3 text-sm">管理画面</span>
        </div>
      </header>
      <main className="mx-auto max-w-6xl p-6">
        <h1 className="text-lg font-semibold">ログインしました</h1>
        <p className="text-muted-foreground mt-1 text-sm">各画面は順に追加されます。</p>
      </main>
    </div>
  )
}

function SessionGate() {
  const { status, error, retry } = useSession()
  if (status === 'loading') {
    return (
      <p role="status" className="text-muted-foreground p-6 text-sm">
        読み込んでいます…
      </p>
    )
  }
  if (status === 'error') {
    return (
      <main className="mx-auto grid max-w-md gap-3 p-6">
        <ApiErrorAlert error={error} />
        <Button type="button" variant="outline" onClick={retry}>
          再試行
        </Button>
      </main>
    )
  }
  if (status === 'unauthenticated') return <LoginPage />
  return <AppShell />
}

export default function App({ api }: { api?: ApiClient }) {
  return (
    <SessionProvider api={api}>
      <SessionGate />
    </SessionProvider>
  )
}
