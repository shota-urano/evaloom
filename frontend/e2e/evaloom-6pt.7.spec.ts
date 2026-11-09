import { mkdirSync, mkdtempSync, realpathSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { expect, test as base, type Page } from '@playwright/test'

import { setupOwner, startEvaloomd, testOwner, type Evaloomd } from './evaloomd.ts'

// evaloom-6pt.7: ログイン画面（ID とパスワード）と、401 のときのログイン画面への移動（18 3.4）。
//
// パスワードを設定した evaloomd と、まだ設定していない evaloomd を、同じバイナリ（global-setup がビルド）で
// 専用の一時 DB を使って別に起動する。設定済みの方は DB のパスを持っておき、セッション切れを
// CLI でのパスワードの再設定（18 3.3: すべてのセッションを無効にする）で起こす。

const SCREENSHOTS = fileURLToPath(new URL('../../docs/design/screenshots/', import.meta.url))

type Server = { binary: string; db: string; origin: string }
type Servers = { owned: Server; unset: Server }

const test = base.extend<{ servers: Servers }, { worker: Servers }>({
  worker: [
    // Playwright は fixture の第1引数を分割代入で受け取る必要がある。
    // oxlint-disable-next-line no-empty-pattern
    async ({}, provide) => {
      const binary = process.env.EVALOOMD_BINARY
      if (!binary) throw new Error('EVALOOMD_BINARY が無い（e2e/global-setup.ts が設定する）')
      const temp = realpathSync(mkdtempSync(join(tmpdir(), 'evaloom-e2e-6pt7-')))
      const started: Evaloomd[] = []
      try {
        const ownedDb = join(temp, 'owned.db')
        const unsetDb = join(temp, 'unset.db')
        setupOwner(binary, ownedDb)
        const owned = await startEvaloomd(binary, ownedDb, '0')
        started.push(owned)
        const unset = await startEvaloomd(binary, unsetDb, '0')
        started.push(unset)
        await provide({
          owned: { binary, db: ownedDb, origin: owned.origin },
          unset: { binary, db: unsetDb, origin: unset.origin },
        })
      } finally {
        for (const server of started) await server.stop()
        rmSync(temp, { recursive: true, force: true })
      }
    },
    { scope: 'worker' },
  ],
  servers: async ({ worker }, provide) => provide(worker),
  baseURL: async ({ worker }, provide) => provide(worker.owned.origin),
})

async function login(page: Page, id: string, password: string) {
  await page.getByLabel('ID', { exact: true }).fill(id)
  await page.getByLabel('パスワード', { exact: true }).fill(password)
  await page.getByRole('button', { name: 'ログイン' }).click()
}

const dashboard = (page: Page) => page.getByRole('heading', { name: 'Dashboard' })
const loginTitle = (page: Page) => page.getByText('Evaloom にログイン')

test('ログイン画面から ID とパスワードでログインでき、Dashboard が出る', async ({ page }) => {
  await page.goto('/')
  await expect(loginTitle(page)).toBeVisible()
  await expect(page.getByLabel('パスワード', { exact: true })).toHaveAttribute('type', 'password')
  mkdirSync(SCREENSHOTS, { recursive: true })
  await page.screenshot({ path: join(SCREENSHOTS, 'evaloom-6pt.7-login.png'), fullPage: true })

  await login(page, testOwner.id, testOwner.password)
  await expect(dashboard(page)).toBeVisible()
  // ログインはセッション Cookie で続く（再読み込みしてもログイン画面に戻らない）。
  await page.reload()
  await expect(dashboard(page)).toBeVisible()
})

test('ID かパスワードを間違えると「ID かパスワードが違う」ことが出て、ログインしない', async ({ page }) => {
  await page.goto('/')
  for (const [id, password] of [
    [testOwner.id, 'incorrect password'],
    ['unknown-owner', testOwner.password],
  ]) {
    await login(page, id, password)
    await expect(page.getByRole('alert')).toContainText('ID かパスワードが違います')
    await expect(page.getByLabel('パスワード', { exact: true })).toHaveValue('')
    await expect(dashboard(page)).toHaveCount(0)
  }
  expect((await page.request.get('/api/session')).status()).toBe(401)

  // 間違えた後でも、正しい ID とパスワードでログインできる。
  await login(page, testOwner.id, testOwner.password)
  await expect(dashboard(page)).toBeVisible()
})

test('ログアウトの操作でログイン画面へ移り、そのセッションはもう使えない', async ({ page }) => {
  await page.goto('/projects')
  await login(page, testOwner.id, testOwner.password)
  await expect(page.getByRole('link', { name: 'Projects', exact: true })).toHaveAttribute('aria-current', 'page')

  await page.getByRole('button', { name: 'ログアウト' }).click()
  await expect(loginTitle(page)).toBeVisible()
  await expect(page).toHaveURL(/\/$/)
  expect((await page.request.get('/api/session')).status()).toBe(401)
  await page.reload()
  await expect(loginTitle(page)).toBeVisible()

  // 次のログインは Dashboard から始まる。
  await login(page, testOwner.id, testOwner.password)
  await expect(dashboard(page)).toBeVisible()
})

test('セッションが切れると操作の途中でもログイン画面へ移り、ログインし直すと元の画面へ戻る', async ({ page, servers }) => {
  await page.goto('/settings')
  await login(page, testOwner.id, testOwner.password)
  const form = page.getByRole('form', { name: 'Linear のラベルの設定' })
  await expect(form).toBeVisible()

  // パスワードを設定し直すと、それまでのセッションはすべて無効になる（18 3.3）。ブラウザの Cookie は残ったまま。
  setupOwner(servers.owned.binary, servers.owned.db)
  await form.getByRole('button', { name: '保存' }).click()

  await expect(loginTitle(page)).toBeVisible()
  await expect(page).toHaveURL(/\/settings$/)
  await login(page, testOwner.id, testOwner.password)
  await expect(page.getByRole('form', { name: 'Linear のラベルの設定' })).toBeVisible()
  await expect(page.getByRole('link', { name: 'Settings', exact: true })).toHaveAttribute('aria-current', 'page')
  await expect(page).toHaveURL(/\/settings$/)
})

test('パスワードが未設定のとき、ログインせずに設定の手順が出る', async ({ page, servers }) => {
  await page.goto(servers.unset.origin + '/')
  await expect(loginTitle(page)).toBeVisible()
  await login(page, testOwner.id, testOwner.password)

  const alert = page.getByRole('alert')
  await expect(alert).toContainText('パスワードが設定されていません')
  await expect(alert).toContainText('Mac mini のターミナル')
  await expect(alert).toContainText('evaloomd -db <DB> owner set-password -id <ID>')
  await expect(alert).not.toContainText('ID かパスワードが違います')
  await expect(dashboard(page)).toHaveCount(0)
  expect((await page.request.get(servers.unset.origin + '/api/session')).status()).toBe(401)
})
