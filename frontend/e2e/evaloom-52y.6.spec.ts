import { spawnSync } from 'node:child_process'
import { mkdirSync, mkdtempSync, realpathSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { expect, test as base, type Page } from '@playwright/test'

import { loginOwner, run, setupOwner, startEvaloomd, type Evaloomd } from './evaloomd.ts'
import { createRepo, tagProfile, type Repo } from '../../scripts/fixtures.mts'

// evaloom-52y.6: Blocked の確認と続行・中止（13 3.4、04 3.8）。
//
// 52y.5 と同じく、global-setup がビルドしたバイナリを空きポートで別に起動する。
// Runner の役は、テストが Runner 用 API（claim・結果の報告）を直接呼んで務め、タスクを Blocked にする。

const SCREENSHOTS = fileURLToPath(new URL('../../docs/design/screenshots/', import.meta.url))
const HARNESS = 'claude-developer'
const PROJECTS = [
  { id: 'alpha-app', name: 'Alpha アプリ' },
  { id: 'beta-app', name: 'Beta アプリ' },
] as const

type Server = {
  origin: string
  cookie: string
  runnerToken: string
}

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

async function login(origin: string): Promise<string> {
  const response = await loginOwner(origin)
  const cookie = /evaloom_session=([^;]+)/.exec(response.headers.getSetCookie().join('\n'))?.[1]
  if (!response.ok || !cookie) throw new Error(`ログインに失敗した: ${response.status}`)
  return cookie
}

/** 本人用 API をセッション Cookie で呼ぶ（テストの前提づくりと確認用）。evaloomd は Origin も照合する。 */
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

type TaskDetail = { id: string; title: string; status: string; executions: { id: string; seq: number; mode: string; status: string }[] }

async function createTask(server: Server, projectId: string, title: string): Promise<TaskDetail> {
  return (await owner(
    server,
    'POST',
    '/api/tasks',
    { projectId, title, body: `${title} の本文`, acceptanceCriteria: `${title} が動く` },
    { 'Idempotency-Key': crypto.randomUUID() },
  )) as TaskDetail
}

const getTask = async (server: Server, id: string) => (await owner(server, 'GET', `/api/tasks/${id}`)) as TaskDetail

/** Runner として次の仕事を claim し、取った Execution の ID を返す。 */
async function claim(server: Server): Promise<{ executionId: string; taskId: string }> {
  const response = await fetch(`${server.origin}/api/runners/local/claim`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${server.runnerToken}`, Accept: 'application/json' },
  })
  const text = await response.text()
  expect(response.status, `claim: ${text}`).toBe(200)
  const body = JSON.parse(text) as { execution: { id: string; taskId: string } }
  return { executionId: body.execution.id, taskId: body.execution.taskId }
}

/** Runner として工程の結果を報告する（POST /api/executions/{id}/result）。 */
async function reportResult(server: Server, executionId: string, body: Record<string, unknown>) {
  const response = await fetch(`${server.origin}/api/executions/${executionId}/result`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${server.runnerToken}`, 'Content-Type': 'application/json', Accept: 'application/json' },
    body: JSON.stringify(body),
  })
  expect(response.status, `result: ${await response.text()}`).toBe(204)
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
      const temp = realpathSync(mkdtempSync(join(tmpdir(), 'evaloom-e2e-52y6-')))
      const db = join(temp, 'evaloom.db')
      const harness = createRepo(temp, 'harness')
      tagProfile(harness.path, '1.0.0', 'first')
      // runner register はトークンを一度だけ標準エラーに出す。
      const registered = spawnSync(binary, ['-db', db, 'runner', 'register', '-worktree-root', join(temp, 'worktrees'), '-name', 'ローカル Runner', 'local'], { encoding: 'utf8' })
      const runnerToken = registered.stderr.trim()
      if (registered.status !== 0 || !/^[A-Za-z0-9_-]{43}$/.test(runnerToken)) throw new Error(`runner register が失敗した: ${registered.stderr}`)
      seedHarness(db, harness)
      setupOwner(binary, db)
      const evaloomd: Evaloomd = await startEvaloomd(binary, db, '0')
      try {
        const server: Server = { origin: evaloomd.origin, cookie: await login(evaloomd.origin), runnerToken }
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
  storageState: async ({ worker }, provide) => provide({ cookies: [sessionCookie(worker.cookie)], origins: [] }),
})

