import { describe, expect, it, vi } from 'vitest'

import { apiError, fakeFetch, json } from '@/test/fake-fetch'
import { ApiClient, ApiError, CSRF_HEADER, NetworkError, extractLoginToken } from './client'

const TOKEN = 'csrf-token-123'

async function loggedInClient() {
  const fake = fakeFetch((request) => {
    if (request.url === '/api/session') return json(200, { csrfToken: TOKEN })
    return json(200, { ok: true })
  })
  const client = new ApiClient(fake.fetch)
  expect(await client.loadSession()).toBe(true)
  fake.calls.length = 0
  return { client, calls: fake.calls }
}

describe('X-CSRF-Token（01-architecture 3.4）', () => {
  it.each(['POST', 'PUT', 'PATCH', 'DELETE'])('状態を変える %s に X-CSRF-Token を付ける', async (method) => {
    const { client, calls } = await loggedInClient()
    await client.request('/api/projects', { method, body: { name: 'x' } })
    expect(calls).toHaveLength(1)
    expect(calls[0].method).toBe(method)
    expect(calls[0].headers[CSRF_HEADER]).toBe(TOKEN)
  })

  it('post/patch/delete の各ヘルパーでも付く', async () => {
    const { client, calls } = await loggedInClient()
    await client.post('/api/tasks', { title: 't' })
    await client.patch('/api/projects/p1', { name: 'n' })
    await client.delete('/api/projects/p1')
    expect(calls.map((c) => [c.method, c.headers[CSRF_HEADER]])).toEqual([
      ['POST', TOKEN],
      ['PATCH', TOKEN],
      ['DELETE', TOKEN],
    ])
  })

  it.each(['GET', 'HEAD'])('%s には付けない', async (method) => {
    const { client, calls } = await loggedInClient()
    await client.request('/api/projects', { method })
    expect(calls[0].headers).not.toHaveProperty(CSRF_HEADER)
  })

  it('ログイン URL でのログイン後、応答の csrfToken を使う', async () => {
    const fake = fakeFetch((request) => {
      if (request.url.startsWith('/auth/login')) return json(200, { csrfToken: 'from-login' })
      return json(201, { id: 'task-1' })
    })
    const client = new ApiClient(fake.fetch)
    await client.login('http://127.0.0.1:8080/auth/login?token=abc%2Bdef')
    await client.post('/api/tasks', {})
    expect(fake.calls[0].url).toBe('/auth/login?token=abc%2Bdef')
    expect(fake.calls[0].method).toBe('GET')
    expect(fake.calls[1].headers[CSRF_HEADER]).toBe('from-login')
  })

  it('セッション Cookie を送るため credentials は same-origin、JSON 本文に Content-Type を付ける', async () => {
    const { client, calls } = await loggedInClient()
    await client.post('/api/tasks', { acceptanceCriteria: 'ok' })
    expect(calls[0].credentials).toBe('same-origin')
    expect(calls[0].headers['Content-Type']).toBe('application/json')
    expect(JSON.parse(calls[0].body ?? '')).toEqual({ acceptanceCriteria: 'ok' })
  })

  it('401 を受けたらトークンを捨てて通知し、以後の POST に古いトークンを付けない', async () => {
    let expired = false
    const fake = fakeFetch((request) => {
      if (request.url === '/api/session') return json(200, { csrfToken: TOKEN })
      return expired ? apiError(401, 'unauthenticated', 'owner session is missing or invalid') : json(200, {})
    })
    const client = new ApiClient(fake.fetch)
    await client.loadSession()
    const listener = vi.fn()
    client.onUnauthorized(listener)
    expired = true
    await expect(client.post('/api/tasks', {})).rejects.toMatchObject({ status: 401, code: 'unauthenticated' })
    expect(fake.calls.at(-1)?.headers[CSRF_HEADER]).toBe(TOKEN)
    expect(listener).toHaveBeenCalledTimes(1)
    expect(client.hasCsrfToken).toBe(false)
    await expect(client.post('/api/tasks', {})).rejects.toBeInstanceOf(ApiError)
    expect(fake.calls.at(-1)?.headers).not.toHaveProperty(CSRF_HEADER)
  })
})

describe('エラー形式（01-architecture 5.3）', () => {
  it('400 validation_failed の details を保持する', async () => {
    const client = new ApiClient(
      fakeFetch(() =>
        apiError(400, 'validation_failed', 'invalid request', [
          { field: 'acceptanceCriteria', reason: 'required' },
          { field: 'title', reason: 'too_long' },
        ]),
      ).fetch,
    )
    const error = await client.post('/api/tasks', {}).catch((e: unknown) => e)
    expect(error).toBeInstanceOf(ApiError)
    expect(error).toMatchObject({
      status: 400,
      code: 'validation_failed',
      message: 'invalid request',
      details: [
        { field: 'acceptanceCriteria', reason: 'required' },
        { field: 'title', reason: 'too_long' },
      ],
    })
  })

  it('共通形式でない応答はステータスから分類する', async () => {
    const client = new ApiClient(fakeFetch(() => new Response('bad gateway', { status: 409 })).fetch)
    await expect(client.get('/api/tasks')).rejects.toMatchObject({ status: 409, code: 'conflict', details: [] })
  })

  it('接続できないときは NetworkError', async () => {
    const client = new ApiClient(async () => {
      throw new TypeError('Failed to fetch')
    })
    await expect(client.get('/api/tasks')).rejects.toBeInstanceOf(NetworkError)
  })

  it('loadSession は 401 なら false、それ以外のエラーは投げる', async () => {
    expect(await new ApiClient(fakeFetch(() => apiError(401, 'unauthenticated', 'x')).fetch).loadSession()).toBe(false)
    await expect(new ApiClient(fakeFetch(() => apiError(500, 'internal', 'x')).fetch).loadSession()).rejects.toMatchObject({
      status: 500,
    })
  })
})

describe('extractLoginToken', () => {
  it.each([
    ['http://127.0.0.1:8080/auth/login?token=abc', 'abc'],
    ['  http://127.0.0.1:8080/auth/login?token=a-b_c  ', 'a-b_c'],
    ['/auth/login?token=xyz', 'xyz'],
    ['raw-token', 'raw-token'],
    ['http://127.0.0.1:8080/auth/login', ''],
    ['', ''],
  ])('%s → %s', (input, want) => {
    expect(extractLoginToken(input)).toBe(want)
  })
})
