// 本人用 API のクライアント（01-architecture 3.4・5.1・5.3）。
// - セッションは HttpOnly Cookie なので JavaScript からは触らない（credentials: same-origin）。
// - 状態を変える要求には X-CSRF-Token を付ける。
// - エラーは 5.3 の共通形式 { error: { code, message, details } } を ApiError に変換する。

export type ErrorCode =
  | 'validation_failed'
  | 'unauthenticated'
  | 'forbidden'
  | 'not_found'
  | 'conflict'
  | 'precondition_failed'
  | 'internal'

export type ErrorDetail = { field: string; reason: string }

export class ApiError extends Error {
  readonly status: number
  readonly code: ErrorCode | string
  readonly details: ErrorDetail[]

  constructor(status: number, code: ErrorCode | string, message: string, details: ErrorDetail[] = []) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    this.details = details
  }
}

/** サーバーに届かなかった（evaloomd が止まっている等）ことを表す。 */
export class NetworkError extends Error {
  constructor(cause: unknown) {
    super('network error', { cause })
    this.name = 'NetworkError'
  }
}

export const CSRF_HEADER = 'X-CSRF-Token'

// サーバー（internal/api/auth.go の changesState）と同じく、安全なメソッド以外は
// すべて状態を変える要求として扱う。未知のメソッドにもトークンを付ける。
const SAFE_METHODS = new Set(['GET', 'HEAD', 'OPTIONS'])
export function changesState(method: string): boolean {
  return !SAFE_METHODS.has(method.toUpperCase())
}

const STATUS_CODES: Record<number, ErrorCode> = {
  400: 'validation_failed',
  401: 'unauthenticated',
  403: 'forbidden',
  404: 'not_found',
  409: 'conflict',
  422: 'precondition_failed',
}

function isDetail(value: unknown): value is ErrorDetail {
  return (
    typeof value === 'object' &&
    value !== null &&
    typeof (value as ErrorDetail).field === 'string' &&
    typeof (value as ErrorDetail).reason === 'string'
  )
}

async function toApiError(response: Response): Promise<ApiError> {
  const fallbackCode = STATUS_CODES[response.status] ?? 'internal'
  try {
    const body: unknown = await response.json()
    const error = (body as { error?: unknown } | null)?.error as
      | { code?: unknown; message?: unknown; details?: unknown }
      | undefined
    if (error && typeof error === 'object') {
      const details = Array.isArray(error.details) ? error.details.filter(isDetail) : []
      return new ApiError(
        response.status,
        typeof error.code === 'string' ? error.code : fallbackCode,
        typeof error.message === 'string' ? error.message : response.statusText,
        details,
      )
    }
  } catch {
    // 共通形式でない応答（プロキシのエラー等）は HTTP ステータスから分類する。
  }
  return new ApiError(response.status, fallbackCode, response.statusText)
}

export type RequestOptions = {
  method?: string
  body?: unknown
  headers?: Record<string, string>
  signal?: AbortSignal
}

type FetchLike = (input: string, init: RequestInit) => Promise<Response>

/** SSE の購読（01-architecture 5.4）。ブラウザでは EventSource。テストでは差し替える。 */
export type EventSourceListener = (event: MessageEvent<string>) => void
export type EventSourceLike = {
  addEventListener(type: string, listener: EventSourceListener): void
  removeEventListener(type: string, listener: EventSourceListener): void
  close(): void
}
export type EventSourceFactory = (url: string) => EventSourceLike

export class ApiClient {
  private csrfToken: string | null = null
  private readonly fetchImpl: FetchLike
  private readonly eventSourceImpl: EventSourceFactory
  private readonly unauthorizedListeners = new Set<() => void>()

  constructor(fetchImpl?: FetchLike, eventSourceImpl?: EventSourceFactory) {
    // テストで globalThis.fetch を差し替えられるよう、呼び出し時に解決する。
    this.fetchImpl = fetchImpl ?? ((input, init) => globalThis.fetch(input, init))
    this.eventSourceImpl = eventSourceImpl ?? ((url) => new EventSource(url))
  }

  /**
   * SSE を購読する（GET、同一オリジンなのでセッション Cookie が付く）。
   * 切断時はブラウザの EventSource が自動で再接続し、Last-Event-ID を送って続きから再開する（13 6節）。
   */
  events(path: string): EventSourceLike {
    return this.eventSourceImpl(path)
  }

  get hasCsrfToken(): boolean {
    return this.csrfToken !== null
  }

  /** 401 を受けたときに呼ぶ。ログイン画面へ戻すのに使う。戻り値で解除する。 */
  onUnauthorized(listener: () => void): () => void {
    this.unauthorizedListeners.add(listener)
    return () => this.unauthorizedListeners.delete(listener)
  }

