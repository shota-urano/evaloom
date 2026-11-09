import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it } from 'vitest'

import { ApiClient, CSRF_HEADER } from '@/api/client'
import { apiError, fakeFetch, json, type Recorded } from '@/test/fake-fetch'
import App from './App'

afterEach(() => {
  window.history.replaceState(null, '', '/')
})

const EMPTY_PAGE = { items: [], nextCursor: '' }

const NOT_LOGGED_IN = () => apiError(401, 'unauthenticated', 'owner session is missing or invalid')

/** /api/session は 401、/auth/login は login() の応答を返す。 */
function loginServer(login: (request: Recorded) => Response, other: (request: Recorded) => Response = () => json(200, {})) {
  return fakeFetch((request) => {
    if (request.url === '/api/session') return NOT_LOGGED_IN()
    if (request.url === '/auth/login') return login(request)
    return other(request)
  })
}

async function fillAndSubmit(id: string, password: string) {
  const idInput = await screen.findByLabelText('ID')
  if (id !== '') await userEvent.type(idInput, id)
  if (password !== '') await userEvent.type(screen.getByLabelText('パスワード'), password)
  await userEvent.click(screen.getByRole('button', { name: 'ログイン' }))
}

describe('ログイン（18 3.4）', () => {
  it('セッションが無ければ ID とパスワードのログイン画面を出す', async () => {
    const fake = fakeFetch(NOT_LOGGED_IN)
    render(<App api={new ApiClient(fake.fetch)} />)
    expect(await screen.findByText('Evaloom にログイン')).toBeVisible()
    expect(screen.getByLabelText('ID')).toBeVisible()
    expect(screen.getByLabelText('パスワード')).toHaveAttribute('type', 'password')
    expect(fake.calls[0]).toMatchObject({ url: '/api/session', method: 'GET' })
  })

  it('セッション Cookie があれば、そのままログイン済みになる', async () => {
    const fake = fakeFetch(() => json(200, { csrfToken: 'session-token' }))
    render(<App api={new ApiClient(fake.fetch)} />)
    expect(await screen.findByRole('heading', { name: 'Dashboard' })).toBeVisible()
  })

  it('ID とパスワードでログインすると Dashboard が出て、以後の状態変更要求にそのトークンを付ける', async () => {
    const fake = loginServer(() => json(200, { csrfToken: 'issued' }))
    const api = new ApiClient(fake.fetch)
    render(<App api={api} />)
    await fillAndSubmit('owner', 'secret pass')
    expect(await screen.findByRole('heading', { name: 'Dashboard' })).toBeVisible()
    const login = fake.calls.find((call) => call.url === '/auth/login')!
    expect(login.method).toBe('POST')
    expect(JSON.parse(login.body!)).toEqual({ id: 'owner', password: 'secret pass' })
    await api.post('/api/tasks', {})
    expect(fake.calls.at(-1)?.headers[CSRF_HEADER]).toBe('issued')
  })

  it('ID かパスワードが違うと、どちらとは言わずにその旨を出し、パスワードを空にする', async () => {
    const fake = loginServer(() => apiError(401, 'unauthenticated', 'ID or password is incorrect'))
    render(<App api={new ApiClient(fake.fetch)} />)
    await fillAndSubmit('owner', 'wrong')
    expect(await screen.findByRole('alert')).toHaveTextContent('ID かパスワードが違います')
    expect(screen.getByLabelText('パスワード')).toHaveValue('')
    expect(screen.getByLabelText('ID')).toHaveValue('owner')
    expect(screen.queryByRole('heading', { name: 'Dashboard' })).toBeNull()
  })

  it('パスワードが未設定なら、Mac mini で設定するコマンドを出す', async () => {
    const fake = loginServer(() =>
      apiError(401, 'unauthenticated', 'owner password is not set', [{ field: 'owner_credentials', reason: 'not_set' }]),
    )
    render(<App api={new ApiClient(fake.fetch)} />)
    await fillAndSubmit('owner', 'anything')
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('パスワードが設定されていません')
    expect(alert).toHaveTextContent('Mac mini')
    expect(alert).toHaveTextContent('evaloomd -db <DB> owner set-password -id <ID>')
    expect(alert).not.toHaveTextContent('ID かパスワードが違います')
  })

  it('続けて失敗して 429 のときは、待ってから試すことを出す', async () => {
    const fake = loginServer(() => apiError(429, 'unauthenticated', 'login is temporarily unavailable'))
    render(<App api={new ApiClient(fake.fetch)} />)
    await fillAndSubmit('owner', 'secret pass')
    expect(await screen.findByRole('alert')).toHaveTextContent('しばらくログインできません')
  })

  it('空のまま送信すると要求を送らずに項目エラーを出す', async () => {
    const fake = fakeFetch(NOT_LOGGED_IN)
    render(<App api={new ApiClient(fake.fetch)} />)
    await fillAndSubmit('', '')
    expect(screen.getByText('ID を入力してください')).toBeVisible()
    expect(screen.getByText('パスワードを入力してください')).toBeVisible()
    expect(screen.getByLabelText('ID')).toHaveAttribute('aria-invalid', 'true')
    expect(fake.calls).toHaveLength(1)
  })

  it('サーバーの 400 details は ID・パスワードの項目に出し、フォームに無い項目は上部に出す', async () => {
    const fake = loginServer(() =>
      apiError(400, 'validation_failed', 'invalid request', [
        { field: 'id', reason: 'too_long' },
        { field: 'unexpected', reason: 'invalid' },
      ]),
    )
    render(<App api={new ApiClient(fake.fetch)} />)
    await fillAndSubmit('owner', 'secret pass')
    expect(await screen.findByText('長すぎます')).toBeVisible()
    expect(screen.getByLabelText('ID')).toHaveAccessibleDescription(expect.stringContaining('長すぎます'))
    expect(screen.getByRole('alert')).toHaveTextContent('unexpected: 値が正しくありません')
  })

  it('ログイン後に API が 401 を返したら、その場でログイン画面へ移り、ログインし直すと元の画面へ戻る', async () => {
    window.history.replaceState(null, '', '/projects')
    let expired = false
    const fake = fakeFetch((request) => {
      if (request.url === '/auth/login') {
        expired = false
        return json(200, { csrfToken: 'again' })
      }
      if (expired) return NOT_LOGGED_IN()
      if (request.url === '/api/session') return json(200, { csrfToken: 't' })
      return json(200, EMPTY_PAGE)
    })
    const api = new ApiClient(fake.fetch)
    render(<App api={api} />)
    expect(await screen.findByRole('link', { name: 'Projects', current: 'page' })).toBeVisible()
    expired = true
    await api.get('/api/tasks').catch(() => undefined)
    await fillAndSubmit('owner', 'secret pass')
    expect(await screen.findByRole('link', { name: 'Projects', current: 'page' })).toBeVisible()
    expect(window.location.pathname).toBe('/projects')
  })

  it('ログアウトするとセッションを消してログイン画面へ移り、次のログインは Dashboard から始まる', async () => {
    window.history.replaceState(null, '', '/projects')
    const fake = fakeFetch((request) => {
      if (request.url === '/api/session') return json(200, { csrfToken: 't' })
      if (request.url === '/auth/logout') return new Response(null, { status: 204 })
      return json(200, EMPTY_PAGE)
    })
    render(<App api={new ApiClient(fake.fetch)} />)
    await userEvent.click(await screen.findByRole('button', { name: 'ログアウト' }))
    expect(await screen.findByText('Evaloom にログイン')).toBeVisible()
    const logout = fake.calls.find((call) => call.url === '/auth/logout')!
    expect(logout).toMatchObject({ method: 'POST' })
    expect(logout.headers[CSRF_HEADER]).toBe('t')
    expect(window.location.pathname).toBe('/')
  })

  it('ログアウトが 401 以外で失敗したら、画面に留まって理由を出す', async () => {
    const fake = fakeFetch((request) => {
      if (request.url === '/api/session') return json(200, { csrfToken: 't' })
      if (request.url === '/auth/logout') return apiError(500, 'internal', 'x')
      return json(200, {})
    })
    render(<App api={new ApiClient(fake.fetch)} />)
    await userEvent.click(await screen.findByRole('button', { name: 'ログアウト' }))
    expect(await screen.findByText('ログアウトできませんでした')).toBeVisible()
    expect(screen.getByRole('button', { name: 'ログアウト' })).toBeEnabled()
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
