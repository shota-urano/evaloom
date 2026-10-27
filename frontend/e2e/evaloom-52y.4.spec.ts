import { mkdirSync, mkdtempSync, realpathSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { expect, test as base, type Page } from '@playwright/test'

import { run, startEvaloomd } from './evaloomd.ts'
import { createRepo, tagProfile, type Repo } from '../../scripts/fixtures.mts'

// evaloom-52y.4: Tasks 一覧とタスク作成。
//
// evaloom-52y.3 と同じく、global-setup がビルドしたバイナリを空きポートで別に起動する。
// タスク作成（POST /api/tasks）にはプロジェクトの既定の役割（developer）への Harness 割り当てが要る（04 3.1）。
// - Harness の行は SQLite に直接入れる（evaloomd は具体的な Adapter をまだ登録していない。52y.3 の spec を参照）。
// - Profile の版の登録、2つのプロジェクトの登録、割り当ては本人用 API で行う。

const SCREENSHOTS = fileURLToPath(new URL('../../docs/design/screenshots/', import.meta.url))
const HARNESS = 'claude-developer'
const PROJECTS = [
  { id: 'alpha-app', name: 'Alpha アプリ' },
  { id: 'beta-app', name: 'Beta アプリ' },
] as const

type Server = { origin: string; cookie: string }

function seedHarness(db: string, harness: Repo) {
  const now = new Date().toISOString()
  run('sqlite3', [
    db,
    `INSERT INTO harnesses (id, name, specialty, supported_tasks, adapter, repo_path, repo_url, supported_models,
      available_tools, tool_permissions, contract_version, created_at, updated_at)
      VALUES ('${HARNESS}', 'Claude Developer', 'developer', '["development"]', 'claude_code', '${harness.path}', '${harness.url}',
      '["claude-e2e-model"]', '["read","write"]', '{}', '1', '${now}', '${now}');`,
  ])
}

/** 本人用 API をセッション Cookie で呼ぶ（テストの前提づくりと件数の確認用）。evaloomd は Origin も照合する。 */
async function owner(server: Server, method: string, path: string, body?: unknown, headers: Record<string, string> = {}) {
  const cookie = `evaloom_session=${server.cookie}`
  const session = await fetch(`${server.origin}/api/session`, { headers: { Cookie: cookie, Accept: 'application/json' } })
  const { csrfToken } = (await session.json()) as { csrfToken: string }
  const response = await fetch(`${server.origin}${path}`, {
    method,
    headers: { Cookie: cookie, Origin: server.origin, 'X-CSRF-Token': csrfToken, 'Content-Type': 'application/json', Accept: 'application/json', ...headers },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  const text = await response.text()
  if (!response.ok) throw new Error(`${method} ${path}: ${response.status} ${text}`)
  return text === '' ? undefined : JSON.parse(text)
}

type ListedTask = { id: string; title: string; projectId: string; status: string }

async function listTasks(server: Server, query = ''): Promise<ListedTask[]> {
  return ((await owner(server, 'GET', `/api/tasks?limit=200${query}`)) as { items: ListedTask[] }).items
}

async function createTask(server: Server, projectId: string, title: string): Promise<ListedTask> {
  return (await owner(
    server,
    'POST',
    '/api/tasks',
    { projectId, title, body: `${title} の本文`, acceptanceCriteria: `${title} が動く` },
    { 'Idempotency-Key': crypto.randomUUID() },
  )) as ListedTask
}

const test = base.extend<{ server: Server }, { worker: Server }>({
  worker: [
    // oxlint-disable-next-line no-empty-pattern
    async ({}, provide) => {
      const binary = process.env.EVALOOMD_BINARY
      if (!binary) throw new Error('EVALOOMD_BINARY が無い（e2e/global-setup.ts が設定する）')
      const temp = realpathSync(mkdtempSync(join(tmpdir(), 'evaloom-e2e-52y4-')))
      const db = join(temp, 'evaloom.db')
      const harness = createRepo(temp, 'harness')
      tagProfile(harness.path, '1.0.0', 'first')
      run(binary, ['-db', db, 'runner', 'register', '-worktree-root', join(temp, 'worktrees'), '-name', 'ローカル Runner', 'local'])
      seedHarness(db, harness)
      const evaloomd = await startEvaloomd(binary, db, '0')
      try {
        const response = await fetch(evaloomd.loginURL, { headers: { Accept: 'application/json' } })
        const cookie = /evaloom_session=([^;]+)/.exec(response.headers.getSetCookie().join('\n'))?.[1]
        if (!response.ok || !cookie) throw new Error(`ログインに失敗した: ${response.status}`)
        const server = { origin: evaloomd.origin, cookie }
        await owner(server, 'POST', '/api/harness-profiles', { harnessId: HARNESS, version: '1.0.0' })
        for (const project of PROJECTS) {
          const repo = createRepo(temp, project.id)
          await owner(server, 'POST', '/api/projects', {
            id: project.id, name: project.name, localRepoPath: repo.path, gitRemoteUrl: repo.url, runnerId: 'local', baseBranch: 'main', defaultRole: 'developer',
          })
          await owner(server, 'POST', `/api/projects/${project.id}/harnesses`, { role: 'developer', harnessId: HARNESS, version: '1.0.0' })
        }
        await provide(server)
      } finally {
        await evaloomd.stop()
        rmSync(temp, { recursive: true, force: true })
      }
    },
    { scope: 'worker' },
  ],
  server: async ({ worker }, provide) => provide(worker),
  baseURL: async ({ worker }, provide) => provide(worker.origin),
  storageState: async ({ worker }, provide) =>
    provide({
      cookies: [
        { name: 'evaloom_session', value: worker.cookie, domain: '127.0.0.1', path: '/', expires: -1, httpOnly: true, secure: false, sameSite: 'Strict' },
      ],
      origins: [],
    }),
})

async function screenshot(page: Page, screen: string) {
  mkdirSync(SCREENSHOTS, { recursive: true })
  await page.screenshot({ path: join(SCREENSHOTS, `evaloom-52y.4-${screen}.png`), fullPage: true })
}

function field(page: Page, label: string) {
  return page.locator('[data-field]').filter({ has: page.getByLabel(label, { exact: true }) })
}

const taskRows = (page: Page) => page.getByRole('table', { name: 'タスク' }).locator('tbody tr')

async function openForm(page: Page) {
  await page.goto('/')
  await page.getByRole('navigation', { name: '画面' }).getByRole('link', { name: 'Tasks' }).click()
  await expect(page.getByRole('heading', { name: 'Tasks' })).toBeVisible()
  await page.getByRole('link', { name: 'タスクを作成' }).click()
  await expect(page).toHaveURL('/tasks/_new')
  await expect(page.getByRole('heading', { name: 'タスクの作成' })).toBeVisible()
}

async function fillForm(page: Page, title: string, acceptanceCriteria: string) {
  await page.getByLabel('プロジェクト').selectOption({ label: PROJECTS[0].name })
  await page.getByLabel('件名').fill(title)
  await page.getByLabel('本文').fill(`${title} の本文`)
  await page.getByLabel('受け入れ条件').fill(acceptanceCriteria)
}

test('受け入れ条件が空だと送信できない', async ({ page, server }) => {
  await openForm(page)
  const title = '受け入れ条件の無いタスク'
  await fillForm(page, title, '')
  await page.getByRole('button', { name: '作成' }).click()
  await expect(field(page, '受け入れ条件').locator('[data-slot=field-error]')).toHaveText('入力してください')
  await expect(page.getByLabel('受け入れ条件')).toHaveAttribute('aria-invalid', 'true')
  await expect(page).toHaveURL('/tasks/_new')
  await screenshot(page, 'task-form')

  // 空白だけでも送らない（04 3.1）。
  await page.getByLabel('受け入れ条件').fill('   \n  ')
  await page.getByRole('button', { name: '作成' }).click()
  await expect(field(page, '受け入れ条件').locator('[data-slot=field-error]')).toHaveText('入力してください')
  await expect(page).toHaveURL('/tasks/_new')
  expect((await listTasks(server)).filter((t) => t.title === title)).toHaveLength(0)
})

test('二重クリックでタスクが2件にならない', async ({ page, server }) => {
  await openForm(page)
  const title = '二重クリックのタスク'
  await fillForm(page, title, 'ログインのテストが通る')
  await page.getByLabel('短い名前').fill('double-click')

  const posts: string[] = []
  page.on('request', (request) => {
    if (request.method() === 'POST' && new URL(request.url()).pathname === '/api/tasks') posts.push(request.headers()['idempotency-key'] ?? '')
  })
  // 送信ボタンを素早く2回クリックする。
  await page.getByRole('button', { name: '作成' }).dblclick()

  await expect(page).toHaveURL('/tasks')
  await expect(page.getByRole('status')).toContainText(`（${title}）を作成しました。`)
  await expect(taskRows(page).filter({ hasText: title })).toHaveCount(1)
  await expect(taskRows(page).filter({ hasText: title })).toContainText('Queued')
  const created = (await listTasks(server)).filter((t) => t.title === title)
  expect(created).toHaveLength(1)
  expect(created[0].status).toBe('queued')
  expect(posts.length).toBeGreaterThanOrEqual(1)
  expect(posts.every((key) => key !== '' && key === posts[0])).toBe(true)

  // 再読み込みしても1件のまま。
  await page.reload()
  await expect(taskRows(page).filter({ hasText: title })).toHaveCount(1)
})

test('状態とプロジェクトで絞り込める', async ({ page, server }) => {
  const queuedAlpha = await createTask(server, PROJECTS[0].id, 'Alpha のログイン画面')
  const cancelledAlpha = await createTask(server, PROJECTS[0].id, 'Alpha の古い依頼')
  const queuedBeta = await createTask(server, PROJECTS[1].id, 'Beta の設定画面')
  await owner(server, 'POST', `/api/tasks/${cancelledAlpha.id}/cancel`)

  await page.goto('/tasks')
  for (const task of [queuedAlpha, cancelledAlpha, queuedBeta]) await expect(taskRows(page).filter({ hasText: task.title })).toHaveCount(1)
  await expect(taskRows(page).filter({ hasText: cancelledAlpha.title })).toContainText('Cancelled')
  await expect(taskRows(page).filter({ hasText: queuedBeta.title })).toContainText(PROJECTS[1].name)
  await expect(taskRows(page).filter({ hasText: queuedBeta.title })).toContainText('#1 pending')

  // プロジェクトで絞り込む。
  await page.getByLabel('プロジェクト').selectOption({ label: PROJECTS[0].name })
  await expect(page).toHaveURL(`/tasks?projectId=${PROJECTS[0].id}`)
  await expect(taskRows(page).filter({ hasText: queuedBeta.title })).toHaveCount(0)
  await expect(taskRows(page).filter({ hasText: queuedAlpha.title })).toHaveCount(1)
  await expect(taskRows(page).filter({ hasText: cancelledAlpha.title })).toHaveCount(1)
  const alphaCount = (await listTasks(server, `&projectId=${PROJECTS[0].id}`)).length
  await expect(taskRows(page)).toHaveCount(alphaCount)
  await expect(taskRows(page).filter({ hasText: PROJECTS[1].name })).toHaveCount(0)
  await screenshot(page, 'tasks')

  // 状態も加えて絞り込む。
  await page.getByLabel('状態').selectOption({ label: 'Cancelled' })
  await expect(page).toHaveURL(`/tasks?status=cancelled&projectId=${PROJECTS[0].id}`)
  await expect(taskRows(page)).toHaveCount(1)
  await expect(taskRows(page).first()).toContainText(cancelledAlpha.title)
  await expect(taskRows(page).first()).toContainText('Cancelled')

  // 状態だけ（Queued）にすると両プロジェクトの Queued が出て、Cancelled は出ない。
  await page.getByLabel('プロジェクト').selectOption({ label: 'すべて' })
  await page.getByLabel('状態').selectOption({ label: 'Queued' })
  await expect(page).toHaveURL('/tasks?status=queued')
  await expect(taskRows(page).filter({ hasText: queuedBeta.title })).toHaveCount(1)
  await expect(taskRows(page).filter({ hasText: queuedAlpha.title })).toHaveCount(1)
  await expect(taskRows(page).filter({ hasText: cancelledAlpha.title })).toHaveCount(0)
  await expect(taskRows(page)).toHaveCount((await listTasks(server, '&status=queued')).length)

  // 条件は URL に残り、再読み込みしても保たれる。
  await page.reload()
  await expect(page.getByLabel('状態')).toHaveValue('queued')
  await expect(taskRows(page).filter({ hasText: cancelledAlpha.title })).toHaveCount(0)

  await page.getByRole('button', { name: '絞り込みを解除' }).click()
  await expect(page).toHaveURL('/tasks')
  await expect(taskRows(page).filter({ hasText: cancelledAlpha.title })).toHaveCount(1)
})
