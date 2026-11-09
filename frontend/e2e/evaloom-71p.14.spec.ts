import { spawnSync } from 'node:child_process'
import { mkdirSync, mkdtempSync, realpathSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { expect, test as base, type Page } from '@playwright/test'

import { loginOwner, setupOwner, startEvaloomd } from './evaloomd.ts'
import { createRepo } from '../../scripts/fixtures.mts'

// evaloom-71p.14: Project 編集の承認ポリシーの選択と、direct_push にするときの確認ダイアログ（17 3.2、requirements 15.3）。
//
// global-setup がビルドしたバイナリを空きポートで別に起動し、本物の検証（backend/internal/api/projects.go の
// validateApprovalPolicy）を通して保存した値を、本人用 API で読み直して確かめる。

const SCREENSHOTS = fileURLToPath(new URL('../../docs/design/screenshots/', import.meta.url))
const LABEL = '承認ポリシー'
const DIALOG = 'PR マージを承認対象から外しますか'
const SCOPE = 'PR のレビューに任せていた範囲（既存テストの書き換え、テストの実行設定・CI の定義の変更など）を確認する人がいなくなります'

type Server = { temp: string; origin: string; cookie: string }

async function login(origin: string): Promise<string> {
  const response = await loginOwner(origin)
  const cookie = /evaloom_session=([^;]+)/.exec(response.headers.getSetCookie().join('\n'))?.[1]
  if (!response.ok || !cookie) throw new Error(`ログインに失敗した: ${response.status}`)
  return cookie
}

/** 本人用 API をセッション Cookie で呼ぶ（テストの前提づくりと確認用）。 */
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

type Project = { approvalPolicy: Record<string, unknown>; updatedAt: string }
const getProject = async (server: Server, id: string) => (await owner(server, 'GET', `/api/projects/${id}`)) as Project

function sessionCookie(value: string) {
  return { name: 'evaloom_session', value, domain: '127.0.0.1', path: '/', expires: -1, httpOnly: true, secure: false, sameSite: 'Strict' as const }
}

const test = base.extend<{ server: Server }, { worker: Server }>({
  worker: [
    // oxlint-disable-next-line no-empty-pattern
    async ({}, provide) => {
      const binary = process.env.EVALOOMD_BINARY
      if (!binary) throw new Error('EVALOOMD_BINARY が無い（e2e/global-setup.ts が設定する）')
      const temp = realpathSync(mkdtempSync(join(tmpdir(), 'evaloom-e2e-71p14-')))
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

/** approvalPolicy を省略して登録する（既定の require_approval になる、17 3.2）。 */
async function registerProject(server: Server, id: string) {
  const repo = createRepo(server.temp, id)
  await owner(server, 'POST', '/api/projects', {
    id,
    name: `Policy ${id}`,
    localRepoPath: repo.path,
    gitRemoteUrl: repo.url,
    runnerId: 'local',
    baseBranch: 'main',
    defaultRole: 'developer',
  })
}

const isPatch = (id: string) => (r: { method(): string; url(): string }) => r.method() === 'PATCH' && new URL(r.url()).pathname === `/api/projects/${id}`

async function openEdit(page: Page, id: string) {
  await page.goto(`/projects/${id}/edit`)
  await expect(page.getByLabel(LABEL)).toBeVisible()
}

test('direct_push を選ぶとダイアログが出て、やめると保存要求は飛ばず、保存しても require_approval のまま', async ({ page, server }) => {
  const id = 'policy-cancel'
  await registerProject(server, id)
  const before = await getProject(server, id)
  expect(before.approvalPolicy).toEqual({ version: 1, mergePullRequest: 'require_approval' })

  const patches: string[] = []
  page.on('request', (r) => {
    if (isPatch(id)(r)) patches.push(r.postData() ?? '')
  })

  await openEdit(page, id)
  await expect(page.getByLabel(LABEL)).toHaveValue('require_approval')
  await expect(page.getByLabel(LABEL).locator('option')).toHaveText([
    'require_approval（既定）：PR を作り、本人の承認の後に Evaloom がマージする',
    'direct_push：PR を作らず、Gate 2・3 を通った commit をベースブランチへ fast-forward で push する',
  ])
  mkdirSync(SCREENSHOTS, { recursive: true })
  await page.getByLabel(LABEL).scrollIntoViewIfNeeded()
  await page.screenshot({ path: join(SCREENSHOTS, 'evaloom-71p.14-project-edit-approval-policy.png'), fullPage: true })

  await page.getByLabel(LABEL).selectOption('direct_push')
  const dialog = page.getByRole('alertdialog', { name: DIALOG })
  await expect(dialog).toBeVisible()
  await expect(dialog).toContainText(SCOPE)
  await expect(dialog.getByRole('button', { name: 'やめる' })).toBeFocused()
  await page.screenshot({ path: join(SCREENSHOTS, 'evaloom-71p.14-direct-push-dialog.png') })

  // やめると値は require_approval に戻る。保存要求はまだ飛んでいない。
  await dialog.getByRole('button', { name: 'やめる' }).click()
  await expect(dialog).toBeHidden()
  await expect(page.getByLabel(LABEL)).toHaveValue('require_approval')
  expect(patches).toEqual([])

  // Escape で閉じても同じ。
  await page.getByLabel(LABEL).selectOption('direct_push')
  await expect(dialog).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(dialog).toBeHidden()
  await expect(page.getByLabel(LABEL)).toHaveValue('require_approval')
  expect(patches).toEqual([])
  expect((await getProject(server, id)).approvalPolicy).toEqual({ version: 1, mergePullRequest: 'require_approval' })

  // 確認せずに保存すると require_approval のまま送られ、保存後の値も変わらない。
  const request = page.waitForRequest(isPatch(id))
  await page.getByRole('button', { name: '保存' }).click()
  expect((await request).postDataJSON()).toMatchObject({ approvalPolicy: { version: 1, mergePullRequest: 'require_approval' } })
  await expect(page).toHaveURL(`/projects/${id}`)
  expect((await getProject(server, id)).approvalPolicy).toEqual({ version: 1, mergePullRequest: 'require_approval' })
})

test('確認すると direct_push で保存され、編集画面で require_approval に戻せる', async ({ page, server }) => {
  const id = 'policy-confirm'
  await registerProject(server, id)

  const patches: string[] = []
  page.on('request', (r) => {
    if (isPatch(id)(r)) patches.push(r.postData() ?? '')
  })

  await openEdit(page, id)
  await page.getByLabel(LABEL).selectOption('direct_push')
  const dialog = page.getByRole('alertdialog', { name: DIALOG })
  await dialog.getByRole('button', { name: 'direct_push にする' }).click()
  await expect(dialog).toBeHidden()
  await expect(page.getByLabel(LABEL)).toHaveValue('direct_push')
  // 確認しただけでは保存しない。
  expect(patches).toEqual([])
  expect((await getProject(server, id)).approvalPolicy).toEqual({ version: 1, mergePullRequest: 'require_approval' })

  const request = page.waitForRequest(isPatch(id))
  await page.getByRole('button', { name: '保存' }).click()
  expect((await request).postDataJSON()).toMatchObject({ approvalPolicy: { version: 1, mergePullRequest: 'direct_push' } })
  await expect(page).toHaveURL(`/projects/${id}`)
  expect((await getProject(server, id)).approvalPolicy).toEqual({ version: 1, mergePullRequest: 'direct_push' })

  // 編集画面を開き直すと direct_push が選ばれている。require_approval へはダイアログ無しで戻せる。
  await page.getByRole('link', { name: '編集' }).click()
  await expect(page.getByLabel(LABEL)).toHaveValue('direct_push')
  await page.getByLabel(LABEL).selectOption('require_approval')
  await expect(page.getByRole('alertdialog')).toHaveCount(0)
  await page.getByRole('button', { name: '保存' }).click()
  await expect(page).toHaveURL(`/projects/${id}`)
  expect((await getProject(server, id)).approvalPolicy).toEqual({ version: 1, mergePullRequest: 'require_approval' })
})
