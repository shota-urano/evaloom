import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from 'react'

import { api as defaultApi, type ApiClient } from '@/api/client'

export type SessionStatus = 'loading' | 'authenticated' | 'unauthenticated' | 'error'

type SessionValue = {
  api: ApiClient
  status: SessionStatus
  /** セッション確認が 401 以外で失敗したときのエラー。 */
  error: unknown
  login: (loginUrlOrToken: string) => Promise<void>
  retry: () => void
}

const SessionContext = createContext<SessionValue | null>(null)

export function SessionProvider({ api = defaultApi, children }: { api?: ApiClient; children: ReactNode }) {
  const [status, setStatus] = useState<SessionStatus>('loading')
  const [error, setError] = useState<unknown>(null)
  const [attempt, setAttempt] = useState(0)

  useEffect(() => {
    let active = true
    api.loadSession().then(
      (authenticated) => {
        if (!active) return
        setError(null)
        setStatus(authenticated ? 'authenticated' : 'unauthenticated')
      },
      (cause: unknown) => {
        if (!active) return
        setError(cause)
        setStatus('error')
      },
    )
    return () => {
      active = false
    }
  }, [api, attempt])

  // どの画面の要求でも 401 が返ったらログイン画面へ戻す（13 6節）。
  useEffect(() => api.onUnauthorized(() => setStatus('unauthenticated')), [api])

  const login = useCallback(
    async (input: string) => {
      await api.login(input)
      setError(null)
      setStatus('authenticated')
    },
    [api],
  )
  const retry = useCallback(() => {
    setStatus('loading')
    setAttempt((n) => n + 1)
  }, [])

  const value = useMemo(() => ({ api, status, error, login, retry }), [api, status, error, login, retry])
  return <SessionContext.Provider value={value}>{children}</SessionContext.Provider>
}

// oxlint-disable-next-line react/only-export-components
export function useSession(): SessionValue {
  const value = useContext(SessionContext)
  if (value === null) throw new Error('useSession must be used inside SessionProvider')
  return value
}
