import { mkdirSync } from 'node:fs'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { expect, test, type Page, type Request } from '@playwright/test'

import { testOwner } from './evaloomd.ts'

// evaloom-oxe.1: 管理画面の雛形（13 3.1）。
// - ビルド成果物（frontend/dist）を evaloomd が同一オリジンで配信し、画面が表示される。
// - 状態を変える要求（POST / PATCH / DELETE など）に X-CSRF-Token が付く（18 3.4〜3.6、01 3.6）。
// - 400 validation_failed の details がフォームの該当項目に表示される（13 6章、01 5.3）。
//
// global-setup が起動した evaloomd（E2E_PORT、パスワード設定済み）をそのまま使う。
// このファイルの操作はサーバーに行を作らない（登録は 400 で拒否される、ログアウトはこの端末のセッションだけを消す）。

const SCREENSHOTS = fileURLToPath(new URL('../../docs/design/screenshots/', import.meta.url))
const CSRF = 'x-csrf-token' // Playwright は要求ヘッダーの名前を小文字で返す
const STATE_CHANGING = new Set(['POST', 'PUT', 'PATCH', 'DELETE'])

async function screenshot(page: Page, screen: string) {
  mkdirSync(SCREENSHOTS, { recursive: true })
  await page.screenshot({ path: join(SCREENSHOTS, `evaloom-oxe.1-${screen}.png`), fullPage: true })
}

async function loginFromScreen(page: Page) {
  await expect(page.getByText('Evaloom にログイン')).toBeVisible()
  await page.getByLabel('ID', { exact: true }).fill(testOwner.id)
  await page.getByLabel('パスワード', { exact: true }).fill(testOwner.password)
  await page.getByRole('button', { name: 'ログイン' }).click()
}

function field(page: Page, label: string) {
  return page.locator('[data-field]').filter({ has: page.getByLabel(label, { exact: true }) })
}

/** 画面が送った本人用 API の要求を記録する（静的ファイルの取得は含めない）。 */
function recordApiRequests(page: Page): Request[] {
  const requests: Request[] = []
  page.on('request', (request) => {
    const { pathname } = new URL(request.url())
    if (pathname.startsWith('/api/') || pathname.startsWith('/auth/')) requests.push(request)
  })
  return requests
}

test('evaloomd が配信したビルド成果物で管理画面が表示され、画面の URL を直接開いても同じ画面が出る', async ({ page }) => {
  // index.html と、そこから読む JavaScript・CSS を evaloomd が返す（開発サーバーではない）。
  const index = await page.request.get('/')
  expect(index.status()).toBe(200)
  expect(index.headers()['content-type']).toContain('text/html')
  const html = await index.text()
  const assets = [...html.matchAll(/(?:src|href)="(\/assets\/[^"]+\.(?:js|css))"/g)].map((match) => match[1])
  expect(assets.some((asset) => asset.endsWith('.js'))).toBe(true)
  expect(assets.some((asset) => asset.endsWith('.css'))).toBe(true)
  for (const asset of assets) {
    const response = await page.request.get(asset)
    expect(response.status(), asset).toBe(200)
  }

  // 未ログインならログイン画面が出て、ログインすると Dashboard が出る。
  await page.goto('/')
  await screenshot(page, 'login')
  await loginFromScreen(page)
  await expect(page.getByRole('heading', { name: 'Dashboard' })).toBeVisible()
  await screenshot(page, 'dashboard')

  // 拡張子の無いパスは index.html に戻され、クライアント側のルーティングで画面が出る。
  await page.goto('/harnesses/_new')
  await expect(page.getByRole('heading', { name: 'Harness の登録' })).toBeVisible()
})

