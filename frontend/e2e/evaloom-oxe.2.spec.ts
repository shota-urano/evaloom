import { mkdtempSync, realpathSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

import { expect, test, type Page } from '@playwright/test'

import { startEvaloomd, testOwner } from './evaloomd.ts'

// evaloom-oxe.2: ログイン画面とログアウト、401 のときのログイン画面への移動（13 3.1・6章、18 3.4〜3.6）。
// - ID とパスワードでログインでき Dashboard が出る。
// - 間違えると「ID かパスワードが違う」ことが出る。
// - セッションが切れたら操作の途中でもログイン画面へ移り、ログインし直すと元の画面へ戻る。
// - パスワードが未設定のとき設定の手順が出る。
// - ログアウトでログイン画面へ移る。
//
// 主な確認は global-setup が起動した evaloomd（E2E_PORT、パスワード設定済み）を使う。
// このファイルの操作はサーバーに行を作らない（登録は送る前にセッションが切れて 401 になる）。
// パスワード未設定の確認だけは、パスワードを設定していない別の evaloomd を一時 DB で起動する。
// 追跡ファイル（docs/design/screenshots など）は書かない。

async function expectLoginScreen(page: Page) {
  await expect(page.getByText('Evaloom にログイン')).toBeVisible()
  await expect(page.getByLabel('ID', { exact: true })).toBeVisible()
  await expect(page.getByLabel('パスワード', { exact: true })).toBeVisible()
}

async function submitLogin(page: Page, id: string, password: string) {
  await page.getByLabel('ID', { exact: true }).fill(id)
  await page.getByLabel('パスワード', { exact: true }).fill(password)
  await page.getByRole('button', { name: 'ログイン' }).click()
}

test('ID とパスワードでログインすると Dashboard が出る', async ({ page }) => {
  await page.goto('/')
  await expectLoginScreen(page)
  const login = page.waitForResponse((response) => new URL(response.url()).pathname === '/auth/login')
  await submitLogin(page, testOwner.id, testOwner.password)
  expect((await login).status()).toBe(200)
  await expect(page.getByRole('heading', { name: 'Dashboard' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'ログアウト' })).toBeVisible()

  // セッションは Cookie に残るので、再読み込みしてもログイン済みのまま。
  await page.reload()
  await expect(page.getByRole('heading', { name: 'Dashboard' })).toBeVisible()
})

test('ID かパスワードを間違えると、その旨が出てログインできない。直して送るとログインできる', async ({ page }) => {
  await page.goto('/')
  await expectLoginScreen(page)

  // パスワード違い。
  const wrongPassword = page.waitForResponse((response) => new URL(response.url()).pathname === '/auth/login')
  await submitLogin(page, testOwner.id, 'not the password')
  expect((await wrongPassword).status()).toBe(401)
  await expect(page.getByRole('alert')).toContainText('ID かパスワードが違います')
  await expect(page.getByRole('heading', { name: 'Dashboard' })).toHaveCount(0)
  // パスワードの欄は空にし、ID は残す。
  await expect(page.getByLabel('パスワード', { exact: true })).toHaveValue('')
  await expect(page.getByLabel('ID', { exact: true })).toHaveValue(testOwner.id)

  // ID 違いも同じ表示になる（どちらが違うかは出さない）。
  const wrongId = page.waitForResponse((response) => new URL(response.url()).pathname === '/auth/login')
  await submitLogin(page, 'someone-else', testOwner.password)
  expect((await wrongId).status()).toBe(401)
  await expect(page.getByRole('alert')).toContainText('ID かパスワードが違います')
  await expect(page.getByRole('alert')).not.toContainText('パスワードが設定されていません')

  // 正しい組で送り直すとログインでき、失敗の回数も0に戻る（18 3.4）。
  await submitLogin(page, testOwner.id, testOwner.password)
  await expect(page.getByRole('heading', { name: 'Dashboard' })).toBeVisible()
  await expect(page.getByText('ID かパスワードが違います')).toHaveCount(0)
})

test('操作の途中でセッションが切れるとログイン画面へ移り、ログインし直すと元の画面へ戻る', async ({ page, baseURL }) => {
  await page.goto('/harnesses/_new')
  await expectLoginScreen(page)
  await submitLogin(page, testOwner.id, testOwner.password)
  await expect(page.getByRole('heading', { name: 'スペシャルハーネスの登録' })).toBeVisible()

  // 入力の途中で、サーバー側だけでこのセッションを無効にする（期限切れと同じく、ブラウザには Cookie が残る）。
  await page.getByLabel('スペシャルハーネス ID').fill('session-expired')
  await page.getByLabel('名称').fill('セッション切れの確認')
  const cookies = await page.context().cookies()
  const session = await page.request.get('/api/session')
  expect(session.status()).toBe(200)
  const { csrfToken } = (await session.json()) as { csrfToken: string }
  const revoked = await page.request.post('/auth/logout', { headers: { Origin: baseURL!, 'X-CSRF-Token': csrfToken } })
  expect(revoked.status()).toBe(204)
  await page.context().addCookies(cookies)
  expect((await page.request.get('/api/session')).status()).toBe(401)

  // 画面はまだ登録フォームのまま。送ると 401 になり、その場でログイン画面へ移る。URL は変えない。
  await page.getByLabel('ハーネスリポジトリのパス').fill('/nonexistent/harness')
  await page.getByLabel('ハーネスリポジトリ URL').fill('git@example.invalid:harness.git')
  const create = page.waitForResponse((response) => response.request().method() === 'POST' && new URL(response.url()).pathname === '/api/harnesses')
  await page.getByRole('button', { name: '登録' }).click()
  expect((await create).status()).toBe(401)
  await expectLoginScreen(page)
  expect(new URL(page.url()).pathname).toBe('/harnesses/_new')

  // ログインし直すと、Dashboard ではなく元の画面（Harness の登録）が出る。
  await submitLogin(page, testOwner.id, testOwner.password)
  await expect(page.getByRole('heading', { name: 'スペシャルハーネスの登録' })).toBeVisible()
  await expect(page.getByRole('heading', { name: 'Dashboard' })).toHaveCount(0)
  expect(new URL(page.url()).pathname).toBe('/harnesses/_new')
})

test('画面の移動でセッション切れが分かったときも、ログインし直すと移動先の画面が出る', async ({ page, baseURL }) => {
  await page.goto('/')
  await submitLogin(page, testOwner.id, testOwner.password)
  await expect(page.getByRole('heading', { name: 'Dashboard' })).toBeVisible()

  const cookies = await page.context().cookies()
  const { csrfToken } = (await (await page.request.get('/api/session')).json()) as { csrfToken: string }
  expect((await page.request.post('/auth/logout', { headers: { Origin: baseURL!, 'X-CSRF-Token': csrfToken } })).status()).toBe(204)
  await page.context().addCookies(cookies)

  await page.getByRole('navigation', { name: '画面' }).getByRole('link', { name: 'スペシャルハーネス', exact: true }).click()
  await expectLoginScreen(page)
  expect(new URL(page.url()).pathname).toBe('/harnesses')

  await submitLogin(page, testOwner.id, testOwner.password)
  await expect(page.getByRole('link', { name: 'スペシャルハーネス', exact: true })).toHaveAttribute('aria-current', 'page')
  expect(new URL(page.url()).pathname).toBe('/harnesses')
})

test('ログアウトするとログイン画面へ移り、そのセッションでは本人用 API を呼べなくなる', async ({ page }) => {
  await page.goto('/projects')
  await submitLogin(page, testOwner.id, testOwner.password)
  await expect(page.getByRole('link', { name: 'Projects' })).toHaveAttribute('aria-current', 'page')

  const logout = page.waitForResponse((response) => new URL(response.url()).pathname === '/auth/logout')
  await page.getByRole('button', { name: 'ログアウト' }).click()
  expect((await logout).status()).toBe(204)
  await expectLoginScreen(page)
  expect((await page.request.get('/api/session')).status()).toBe(401)

  // 再読み込みしてもログイン画面のまま。次のログインは Dashboard から始まる。
  await page.reload()
  await expectLoginScreen(page)
  await submitLogin(page, testOwner.id, testOwner.password)
  await expect(page.getByRole('heading', { name: 'Dashboard' })).toBeVisible()
})

test('パスワードが未設定のときは、ログイン画面に設定の手順が出てログインできない', async ({ page }) => {
  const binary = process.env.EVALOOMD_BINARY
  if (!binary) throw new Error('EVALOOMD_BINARY が無い（e2e/global-setup.ts が設定する）')
  const temp = realpathSync(mkdtempSync(join(tmpdir(), 'evaloom-e2e-oxe2-')))
  const evaloomd = await startEvaloomd(binary, join(temp, 'evaloom.db'), '0').catch((cause: unknown) => {
    rmSync(temp, { recursive: true, force: true })
    throw cause
  })
  try {
    await page.goto(evaloomd.origin + '/')
    await expectLoginScreen(page)
    const login = page.waitForResponse((response) => new URL(response.url()).pathname === '/auth/login')
    await submitLogin(page, testOwner.id, testOwner.password)
    expect((await login).status()).toBe(401)

    const alert = page.getByRole('alert')
    await expect(alert).toContainText('パスワードが設定されていません')
    await expect(alert).toContainText('Mac mini のターミナル')
    await expect(alert.locator('code')).toHaveText('evaloomd -db <DB> owner set-password -id <ID>')
    await expect(alert).not.toContainText('ID かパスワードが違います')
    await expect(page.getByRole('heading', { name: 'Dashboard' })).toHaveCount(0)
    expect((await page.request.get(evaloomd.origin + '/api/session')).status()).toBe(401)
  } finally {
    await evaloomd.stop()
    rmSync(temp, { recursive: true, force: true })
  }
})
