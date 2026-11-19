import { spawnSync } from 'node:child_process'
import { mkdtempSync, realpathSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

import { expect, test as base, type Page } from '@playwright/test'

import { loginOwner, run, setupOwner, startEvaloomd } from './evaloomd.ts'

// evaloom-oxe.5: 振り分けの表（13 3.6）。
// - Project とスペシャルハーネスのどちらかを振り分け先にしたラベルを登録・変更・削除できる。
// - 両方または両方なしでは送信できない。
// - ラベルの重複と同じ振り分け先への2つ目のラベルで 409 の内容が出る。
// - 接頭辞で始まらないラベルで 400 の内容が出る。
//
// global-setup がビルドしたバイナリを一時 DB と空きポートで別に起動する。Project とスペシャルハーネスの行は
// DB へ直接入れ、ラベルの登録・変更・削除は本物の API を画面から通す。追跡ファイルは書かない。

const NOW = '2026-11-19T00:00:00Z'
const RUNNER = 'local'
const PROFILE = '01JE2E0P0000000000000000R1'
const PREFIX = 'repo:'

type Server = { origin: string; cookie: string }

const sqlText = (value: unknown) => `'${(typeof value === 'string' ? value : JSON.stringify(value)).replaceAll("'", "''")}'`

function seed(db: string) {
  const project = (id: string, name: string) =>
    `INSERT INTO projects (id, name, harness_id, profile_id, params, approval_waivers, runner_id, created_at, updated_at)
      VALUES ('${id}', '${name}', 'engineer', '${PROFILE}', '{}', '[]', '${RUNNER}', '${NOW}', '${NOW}');`
  const statements = [
    `INSERT INTO settings (key, value, updated_at) VALUES ('linear.routingLabelPrefix', ${sqlText(JSON.stringify(PREFIX))}, '${NOW}');`,
    `INSERT INTO harnesses (id, name, repo_url, repo_path, default_profile_id, approval_waivers, created_at, updated_at)
      VALUES ('engineer', 'エンジニア', 'https://example.com/e.git', '/tmp/e2e-engineer', NULL, '[]', '${NOW}', '${NOW}');`,
    `INSERT INTO harnesses (id, name, repo_url, repo_path, default_profile_id, approval_waivers, created_at, updated_at)
      VALUES ('writer', 'ライター', 'https://example.com/w.git', '/tmp/e2e-writer', NULL, '[]', '${NOW}', '${NOW}');`,
    `INSERT INTO harness_profiles (id, harness_id, version, commit_sha, tag_name, contract_version, definition, definition_hash, model_id, operation_status, registered_at)
      VALUES ('${PROFILE}', 'engineer', '1.0.0', '${'a'.repeat(40)}', 'v1.0.0', '1', '{}', 'hash', 'model', 'provisional', '${NOW}');`,
    `UPDATE harnesses SET default_profile_id = '${PROFILE}' WHERE id = 'engineer';`,
    project('alpha', 'アルファ'),
    project('beta', 'ベータ'),
    project('gamma', 'ガンマ'),
    `INSERT INTO routes (label, project_id, harness_id, created_at, updated_at) VALUES ('repo:alpha', 'alpha', NULL, '${NOW}', '${NOW}');`,
  ]
  run('sqlite3', [db, `BEGIN;\n${statements.join('\n')}\nCOMMIT;`])
}

async function login(origin: string): Promise<string> {
  const response = await loginOwner(origin)
  const cookie = /evaloom_session=([^;]+)/.exec(response.headers.getSetCookie().join('\n'))?.[1]
  if (!response.ok || !cookie) throw new Error(`ログインに失敗した: ${response.status}`)
  return cookie
}

function sessionCookie(value: string) {
  return { name: 'evaloom_session', value, domain: '127.0.0.1', path: '/', expires: -1, httpOnly: true, secure: false, sameSite: 'Strict' as const }
}

const test = base.extend<{ server: Server }, { worker: Server }>({
  worker: [
    // oxlint-disable-next-line no-empty-pattern
    async ({}, provide) => {
      const binary = process.env.EVALOOMD_BINARY
      if (!binary) throw new Error('EVALOOMD_BINARY が無い（e2e/global-setup.ts が設定する）')
      const temp = realpathSync(mkdtempSync(join(tmpdir(), 'evaloom-e2e-oxe5-')))
      const db = join(temp, 'evaloom.db')
      const registered = spawnSync(binary, ['-db', db, 'runner', 'register', '-worktree-root', join(temp, 'worktrees'), '-name', 'ローカル Runner', RUNNER], { encoding: 'utf8' })
      if (registered.status !== 0) throw new Error(`runner register が失敗した: ${registered.stderr}`)
      setupOwner(binary, db)
      seed(db)
      const evaloomd = await startEvaloomd(binary, db, '0')
      try {
        await provide({ origin: evaloomd.origin, cookie: await login(evaloomd.origin) })
      } finally {
        await evaloomd.stop()
        rmSync(temp, { recursive: true, force: true })
      }
    },
    { scope: 'worker' },
  ],
  server: async ({ worker }, provide) => provide(worker),
  baseURL: async ({ worker }, provide) => provide(worker.origin),
  storageState: async ({ worker }, provide) => provide({ cookies: [sessionCookie(worker.cookie)], origins: [] }),
})

type RouteView = { label: string; projectId: string | null; harnessId: string | null }

async function listRoutes(page: Page): Promise<RouteView[]> {
  const response = await page.request.get('/api/routes')
  expect(response.status()).toBe(200)
  return ((await response.json()) as { items: RouteView[] }).items
}

const row = (page: Page, label: string) => page.getByTestId('route-row').filter({ hasText: label })

/** 登録ボタンを押し、POST /api/routes の応答を待つ。 */
async function submitCreate(page: Page) {
  const [response] = await Promise.all([
    page.waitForResponse((r) => new URL(r.url()).pathname === '/api/routes' && r.request().method() === 'POST'),
    page.getByRole('button', { name: '登録' }).click(),
  ])
  return response
}

test('Project とスペシャルハーネスのどちらかを振り分け先にしたラベルを登録・変更・削除できる', async ({ page }) => {
  await page.goto('/routes')
  await expect(page.getByTestId('recheck-note')).toContainText('次の照会で判定し直します')
  await expect(row(page, 'repo:alpha')).toContainText('Project')
  await expect(row(page, 'repo:alpha')).toContainText('アルファ')

  // Project を振り分け先にして登録する。
  await page.getByLabel('ラベル', { exact: true }).fill('repo:beta')
  await page.getByLabel('Project', { exact: true }).selectOption('beta')
  expect((await submitCreate(page)).status()).toBe(201)
  await expect(page.getByRole('status')).toContainText('ラベル「repo:beta」を登録しました')
  await expect(row(page, 'repo:beta')).toContainText('ベータ')

  // スペシャルハーネスを振り分け先にして登録する。既定の版が無いことを示す。
  await page.getByLabel('ラベル', { exact: true }).fill('repo:writer')
  await page.getByLabel('スペシャルハーネス', { exact: true }).selectOption('writer')
  await expect(page.getByTestId('no-default-profile')).toContainText('既定の版が無いため')
  expect((await submitCreate(page)).status()).toBe(201)
  await expect(row(page, 'repo:writer')).toContainText('スペシャルハーネス')
  await expect(row(page, 'repo:writer')).toContainText('ライター')
  await expect(row(page, 'repo:writer')).toContainText('既定の版が無いため実行されません')

  // 振り分け先を Project からスペシャルハーネスへ変える。
  await page.getByRole('button', { name: '「repo:beta」を変更' }).click()
  const form = page.getByRole('form', { name: '「repo:beta」の振り分け先の変更' })
  await expect(form.getByLabel('ラベル', { exact: true })).toBeDisabled()
  await form.getByLabel('Project', { exact: true }).selectOption('')
  await form.getByLabel('スペシャルハーネス', { exact: true }).selectOption('engineer')
  const [patched] = await Promise.all([
    page.waitForResponse((r) => new URL(r.url()).pathname === '/api/routes/repo%3Abeta' && r.request().method() === 'PATCH'),
    form.getByRole('button', { name: '保存' }).click(),
  ])
  expect(patched.status()).toBe(200)
  await expect(page.getByRole('status')).toContainText('振り分け先を変更しました')
  await expect(row(page, 'repo:beta')).toContainText('エンジニア')

  // 削除する。
  await page.getByRole('button', { name: '「repo:writer」を削除' }).click()
  const dialog = page.getByRole('alertdialog')
  await expect(dialog).toContainText('次の照会で判定し直します')
  await dialog.getByRole('button', { name: '削除' }).click()
  await expect(page.getByRole('status')).toContainText('ラベル「repo:writer」を削除しました')
  await expect(row(page, 'repo:writer')).toHaveCount(0)

  const routes = await listRoutes(page)
  expect(routes.find((r) => r.label === 'repo:beta')).toMatchObject({ projectId: null, harnessId: 'engineer' })
  expect(routes.some((r) => r.label === 'repo:writer')).toBe(false)
})

test('両方または両方なしでは送信できない', async ({ page }) => {
  const writes: string[] = []
  page.on('request', (request) => {
    if (new URL(request.url()).pathname.startsWith('/api/routes') && request.method() !== 'GET') writes.push(request.method())
  })
  await page.goto('/routes')
  await page.getByLabel('ラベル', { exact: true }).fill('repo:gamma')
  await page.getByRole('button', { name: '登録' }).click()
  await expect(page.getByText('Project かスペシャルハーネスのどちらか一方を選んでください')).toBeVisible()

  await page.getByLabel('Project', { exact: true }).selectOption('gamma')
  await page.getByLabel('スペシャルハーネス', { exact: true }).selectOption('writer')
  await page.getByRole('button', { name: '登録' }).click()
  await expect(page.getByText('Project かスペシャルハーネスのどちらか一方だけを選んでください')).toBeVisible()

  expect(writes).toEqual([])
  expect((await listRoutes(page)).some((r) => r.label === 'repo:gamma')).toBe(false)
})

test('ラベルの重複と同じ振り分け先への2つ目のラベルで 409 の内容が出る', async ({ page }) => {
  await page.goto('/routes')
  // ラベルの重複。
  await page.getByLabel('ラベル', { exact: true }).fill('repo:alpha')
  await page.getByLabel('Project', { exact: true }).selectOption('gamma')
  expect((await submitCreate(page)).status()).toBe(409)
  const alert = page.getByTestId('api-error')
  await expect(alert).toContainText('ラベルを登録できませんでした')
  await expect(alert).toContainText('同じラベルか、同じ振り分け先へのラベルがすでに登録されています')

  // 同じ振り分け先（alpha）への2つ目のラベル。
  await page.getByLabel('ラベル', { exact: true }).fill('repo:alpha-second')
  await page.getByLabel('Project', { exact: true }).selectOption('alpha')
  expect((await submitCreate(page)).status()).toBe(409)
  await expect(alert).toContainText('同じラベルか、同じ振り分け先へのラベルがすでに登録されています')
  // 入力は残る。
  await expect(page.getByLabel('ラベル', { exact: true })).toHaveValue('repo:alpha-second')
  expect((await listRoutes(page)).some((r) => r.label === 'repo:alpha-second')).toBe(false)
})

test('接頭辞で始まらないラベルで 400 の内容が出る', async ({ page }) => {
  await page.goto('/routes')
  await page.getByLabel('ラベル', { exact: true }).fill('team:gamma')
  await page.getByLabel('Project', { exact: true }).selectOption('gamma')
  expect((await submitCreate(page)).status()).toBe(400)
  const field = page.locator('[data-field="label"]')
  await expect(field.locator('[data-slot="field-error"]')).toContainText('「repo:」で始まるラベルにしてください')
  await expect(page.getByLabel('ラベル', { exact: true })).toHaveAttribute('aria-invalid', 'true')
  expect((await listRoutes(page)).some((r) => r.label === 'team:gamma')).toBe(false)
})