  async request<T>(path: string, options: RequestOptions = {}): Promise<T> {
    const method = (options.method ?? 'GET').toUpperCase()
    const headers: Record<string, string> = { Accept: 'application/json', ...options.headers }
    let body: string | undefined
    if (options.body !== undefined) {
      headers['Content-Type'] = 'application/json'
      body = JSON.stringify(options.body)
    }
    const response = await this.send(path, method, headers, body, options.signal)
    if (response.status === 204) return undefined as T
    const text = await response.text()
    return (text === '' ? undefined : JSON.parse(text)) as T
  }

  /**
   * SSE を fetch で開き、本文を読まずに返す（GET /api/executions/{id}/events）。
   * EventSource と違って Last-Event-ID を自分で付けられるので、再接続の契機と続きの位置を呼び出し側で決められる（13 6節）。
   * 応答が 2xx 以外なら request と同じく ApiError にする（401 ではログインへ戻す）。
   */
  stream(path: string, options: { headers?: Record<string, string>; signal?: AbortSignal } = {}): Promise<Response> {
    return this.send(path, 'GET', { Accept: 'text/event-stream', ...options.headers }, undefined, options.signal)
  }

  private async send(path: string, method: string, headers: Record<string, string>, body: string | undefined, signal?: AbortSignal): Promise<Response> {
    if (changesState(method) && this.csrfToken !== null) {
      headers[CSRF_HEADER] = this.csrfToken
    }
    let response: Response
    try {
      response = await this.fetchImpl(path, {
        method,
        headers,
        body,
        credentials: 'same-origin',
        signal,
      })
    } catch (cause) {
      if (cause instanceof DOMException && cause.name === 'AbortError') throw cause
      throw new NetworkError(cause)
    }
    if (!response.ok) {
      const error = await toApiError(response)
      if (error.status === 401) {
        this.csrfToken = null
        this.unauthorizedListeners.forEach((listener) => listener())
      }
      throw error
    }
    return response
  }

  get<T>(path: string, options?: Omit<RequestOptions, 'method' | 'body'>) {
    return this.request<T>(path, { ...options, method: 'GET' })
  }

  post<T>(path: string, body?: unknown, options?: Omit<RequestOptions, 'method' | 'body'>) {
    return this.request<T>(path, { ...options, method: 'POST', body })
  }

  put<T>(path: string, body?: unknown, options?: Omit<RequestOptions, 'method' | 'body'>) {
    return this.request<T>(path, { ...options, method: 'PUT', body })
  }

  patch<T>(path: string, body?: unknown, options?: Omit<RequestOptions, 'method' | 'body'>) {
    return this.request<T>(path, { ...options, method: 'PATCH', body })
  }

  delete<T>(path: string, options?: Omit<RequestOptions, 'method' | 'body'>) {
    return this.request<T>(path, { ...options, method: 'DELETE' })
  }

  /**
   * 既存のセッション Cookie から CSRF トークンを取得する（GET /api/session）。
   * ログイン URL をブラウザで直接開いた場合はこちらでログイン済みになる。
   * 未ログインなら false。
   */
  async loadSession(): Promise<boolean> {
    try {
      const session = await this.get<{ csrfToken: string }>('/api/session')
      this.csrfToken = session.csrfToken
      return true
    } catch (error) {
      if (error instanceof ApiError && error.status === 401) return false
      throw error
    }
  }

  /**
   * 一回限りのログイン URL（またはそのトークン）でログインする（GET /auth/login?token=）。
   * 成功するとサーバーがセッション Cookie を設定し、CSRF トークンを返す。
   */
  async login(loginUrlOrToken: string): Promise<void> {
    const token = extractLoginToken(loginUrlOrToken)
    if (token === '') throw new ApiError(400, 'validation_failed', 'login URL is empty', [{ field: 'loginUrl', reason: 'required' }])
    const result = await this.get<{ csrfToken: string }>(`/auth/login?token=${encodeURIComponent(token)}`)
    this.csrfToken = result.csrfToken
  }
}

/** 貼り付けられたログイン URL から token を取り出す。URL でなければ入力そのものをトークンとみなす。 */
export function extractLoginToken(input: string): string {
  const trimmed = input.trim()
  if (trimmed === '') return ''
  if (/^https?:\/\//i.test(trimmed) || trimmed.startsWith('/')) {
    try {
      return new URL(trimmed, 'http://127.0.0.1').searchParams.get('token') ?? ''
    } catch {
      return ''
    }
  }
  return trimmed
}

export const api = new ApiClient()
