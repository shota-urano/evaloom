import { useEffect, useState } from 'react'

// 画面遷移の最小の仕組み（13 3.1「ルーティングのライブラリは実装時に確定する」）。
// evaloomd は拡張子の無いパスに index.html を返す（internal/api/web.go）ので、History API のパスを使う。

const EVENT = 'evaloom:navigate'

/** 画面遷移時に渡す一度きりの値（保存直後の警告など）。history.state に入れる。 */
export type NavigationState = { warnings?: string[]; notice?: string } | null

export function navigate(path: string, state: NavigationState = null) {
  window.history.pushState(state, '', path)
  window.dispatchEvent(new Event(EVENT))
}

export function useLocation(): { path: string; state: NavigationState } {
  const read = () => ({ path: window.location.pathname, state: (window.history.state ?? null) as NavigationState })
  const [location, setLocation] = useState(read)
  useEffect(() => {
    const update = () => setLocation(read())
    window.addEventListener('popstate', update)
    window.addEventListener(EVENT, update)
    return () => {
      window.removeEventListener('popstate', update)
      window.removeEventListener(EVENT, update)
    }
  }, [])
  return location
}

/** pattern の `:name` を取り出す。一致しなければ null。 */
export function matchPath(pattern: string, path: string): Record<string, string> | null {
  const want = pattern.split('/').filter(Boolean)
  const got = path.split('/').filter(Boolean)
  if (want.length !== got.length) return null
  const params: Record<string, string> = {}
  for (let i = 0; i < want.length; i++) {
    if (want[i].startsWith(':')) {
      params[want[i].slice(1)] = decodeURIComponent(got[i])
    } else if (want[i] !== got[i]) {
      return null
    }
  }
  return params
}
