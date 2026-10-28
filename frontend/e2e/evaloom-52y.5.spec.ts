import { spawnSync } from 'node:child_process'
import { mkdirSync, mkdtempSync, realpathSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { expect, test as base, type BrowserContext, type Page } from '@playwright/test'

import { run, startEvaloomd, type Evaloomd } from './evaloomd.ts'
import { createRepo, tagProfile, type Repo } from '../../scripts/fixtures.mts'

// evaloom-52y.5: タスク詳細（Execution 履歴、SSE ログ、中止・再実行）。
//
// 52y.4 と同じく、global-setup がビルドしたバイナリを空きポートで別に起動する。
// Runner の役は、テストが Runner 用 API（claim・ログの報告）を直接呼んで務める。
// 再接続の確認では evaloomd を同じポート・同じ DB で再起動して、表示中の SSE を実際に切る。

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
  /** evaloomd を同じポート・同じ DB で起動し直し、ログインし直す（セッションはプロセスのメモリにだけあるため）。 */
  restart: () => Promise<void>
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

async function login(loginURL: string): Promise<string> {
  const response = await fetch(loginURL, { headers: { Accept: 'application/json' } })
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

let sourceSeq = 0
/** Runner としてログを1行ずつ報告する（POST /api/executions/{id}/events）。 */
async function reportLogs(server: Server, executionId: string, stream: string, lines: string[]) {
  const events = lines.map((text) => ({ id: `e2e-${++sourceSeq}`, stream, text }))
  const response = await fetch(`${server.origin}/api/executions/${executionId}/events`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${server.runnerToken}`, 'Content-Type': 'application/json', Accept: 'application/json' },
    body: JSON.stringify({ events }),
  })
  expect(response.status, `log report: ${await response.text()}`).toBe(204)
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
      const temp = realpathSync(mkdtempSync(join(tmpdir(), 'evaloom-e2e-52y5-')))
      const db = join(temp, 'evaloom.db')
      const harness = createRepo(temp, 'harness')
      tagProfile(harness.path, '1.0.0', 'first')
      // runner register はトークンを一度だけ標準エラーに出す。
      const registered = spawnSync(binary, ['-db', db, 'runner', 'register', '-worktree-root', join(temp, 'worktrees'), '-name', 'ローカル Runner', 'local'], { encoding: 'utf8' })
      const runnerToken = registered.stderr.trim()
      if (registered.status !== 0 || !/^[A-Za-z0-9_-]{43}$/.test(runnerToken)) throw new Error(`runner register が失敗した: ${registered.stderr}`)
      seedHarness(db, harness)
      let evaloomd: Evaloomd = await startEvaloomd(binary, db, '0')
      try {
        const server: Server = {
          origin: evaloomd.origin,
          cookie: await login(evaloomd.loginURL),
          runnerToken,
          restart: async () => {
            await evaloomd.stop()
            evaloomd = await startEvaloomd(binary, db, new URL(server.origin).port)
            server.cookie = await login(evaloomd.loginURL)
          },
        }
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

const historyRows = (page: Page) => page.getByRole('table', { name: 'Execution 履歴' }).locator('tbody tr')
const logLines = (page: Page) => page.getByRole('log').locator('[data-log-line]')

test('再実行で新しい Execution が履歴に増える（継続・最初から）', async ({ page, server }) => {
  const task = await createTask(server, PROJECTS[0].id, 'Alpha の再実行')
  await openTask(page, task)
  await expect(historyRows(page)).toHaveCount(1)
  await expect(historyRows(page).first()).toContainText('#1')
  await expect(historyRows(page).first()).toContainText('pending')

  // Queued のタスクを止める。
  await page.getByRole('button', { name: '停止（中止）' }).click()
  await expect(page.getByRole('status').filter({ hasText: '中止を受け付けました。' })).toBeVisible()
  await expect(historyRows(page).first()).toContainText('cancelled')
  await expect(page.getByRole('button', { name: '停止（中止）' })).toHaveCount(0)

  // 継続で再実行すると、#2 が履歴に増えて表示が #2 に移る。
  await page.getByRole('button', { name: '再実行（継続）' }).click()
  await expect(page.getByRole('status').filter({ hasText: '再実行（継続）を受け付けました。' })).toBeVisible()
  await expect(historyRows(page)).toHaveCount(2)
  await expect(historyRows(page).first()).toContainText('#2')
  await expect(historyRows(page).first()).toContainText('継続')
  await expect(historyRows(page).first()).toContainText('pending')
  await expect(historyRows(page).nth(1)).toContainText('#1')
  await expect(historyRows(page).nth(1)).toContainText('cancelled')
  await expect(page.getByRole('heading', { name: 'ログ（#2）' })).toBeVisible()

  // 最初からの再実行は確認ダイアログを出す。やめると送らない。
  await page.getByRole('button', { name: '停止（中止）' }).click()
  await expect(historyRows(page).first()).toContainText('cancelled')
  await page.getByRole('button', { name: '再実行（最初から）' }).click()
  const dialog = page.getByRole('alertdialog', { name: '最初から再実行しますか' })
  await expect(dialog).toBeVisible()
  await dialog.getByRole('button', { name: 'やめる' }).click()
  await expect(dialog).toHaveCount(0)
  expect((await getTask(server, task.id)).executions).toHaveLength(2)

  await page.getByRole('button', { name: '再実行（最初から）' }).click()
  await page.getByRole('alertdialog').getByRole('button', { name: '最初から再実行する' }).click()
  await expect(historyRows(page)).toHaveCount(3)
  await expect(historyRows(page).first()).toContainText('#3')
  await expect(historyRows(page).first()).toContainText('最初から')
  const saved = await getTask(server, task.id)
  expect(saved.executions.map((e) => [e.seq, e.mode, e.status])).toEqual([
    [1, 'continue', 'cancelled'],
    [2, 'continue', 'cancelled'],
    [3, 'fresh', 'pending'],
  ])

  // 再読み込みしても3件のまま。後のテストの claim 対象にしないよう止めておく。
  await page.reload()
  await expect(historyRows(page)).toHaveCount(3)
  await page.getByRole('button', { name: '停止（中止）' }).click()
  await expect(historyRows(page).first()).toContainText('cancelled')
})

test('実行中のログが逐次表示され、終了すると保存済みログに切り替わる', async ({ page, server }) => {
  const task = await createTask(server, PROJECTS[0].id, 'Alpha のログ表示')
  const { executionId, taskId } = await claim(server)
  expect(taskId).toBe(task.id)
  await openTask(page, task)
  await expect(page.getByTestId('log-source')).toContainText('受信中')
  await expect(page.getByRole('log')).toContainText('まだログはありません。')
  // 画面を読み直していないことの目印。
  await page.evaluate(() => Object.assign(globalThis, { __notReloaded: true }))

  await reportLogs(server, executionId, 'harness', ['npm ci を実行しています'])
  await expect(logLines(page)).toHaveText(['npm ci を実行しています'])
  await reportLogs(server, executionId, 'harness', ['ログイン画面を実装しています'])
  await expect(logLines(page)).toHaveText(['npm ci を実行しています', 'ログイン画面を実装しています'])
  await reportLogs(server, executionId, 'harness', ['テストを追加しました', 'テストを実行しています'])
  await expect(logLines(page)).toHaveText(['npm ci を実行しています', 'ログイン画面を実装しています', 'テストを追加しました', 'テストを実行しています'])

  // stream で切り替える。
  await reportLogs(server, executionId, 'runner', ['worktree を準備しました'])
  await expect(page.getByRole('tab', { name: 'runner (1)' })).toBeVisible()
  await page.getByRole('tab', { name: 'runner (1)' }).click()
  await expect(logLines(page)).toHaveText(['worktree を準備しました'])
  await page.getByRole('tab', { name: 'harness (4)' }).click()
  await expect(logLines(page)).toHaveCount(4)
  expect(await page.evaluate(() => (globalThis as { __notReloaded?: boolean }).__notReloaded)).toBe(true)

  mkdirSync(SCREENSHOTS, { recursive: true })
  await page.screenshot({ path: join(SCREENSHOTS, 'evaloom-52y.5-task-detail.png'), fullPage: true })

  // Runner が Execution を終了（cancelled）にする。
  // E2E の evaloomd は成果物の保存先（-data-dir）を持たないので、harness のログファイルは登録しない。
  // 保存済みログ（GET /api/executions/{id}/logs）では harness は空、runner は保存済みのイベントから返る。
  await reportResult(server, executionId, { fromStatus: 'claimed', status: 'cancelled' })
  // 画面を読み直さずに、SSE の追記表示から保存済みログの表示へ切り替わる。
  await expect(page.getByTestId('log-source')).toHaveText('保存済みのログを表示しています')
  await expect(historyRows(page).first()).toContainText('cancelled')
  await expect(page.getByRole('tab', { name: 'harness (0)' })).toBeVisible()
  await page.getByRole('tab', { name: 'harness (0)' }).click()
  await expect(page.getByRole('log')).toHaveText('この stream の保存済みログはありません。')
  await page.getByRole('tab', { name: 'runner (1)' }).click()
  await expect(logLines(page)).toHaveText(['worktree を準備しました'])
  expect(await page.evaluate(() => (globalThis as { __notReloaded?: boolean }).__notReloaded)).toBe(true)
})

test('切断から再接続すると、欠けなく続きが表示される', async ({ page, context, server }) => {
  const task = await createTask(server, PROJECTS[1].id, 'Beta の再接続')
  const { executionId, taskId } = await claim(server)
  expect(taskId).toBe(task.id)
  await openTask(page, task)
  await expect(page.getByTestId('log-source')).toContainText('受信中')

  const before = ['1 行目', '2 行目', '3 行目']
  await reportLogs(server, executionId, 'harness', before)
  await expect(logLines(page)).toHaveText(before)

  const reconnects: (string | undefined)[] = []
  page.on('response', (response) => {
    const url = new URL(response.url())
    if (url.pathname === `/api/executions/${executionId}/events` && response.status() === 200 && response.request().resourceType() === 'fetch') {
      reconnects.push(response.request().headers()['last-event-id'])
    }
  })

  // evaloomd を止めて SSE を切る。再起動してログインし直すまで、画面からの再接続は通さない。
  await blockEvents(page, context, server, async () => {
    await server.restart()
    await expect(page.getByTestId('log-source')).toContainText('再接続しています')
    // 切断中にも Runner はログを報告する。
    await reportLogs(server, executionId, 'harness', ['4 行目', '5 行目'])
  })
  await expect(logLines(page)).toHaveText([...before, '4 行目', '5 行目'], { timeout: 20_000 })
  await expect(page.getByTestId('log-source')).toContainText('受信中')
  // 再接続は Last-Event-ID 付きで、続きから受け取っている（最初から受け直していない）。
  expect(reconnects.length).toBeGreaterThanOrEqual(1)
  expect(reconnects.at(-1)).toMatch(/^[1-9][0-9]*$/)

  // 再接続後も逐次表示が続く。
  await reportLogs(server, executionId, 'harness', ['6 行目'])
  await expect(logLines(page)).toHaveText([...before, '4 行目', '5 行目', '6 行目'])
})

/** 再起動中は SSE の要求を通信エラーにし、新しいセッション Cookie をブラウザに入れてから通す。 */
async function blockEvents(page: Page, context: BrowserContext, server: Server, during: () => Promise<void>) {
  const pattern = '**/api/executions/*/events'
  await page.route(pattern, (route) => route.abort('connectionrefused'))
  await during()
  await context.addCookies([sessionCookie(server.cookie)])
  await page.unroute(pattern)
}