/** Tasks 一覧から件名のリンクでタスク詳細を開く。 */
async function openTask(page: Page, task: { id: string; title: string }) {
  await page.goto('/tasks')
  await page.getByRole('link', { name: task.title }).click()
  await expect(page).toHaveURL(`/tasks/${task.id}`)
  await expect(page.getByRole('heading', { name: task.title })).toBeVisible()
}

const BASE_SHA = 'a'.repeat(40)
const HEAD_SHA = '1234567890abcdef1234567890abcdef12345678'

type Gate = { gate: number; result: string; baseSha: string; targetSha: string; findings: { rule: string; path: string; line: number; message: string }[] }

/**
 * Runner として claim から Blocked までを報告する。
 * gate を渡すと、その Gate の結果（result=blocked）と Block を同じ報告で送る（Runner の gate2.go・gate3.go と同じ形）。
 */
async function blockTask(server: Server, taskId: string, block: { cause: string; headSha: string }, gate?: Gate): Promise<string> {
  const { executionId, taskId: claimed } = await claim(server)
  expect(claimed).toBe(taskId)
  await reportResult(server, executionId, { fromStatus: 'claimed', status: 'preparing' })
  await reportResult(server, executionId, { fromStatus: 'preparing', status: 'running' })
  await reportResult(server, executionId, { fromStatus: 'running', status: 'verifying', baseSha: BASE_SHA, harnessHeadSha: gate?.targetSha ?? HEAD_SHA })
  await reportResult(server, executionId, { fromStatus: 'verifying', status: 'blocked', gateResults: gate ? [gate] : [], block })
  return executionId
}

const reviewPanel = (page: Page) => page.locator('[data-slot=card]').filter({ has: page.getByRole('heading', { name: /^Blocked の確認/ }) })

test('Block 原因と Finding を表示し、続行の送信に表示中の headSha が含まれる', async ({ page, server }) => {
  const task = await createTask(server, PROJECTS[0].id, 'Alpha の Blocked 続行')
  const executionId = await blockTask(server, task.id, { cause: 'gate2_missing_tests', headSha: HEAD_SHA }, {
    gate: 2,
    result: 'blocked',
    baseSha: BASE_SHA,
    targetSha: HEAD_SHA,
    findings: [
      { rule: 'existing_test_not_run', path: 'src/login.test.ts', line: 12, message: '既存のテスト "login works" が実行されていない' },
      { rule: 'existing_test_not_run', path: 'src/session.test.ts', line: 0, message: '既存のテスト "session expires" が実行されていない' },
    ],
  })
  await openTask(page, task)
  const panel = reviewPanel(page)
  await expect(panel.getByRole('heading', { name: 'Blocked の確認（#1）' })).toBeVisible()
  await expect(panel).toContainText('gate2_missing_tests')
  await expect(panel.getByTestId('block-head-sha')).toHaveText(HEAD_SHA)
  const findings = panel.getByRole('list', { name: 'Finding' })
  await expect(findings).toContainText('src/login.test.ts:12')
  await expect(findings).toContainText('既存のテスト "login works" が実行されていない')
  await expect(findings).toContainText('既存のテスト "session expires" が実行されていない')
  // 成果物の一覧を返す API が無いので、差分の欄はその旨を出す（仕様の穴として報告済み）。
  await expect(panel).toContainText('成果物（差分）の一覧を返す API が無いため')

  mkdirSync(SCREENSHOTS, { recursive: true })
  await page.screenshot({ path: join(SCREENSHOTS, 'evaloom-52y.6-blocked.png'), fullPage: true })

  const displayed = await panel.getByTestId('block-head-sha').textContent()
  const request = page.waitForRequest((r) => r.url().endsWith(`/api/tasks/${task.id}/resume`) && r.method() === 'POST')
  await panel.getByRole('button', { name: '続行する' }).click()
  const body = (await request).postDataJSON() as { blockId: string; decision: string; headSha: string }
  expect(body.decision).toBe('continue')
  expect(body.headSha).toBe(displayed)
  expect(body.blockId).toMatch(/^[0-9A-HJKMNP-TV-Z]{26}$/)
  await expect(page.getByRole('status').filter({ hasText: '続行を受け付けました。' })).toBeVisible()
  await expect(page.getByRole('heading', { name: /^Blocked の確認/ })).toHaveCount(0)
  const saved = await getTask(server, task.id)
  expect(saved.status).toBe('queued')
  expect(saved.executions.map((e) => [e.id, e.status])).toEqual([[executionId, 'pending']])

  // 後のテストの claim 対象にしないよう、再開した Execution を取って中止にしておく。
  const again = await claim(server)
  expect(again).toEqual({ executionId, taskId: task.id })
  await reportResult(server, executionId, { fromStatus: 'claimed', status: 'cancelled' })
})

