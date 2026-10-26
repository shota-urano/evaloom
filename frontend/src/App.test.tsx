import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'

import { ApiClient, CSRF_HEADER } from '@/api/client'
import { apiError, fakeFetch, json } from '@/test/fake-fetch'
import App from './App'

describe('ログイン（01-architecture 3.4）', () => {
  it('セッションが無ければログイン画面を出す', async () => {
    const fake = fakeFetch(() => apiError(401, 'unauthenticated', 'owner session is missing or invalid'))
    render(<App api={new ApiClient(fake.fetch)} />)
    expect(await screen.findByText('Evaloom にログイン')).toBeVisible()
    expect(fake.calls[0]).toMatchObject({ url: '/api/session', method: 'GET' })
  })

  it('ログイン URL をブラウザで開いて Cookie があれば、そのままログイン済みになる', async () => {
    const fake = fakeFetch(() => json(200, { csrfToken: 'session-token' }))
    render(<App api={new ApiClient(fake.fetch)} />)
    expect(await screen.findByText('ログインしました')).toBeVisible()
  })

  it('貼り付けたログイン URL でログインし、以後の状態変更要求にそのトークンを付ける', async () => {
    const fake = fakeFetch((request) => {
      if (request.url === '/api/session') return apiError(401, 'unauthenticated', 'x')
      if (request.url === '/auth/login?token=one-time') return json(200, { csrfToken: 'issued' })
      return json(200, {})
    })
    const api = new ApiClient(fake.fetch)
    render(<App api={api} />)
    await userEvent.type(await screen.findByLabelText('ログイン URL'), 'http://127.0.0.1:8080/auth/login?token=one-time')
    await userEvent.click(screen.getByRole('button', { name: 'ログイン' }))
    expect(await screen.findByText('ログインしました')).toBeVisible()
    await api.post('/api/tasks', {})
    expect(fake.calls.at(-1)?.headers[CSRF_HEADER]).toBe('issued')
  })

  it('使用済みのログイン URL はログイン URL の項目にエラーを出す', async () => {
    const fake = fakeFetch((request) =>
      request.url === '/api/session'
        ? apiError(401, 'unauthenticated', 'x')
        : apiError(401, 'unauthenticated', 'login URL is invalid or already used'),
    )
    render(<App api={new ApiClient(fake.fetch)} />)
    const input = await screen.findByLabelText('ログイン URL')
    await userEvent.type(input, 'http://127.0.0.1:8080/auth/login?token=used')
    await userEvent.click(screen.getByRole('button', { name: 'ログイン' }))
    expect(await screen.findByText('ログイン URL が無効か、すでに使われています。')).toBeVisible()
    expect(input).toHaveAttribute('aria-invalid', 'true')
  })

  it('空のまま送信すると要求を送らずに項目エラーを出す', async () => {
    const fake = fakeFetch(() => apiError(401, 'unauthenticated', 'x'))
    render(<App api={new ApiClient(fake.fetch)} />)
    await userEvent.click(await screen.findByRole('button', { name: 'ログイン' }))
    expect(screen.getByText('ログイン URL を入力してください')).toBeVisible()
    expect(fake.calls).toHaveLength(1)
  })

  it('ログイン後に API が 401 を返したらログイン画面へ戻る', async () => {
    let expired = false
    const fake = fakeFetch(() => (expired ? apiError(401, 'unauthenticated', 'x') : json(200, { csrfToken: 't' })))
    const api = new ApiClient(fake.fetch)
    render(<App api={api} />)
    expect(await screen.findByText('ログインしました')).toBeVisible()
    expired = true
    await api.get('/api/tasks').catch(() => undefined)
    expect(await screen.findByLabelText('ログイン URL')).toBeVisible()
  })

  it('evaloomd に接続できなければ、その旨と再試行を出す', async () => {
    const api = new ApiClient(async () => {
      throw new TypeError('Failed to fetch')
    })
    render(<App api={api} />)
    expect(await screen.findByRole('alert')).toHaveTextContent('evaloomd に接続できません')
    expect(screen.getByRole('button', { name: '再試行' })).toBeVisible()
  })
})

describe('ログイン画面でもサーバーの 400 details を項目に表示する（13 6節）', () => {
  function loginFails(response: () => Response) {
    return fakeFetch((request) => (request.url === '/api/session' ? apiError(401, 'unauthenticated', 'x') : response()))
  }

  async function submit(fake: ReturnType<typeof fakeFetch>) {
    render(<App api={new ApiClient(fake.fetch)} />)
    const input = await screen.findByLabelText('ログイン URL')
    await userEvent.type(input, 'http://127.0.0.1:8080/auth/login?token=abc')
    await userEvent.click(screen.getByRole('button', { name: 'ログイン' }))
    return input
  }

  it('details の loginUrl（too_long）をログイン URL の項目に出し、固定文言に置き換えない', async () => {
    const fake = loginFails(() =>
      apiError(400, 'validation_failed', 'invalid request', [{ field: 'loginUrl', reason: 'too_long' }]),
    )
    const input = await submit(fake)
    expect(await screen.findByText('長すぎます')).toBeVisible()
    expect(input).toHaveAttribute('aria-invalid', 'true')
    expect(input).toHaveAccessibleDescription(expect.stringContaining('長すぎます'))
    expect(screen.queryByText('ログイン URL に token が含まれていません。')).toBeNull()
    expect(screen.getByRole('alert')).toHaveTextContent('入力内容を確認してください')
  })

  it('フォームに無い項目の details は上部の一覧に出す', async () => {
    const fake = loginFails(() => apiError(400, 'validation_failed', 'invalid request', [{ field: 'token', reason: 'invalid' }]))
    const input = await submit(fake)
    expect(await screen.findByText('token: 値が正しくありません')).toBeVisible()
    expect(input).not.toHaveAttribute('aria-invalid')
  })

  it('details の無い 400 はログイン URL の項目に既定の文言を出す', async () => {
    const fake = loginFails(() => apiError(400, 'validation_failed', 'invalid request'))
    const input = await submit(fake)
    expect(await screen.findByText('ログイン URL に token が含まれていません。')).toBeVisible()
    expect(input).toHaveAttribute('aria-invalid', 'true')
  })

  it('token の無い URL は送らずに項目エラーを出す', async () => {
    const fake = loginFails(() => json(200, {}))
    render(<App api={new ApiClient(fake.fetch)} />)
    await userEvent.type(await screen.findByLabelText('ログイン URL'), 'http://127.0.0.1:8080/auth/login')
    await userEvent.click(screen.getByRole('button', { name: 'ログイン' }))
    expect(await screen.findByText('ログイン URL に token が含まれていません。')).toBeVisible()
    expect(fake.calls).toHaveLength(1)
  })
})
