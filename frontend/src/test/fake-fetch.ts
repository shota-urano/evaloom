import { vi } from 'vitest'

export type Recorded = { url: string; method: string; headers: Record<string, string>; body?: string; credentials?: RequestCredentials }

export function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

export function apiError(status: number, code: string, message: string, details: { field: string; reason: string }[] = []) {
  return json(status, { error: { code, message, details } })
}

/** 要求を記録し、handler の返す応答を返す fetch。 */
export function fakeFetch(handler: (request: Recorded) => Response | Promise<Response>) {
  const calls: Recorded[] = []
  const fetch = vi.fn(async (input: string, init: RequestInit) => {
    const request: Recorded = {
      url: input,
      method: init.method ?? 'GET',
      headers: { ...(init.headers as Record<string, string>) },
      body: typeof init.body === 'string' ? init.body : undefined,
      credentials: init.credentials,
    }
    calls.push(request)
    return handler(request)
  })
  return { fetch, calls }
}