test('状態を変える要求には X-CSRF-Token が付き、読み取りの要求には付かない', async ({ page }) => {
  const requests = recordApiRequests(page)
  await page.goto('/')
  await loginFromScreen(page)
  await expect(page.getByRole('heading', { name: 'Dashboard' })).toBeVisible()
  const session = await page.request.get('/api/session')
  expect(session.status()).toBe(200)
  const { csrfToken } = (await session.json()) as { csrfToken: string }
  expect(csrfToken).toBeTruthy()

  // 登録の送信（POST）。サーバーは 400 で拒否するが、要求にはトークンが付いている。
  await page.goto('/harnesses/_new')
  await page.getByLabel('Harness ID').fill('Bad_ID')
  await page.getByLabel('名称').fill('CSRF の確認')
  await page.getByLabel('ハーネスリポジトリのパス').fill('/nonexistent/harness')
  await page.getByLabel('ハーネスリポジトリ URL').fill('git@example.invalid:harness.git')
  const create = page.waitForRequest((request) => request.method() === 'POST' && new URL(request.url()).pathname === '/api/harnesses')
  await page.getByRole('button', { name: '登録' }).click()
  expect((await create).headers()[CSRF]).toBe(csrfToken)
  await expect(page.getByTestId('api-error')).toBeVisible()

  // ログアウト（POST /auth/logout）にもトークンが付き、受け付けられてログイン画面へ戻る。
  const logout = page.waitForResponse((response) => new URL(response.url()).pathname === '/auth/logout')
  await page.getByRole('button', { name: 'ログアウト' }).click()
  const logoutResponse = await logout
  expect(logoutResponse.status()).toBeLessThan(300)
  expect(logoutResponse.request().headers()[CSRF]).toBe(csrfToken)
  await expect(page.getByText('Evaloom にログイン')).toBeVisible()

  // ログイン前の POST /auth/login を除き、状態を変える要求にはすべてトークンが付いている。読み取りの要求には付けない。
  const changing = requests.filter((request) => STATE_CHANGING.has(request.method()) && new URL(request.url()).pathname !== '/auth/login')
  expect(changing.length).toBeGreaterThanOrEqual(2)
  for (const request of changing) expect(request.headers()[CSRF], `${request.method()} ${request.url()}`).toBe(csrfToken)
  const reads = requests.filter((request) => request.method() === 'GET')
  expect(reads.length).toBeGreaterThan(0)
  for (const request of reads) expect(request.headers()[CSRF], request.url()).toBeUndefined()
})

test('トークンの無い状態を変える要求は evaloomd が 403 で拒否する', async ({ page, baseURL }) => {
  await page.goto('/')
  await loginFromScreen(page)
  await expect(page.getByRole('heading', { name: 'Dashboard' })).toBeVisible()
  // 画面と同じセッション Cookie で、X-CSRF-Token だけを付けずに送る。
  const response = await page.request.post('/api/harnesses', {
    headers: { Origin: baseURL! },
    data: { id: 'no-token', name: 'x', repoPath: '/nonexistent', repoUrl: 'git@example.invalid:x.git' },
  })
  expect(response.status()).toBe(403)
  expect((await response.json()).error.code).toBe('forbidden')
})

test('400 の details がフォームの該当する項目に表示され、直して送り直すと別の項目に移る', async ({ page }) => {
  await page.goto('/harnesses/_new')
  await loginFromScreen(page)
  await expect(page.getByRole('heading', { name: 'Harness の登録' })).toBeVisible()

  // Harness ID が slug でない → details: [{ field: "id", reason: "invalid_format" }]
  await page.getByLabel('Harness ID').fill('Bad_ID')
  await page.getByLabel('名称').fill('details の確認')
  await page.getByLabel('ハーネスリポジトリのパス').fill('/nonexistent/harness')
  await page.getByLabel('ハーネスリポジトリ URL').fill('git@example.invalid:harness.git')
  const first = page.waitForResponse((response) => response.request().method() === 'POST' && new URL(response.url()).pathname === '/api/harnesses')
  await page.getByRole('button', { name: '登録' }).click()
  const firstResponse = await first
  expect(firstResponse.status()).toBe(400)
  expect((await firstResponse.json()).error.details).toContainEqual({ field: 'id', reason: 'invalid_format' })

  await expect(page.getByTestId('api-error')).toContainText('入力内容を確認してください')
  await expect(field(page, 'Harness ID').locator('[data-slot=field-error]')).toHaveText('形式が正しくありません')
  await expect(page.getByLabel('Harness ID')).toHaveAttribute('aria-invalid', 'true')
  await expect(field(page, 'ハーネスリポジトリ URL').locator('[data-slot=field-error]')).toHaveCount(0)
  // 入力は消さずに残す。
  await expect(page.getByLabel('名称')).toHaveValue('details の確認')
  await screenshot(page, 'form-error')

  // ID を直し、URL を形式の正しくない値にして送り直す → details: [{ field: "repoUrl", reason: "invalid_format" }]
  await page.getByLabel('Harness ID').fill('details-check')
  await page.getByLabel('ハーネスリポジトリ URL').fill('not a url')
  const second = page.waitForResponse((response) => response.request().method() === 'POST' && new URL(response.url()).pathname === '/api/harnesses')
  await page.getByRole('button', { name: '登録' }).click()
  expect((await second).status()).toBe(400)
  await expect(field(page, 'ハーネスリポジトリ URL').locator('[data-slot=field-error]')).toHaveText('形式が正しくありません')
  await expect(page.getByLabel('ハーネスリポジトリ URL')).toHaveAttribute('aria-invalid', 'true')
  await expect(field(page, 'Harness ID').locator('[data-slot=field-error]')).toHaveCount(0)
  await expect(page.getByLabel('Harness ID')).not.toHaveAttribute('aria-invalid', 'true')
})
