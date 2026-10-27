import { useCallback, useEffect, useRef, useState } from 'react'

export type Resource<T> = {
  data: T | undefined
  error: unknown
  loading: boolean
  /** 409 の「最新を読み直す」や保存後の再取得に使う。 */
  reload: () => void
}

/**
 * 画面表示時に API から読む。key が変わるか reload を呼ぶと読み直す。
 * 画面を離れたら要求を中止し、古い応答で表示を上書きしない。読み直し中も前の data は残す。
 */
export function useResource<T>(load: (signal: AbortSignal) => Promise<T>, key: string): Resource<T> {
  const [attempt, setAttempt] = useState(0)
  const request = `${key}#${attempt}`
  const [state, setState] = useState<{ request: string; data?: T; error: unknown }>({ request: '', error: null })
  // load は呼び出し側で毎回作られるので、読み直しの契機は key と reload だけにする。
  const loadRef = useRef(load)
  useEffect(() => {
    loadRef.current = load
  })
  useEffect(() => {
    const controller = new AbortController()
    loadRef.current(controller.signal).then(
      (data) => {
        if (!controller.signal.aborted) setState({ request, data, error: null })
      },
      (error: unknown) => {
        if (!controller.signal.aborted) setState((previous) => ({ ...previous, request, error }))
      },
    )
    return () => controller.abort()
  }, [request])
  const reload = useCallback(() => setAttempt((n) => n + 1), [])
  return { data: state.data, error: state.error, loading: state.request !== request, reload }
}