test('Gate 3 の Block を中止すると、タスクが Cancelled になる', async ({ page, server }) => {
  const task = await createTask(server, PROJECTS[1].id, 'Beta の Blocked 中止')
  const observed = 'fedcba0987654321fedcba0987654321fedcba09'
  await blockTask(server, task.id, { cause: 'gate3_mismatch', headSha: observed }, {
    gate: 3,
    result: 'blocked',
    baseSha: BASE_SHA,
    targetSha: observed,
    findings: [{ rule: 'uncommitted_changes', path: 'README.md', line: 0, message: '未コミットの変更がある' }],
  })
  await openTask(page, task)
  const panel = reviewPanel(page)
  await expect(panel).toContainText('gate3_mismatch')
  await expect(panel.getByTestId('block-head-sha')).toHaveText(observed)
  await expect(panel.getByRole('list', { name: 'Finding' })).toContainText('未コミットの変更がある')

  const request = page.waitForRequest((r) => r.url().endsWith(`/api/tasks/${task.id}/resume`))
  await panel.getByRole('button', { name: '中止する' }).click()
  expect((await request).postDataJSON()).toMatchObject({ decision: 'abort', headSha: observed })
  await expect(page.getByRole('status').filter({ hasText: '中止を受け付けました。' })).toBeVisible()
  await expect(page.getByRole('heading', { name: /^Blocked の確認/ })).toHaveCount(0)
  expect((await getTask(server, task.id)).status).toBe('cancelled')
})

test('context_conflict の Block では続行ボタンが出ず、中止と再実行だけが出る', async ({ page, server }) => {
  const task = await createTask(server, PROJECTS[0].id, 'Alpha の Context 矛盾')
  await blockTask(server, task.id, { cause: 'context_conflict', headSha: HEAD_SHA })
  await openTask(page, task)
  const panel = reviewPanel(page)
  await expect(panel).toContainText('context_conflict')
  await expect(panel).toContainText('この原因の Block は続行できません')
  await expect(page.getByRole('button', { name: /続行/ })).toHaveCount(0)
  await expect(page.getByRole('button', { name: '停止（中止）' })).toBeVisible()
  await expect(page.getByRole('button', { name: '再実行（継続）' })).toBeVisible()
  await expect(page.getByRole('button', { name: '再実行（最初から）' })).toBeVisible()

  await page.getByRole('button', { name: '停止（中止）' }).click()
  await expect(page.getByRole('status').filter({ hasText: '中止を受け付けました。' })).toBeVisible()
  await expect(page.getByRole('heading', { name: /^Blocked の確認/ })).toHaveCount(0)
  expect((await getTask(server, task.id)).status).toBe('cancelled')
})

test('続行が 422（SHA 不一致）になると、内容が変わったことを表示して読み直させる', async ({ page, server }) => {
  const task = await createTask(server, PROJECTS[1].id, 'Beta の SHA 不一致')
  // Gate の結果の SHA（画面に表示する SHA）と、Block に記録された SHA が食い違っている。
  const recorded = '0'.repeat(39) + '1'
  await blockTask(server, task.id, { cause: 'gate2_unverifiable', headSha: recorded }, {
    gate: 2,
    result: 'blocked',
    baseSha: BASE_SHA,
    targetSha: HEAD_SHA,
    findings: [{ rule: 'test_report_missing', path: '', line: 0, message: 'テスト結果または基準値を照合できない' }],
  })
  await openTask(page, task)
  const panel = reviewPanel(page)
  await expect(panel.getByTestId('block-head-sha')).toHaveText(HEAD_SHA)
  const response = page.waitForResponse((r) => r.url().endsWith(`/api/tasks/${task.id}/resume`))
  await panel.getByRole('button', { name: '続行する' }).click()
  expect((await response).status()).toBe(422)
  const stale = page.getByTestId('block-stale')
  await expect(stale).toContainText('内容が変わりました')
  await expect(stale).toContainText('画面を読み直して')

  const reloaded = page.waitForResponse((r) => r.url().includes('/api/executions/') && !r.url().endsWith('/events') && r.request().method() === 'GET')
  await stale.getByRole('button', { name: '画面を読み直す' }).click()
  await reloaded
  await expect(stale).toHaveCount(0)
  await expect(panel.getByTestId('block-head-sha')).toHaveText(HEAD_SHA)
  // サーバー側は何も変わっていない（判断は記録されない）。
  expect((await getTask(server, task.id)).status).toBe('blocked')
})
