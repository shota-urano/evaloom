import { spawnSync } from 'node:child_process'
import { mkdirSync, mkdtempSync, realpathSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { expect, test as base, type Page } from '@playwright/test'

import { loginOwner, setupOwner, startEvaloomd } from './evaloomd.ts'
import { createRepo } from '../../scripts/fixtures.mts'

// evaloom-52y.13: Project の登録・編集の setup の許可宛先（13 3.2、03-registry 3.3.1、Q87）。
//
// global-setup がビルドしたバイナリを空きポートで別に起動し、本物の検証（backend/internal/api/projects.go）の
// 400 の details がフォームの入力欄に出ることを確かめる。

const SCREENSHOTS = fileURLToPath(new URL('../../docs/design/screenshots/', import.meta.url))
const LABEL = 'setup の許可宛先'
const INVALID_FORMAT = 'host:port の形式で入力してください（ワイルドカード・IP アドレス・localhost などのローカルの名前・ポートの無い値は使えません）'
const DUPLICATE = '同じ宛先（host:port）が2回以上入力されています'

type Server = { temp: string; origin: string; cookie: string }

async function login(origin: string): Promise<string> {
  const response = await loginOwner(origin)
  const cookie = /evaloom_session=([^;]+)/.exec(response.headers.getSetCookie().join('\n'))?.[1]
  if (!response.ok || !cookie) throw new Error(`ログインに失敗した: ${response.status}`)
  return cookie
}

/** 本人用 API をセッション Cookie で呼ぶ（テストの確認用）。 */
async function owner(server: Server, method: string, path: string, body?: unknown) {
  const cookie = `evaloom_session=${server.cookie}`
  const session = await fetch(`${server.origin}/api/session`, { headers: { Cookie: cookie, Accept: 'application/json' } })
  const { csrfToken } = (await session.json()) as { csrfToken: string }
  const response = await fetch(`${server.origin}${path}`, {
    method,
    headers: { Cookie: cookie, Origin: server.origin, 'X-CSRF-Token': csrfToken, 'Content-Type': 'application/json', Accept: 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  const text = await response.text()
  if (!response.ok) throw new Error(`${method} ${path}: ${response.status} ${text}`)
  return text === '' ? undefined : JSON.parse(text)
}

const getHosts = async (server: Server, id: string) =>
  ((await owner(server, 'GET', `/api/projects/${id}`)) as { setupAllowedHosts: string[] }).setupAllowedHosts

function sessionCookie(value: string) {
  return { name: 'evaloom_session', value, domain: '127.0.0.1', path: '/', expires: -1, httpOnly: true, secure: false, sameSite: 'Strict' as const }
}

const test = base.extend<{ server: Server }, { worker: Server }>({
  worker: [
    // oxlint-disable-next-line no-empty-pattern
    async ({}, provide) => {
      const binary = process.env.EVALOOMD_BINARY
      if (!binary) throw new Error('EVALOOMD_BINARY が無い（e2e/global-setup.ts が設定する）')
      const temp = realpathSync(mkdtempSync(join(tmpdir(), 'evaloom-e2e-52y13-')))
      const db = join(temp, 'evaloom.db')
      const registered = spawnSync(binary, ['-db', db, 'runner', 'register', '-worktree-root', join(temp, 'worktrees'), '-name', 'ローカル Runner', 'local'], { encoding: 'utf8' })
      if (registered.status !== 0) throw new Error(`runner register が失敗した: ${registered.stderr}`)
      setupOwner(binary, db)
      const evaloomd = await startEvaloomd(binary, db, '0')
      try {
        await provide({ temp, origin: evaloomd.origin, cookie: await login(evaloomd.origin) })
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

const field = (page: Page, label: string) => page.locator('[data-field]').filter({ has: page.getByLabel(label, { exact: true }) })
const detailRow = (page: Page, label: string) => page.locator('dl > div').filter({ has: page.getByText(label, { exact: true }) }).locator('dd')

async function fillRepository(page: Page, id: string, name: string, repo: { path: string; url: string }) {
  await page.getByLabel('Project ID').fill(id)
  await page.getByLabel('名称').fill(name)
  await page.getByLabel('ローカルリポジトリのパス').fill(repo.path)
  await page.getByLabel('Git リポジトリ URL').fill(repo.url)
  await page.getByLabel('Runner').selectOption('local')
}

test('登録で host:port を複数入力して保存し、編集画面に登録済みの値が出る。全部消して保存すると空配列になる', async ({ page, server }) => {
  const repo = createRepo(server.temp, 'hosts-alpha')
  await page.goto('/projects')
  await page.getByRole('link', { name: 'プロジェクトを登録' }).click()
  await fillRepository(page, 'hosts-alpha', 'Hosts Alpha', repo)
  await page.getByLabel(LABEL).fill('registry.npmjs.org:443\n\n  proxy.golang.org:443  \n')
  await page.getByRole('button', { name: '登録' }).click()

  await expect(page).toHaveURL('/projects/hosts-alpha')
  await expect(detailRow(page, LABEL)).toHaveText('registry.npmjs.org:443proxy.golang.org:443')
  expect(await getHosts(server, 'hosts-alpha')).toEqual(['registry.npmjs.org:443', 'proxy.golang.org:443'])

  // 編集画面では登録済みの値が1行に1つ入っている。1つ足して保存する。
  await page.getByRole('link', { name: '編集' }).click()
  await expect(page.getByLabel(LABEL)).toHaveValue('registry.npmjs.org:443\nproxy.golang.org:443')
  mkdirSync(SCREENSHOTS, { recursive: true })
  await page.screenshot({ path: join(SCREENSHOTS, 'evaloom-52y.13-project-edit.png'), fullPage: true })
  await page.getByLabel(LABEL).fill('registry.npmjs.org:443\nproxy.golang.org:443\ngithub.com:443')
  await page.getByRole('button', { name: '保存' }).click()
  await expect(page).toHaveURL('/projects/hosts-alpha')
  expect(await getHosts(server, 'hosts-alpha')).toEqual(['registry.npmjs.org:443', 'proxy.golang.org:443', 'github.com:443'])

  // 空欄にして保存すると空配列で解除される。
  await page.getByRole('link', { name: '編集' }).click()
  await page.getByLabel(LABEL).fill('')
  await page.getByRole('button', { name: '保存' }).click()
  await expect(page).toHaveURL('/projects/hosts-alpha')
  await expect(detailRow(page, LABEL)).toHaveText('なし')
  expect(await getHosts(server, 'hosts-alpha')).toEqual([])
})

test('何も入力せずに登録すると、setup の許可宛先は空配列で保存される', async ({ page, server }) => {
  const repo = createRepo(server.temp, 'hosts-empty')
  await page.goto('/projects/_new')
  await fillRepository(page, 'hosts-empty', 'Hosts Empty', repo)
  const request = page.waitForRequest((r) => r.method() === 'POST' && new URL(r.url()).pathname === '/api/projects')
  await page.getByRole('button', { name: '登録' }).click()
  expect((await request).postDataJSON()).toMatchObject({ setupAllowedHosts: [] })
  await expect(page).toHaveURL('/projects/hosts-empty')
  expect(await getHosts(server, 'hosts-empty')).toEqual([])
})

test('ワイルドカード・IP リテラル・ローカルの名前・ポートの無い値・重複は 400 になり、内容が入力欄に出る', async ({ page, server }) => {
  const repo = createRepo(server.temp, 'hosts-bad')
  await page.goto('/projects/_new')
  await fillRepository(page, 'hosts-bad', 'Hosts Bad', repo)

  const cases = [
    { value: '*.example.com:443', message: INVALID_FORMAT },
    { value: '192.0.2.10:443', message: INVALID_FORMAT },
    { value: 'localhost:8080', message: INVALID_FORMAT },
    { value: 'cache.internal:443', message: INVALID_FORMAT },
    { value: 'registry.npmjs.org', message: INVALID_FORMAT },
    { value: 'github.com:443\ngithub.com:443', message: DUPLICATE },
  ]
  for (const { value, message } of cases) {
    await page.getByLabel(LABEL).fill(value)
    const response = page.waitForResponse((r) => r.request().method() === 'POST' && new URL(r.url()).pathname === '/api/projects')
    await page.getByRole('button', { name: '登録' }).click()
    const body = (await (await response).json()) as { error: { code: string; details: { field: string }[] } }
    expect((await response).status(), value).toBe(400)
    expect(body.error.details.map((d) => d.field), value).toEqual(['setupAllowedHosts'])
    await expect(field(page, LABEL), value).toContainText(message)
    await expect(page.getByLabel(LABEL)).toHaveAttribute('aria-invalid', 'true')
    await expect(page).toHaveURL('/projects/_new')
    if (message === DUPLICATE) {
      mkdirSync(SCREENSHOTS, { recursive: true })
      await page.screenshot({ path: join(SCREENSHOTS, 'evaloom-52y.13-project-form.png'), fullPage: true })
    }
  }

  // 直せば登録できる。
  await page.getByLabel(LABEL).fill('github.com:443')
  await page.getByRole('button', { name: '登録' }).click()
  await expect(page).toHaveURL('/projects/hosts-bad')
  expect(await getHosts(server, 'hosts-bad')).toEqual(['github.com:443'])

  // 編集でも同じく入力欄に出て、保存されない。
  await page.getByRole('link', { name: '編集' }).click()
  await page.getByLabel(LABEL).fill('github.com:443\ngithub.com:443')
  await page.getByRole('button', { name: '保存' }).click()
  await expect(field(page, LABEL)).toContainText(DUPLICATE)
  expect(await getHosts(server, 'hosts-bad')).toEqual(['github.com:443'])
})
