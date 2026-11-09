import { spawnSync } from 'node:child_process'
import { mkdirSync, mkdtempSync, realpathSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { expect, test as base, type Page } from '@playwright/test'

import { loginOwner, run, setupOwner, startEvaloomd, type Evaloomd } from './evaloomd.ts'
import { createRepo, tagProfile, type Repo } from '../../scripts/fixtures.mts'

// evaloom-52y.7: Dashboard と Settings（13 3.2・4節）。
//
// 52y.6 と同じく、global-setup がビルドしたバイナリを空きポートで別に起動する。
// Runner の役は、テストが Runner 用 API（heartbeat・claim・結果の報告）を直接呼んで務め、
// 実行中・待機中・Blocked のタスクと、online / offline の Runner を作る。

const SCREENSHOTS = fileURLToPath(new URL('../../docs/design/screenshots/', import.meta.url))
const HARNESS = 'claude-developer'
const PROJECTS = [
  { id: 'alpha-app', name: 'Alpha アプリ' },
  { id: 'beta-app', name: 'Beta アプリ' },
] as const

type Server = {
  db: string
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
      const temp = realpathSync(mkdtempSync(join(tmpdir(), 'evaloom-e2e-52y7-')))
      const db = join(temp, 'evaloom.db')
      const harness = createRepo(temp, 'harness')
      tagProfile(harness.path, '1.0.0', 'first')
      // runner register はトークンを一度だけ標準エラーに出す。
      const registered = spawnSync(binary, ['-db', db, 'runner', 'register', '-worktree-root', join(temp, 'worktrees'), '-name', 'ローカル Runner', 'local'], { encoding: 'utf8' })
      const runnerToken = registered.stderr.trim()
      if (registered.status !== 0 || !/^[A-Za-z0-9_-]{43}$/.test(runnerToken)) throw new Error(`runner register が失敗した: ${registered.stderr}`)
      const spare = spawnSync(binary, ['-db', db, 'runner', 'register', '-worktree-root', join(temp, 'spare-worktrees'), '-name', '予備 Runner', 'spare'], { encoding: 'utf8' })
      if (spare.status !== 0) throw new Error(`runner register が失敗した: ${spare.stderr}`)
      seedHarness(db, harness)
      setupOwner(binary, db)
      const evaloomd: Evaloomd = await startEvaloomd(binary, db, '0')
      try {
        const server: Server = { db, origin: evaloomd.origin, cookie: await login(evaloomd.origin), runnerToken }
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

/** Runner として生存報告を送る（POST /api/runners/{id}/heartbeat）。送った Runner は online になる。 */
async function heartbeat(server: Server) {
  const response = await fetch(`${server.origin}/api/runners/local/heartbeat`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${server.runnerToken}`, 'Content-Type': 'application/json', Accept: 'application/json' },
    body: JSON.stringify({ bootId: 'e2e-boot', runningExecutionIds: [] }),
  })
  expect(response.status, `heartbeat: ${await response.text()}`).toBe(200)
}

const HEAD_SHA = '1234567890abcdef1234567890abcdef12345678'

/** claim した Execution を Runner として running まで進める。 */
async function start(server: Server, taskId: string): Promise<string> {
  const { executionId, taskId: claimed } = await claim(server)
  expect(claimed).toBe(taskId)
  await reportResult(server, executionId, { fromStatus: 'claimed', status: 'preparing' })
  await reportResult(server, executionId, { fromStatus: 'preparing', status: 'running' })
  return executionId
}

/**
 * 通知失敗を記録する。evaloomd の macOS Notifier の成否は実行環境（通知ヘルパーの配置・許可）で変わるので、
 * Control Plane の Dispatcher が書くのと同じ形の notification_failed イベント（02-data-model 5.3、12 3.4。
 * kind は要求の Execution 状態）を execution_events へ直接入れ、結果を環境に左右されないようにする。
 */
function recordNotificationFailure(server: Server, executionId: string, payload: { kind: string; channel: string; reason: string }) {
  run('sqlite3', [
    server.db,
    `INSERT INTO execution_events (execution_id, type, payload, created_at)
      VALUES ('${executionId}', 'notification_failed', '${JSON.stringify(payload)}', '${new Date().toISOString()}');`,
  ])
}

const count = (page: Page, testId: string) => page.getByTestId(testId).locator('dd')

test('Dashboard に Runner の online / offline と、実行中・待機中・Blocked の件数が表示される', async ({ page, server }) => {
  // local は heartbeat を送って online、spare は登録しただけなので offline（store.RegisterRunner）。
  await heartbeat(server)

  // Beta のタスクを Blocked にする。Blocked は同時実行の枠を占有しない（04 3.3、Q23）。
  const blocked = await createTask(server, PROJECTS[1].id, 'Beta の Blocked タスク')
  const blockedExecution = await start(server, blocked.id)
  await reportResult(server, blockedExecution, { fromStatus: 'running', status: 'verifying', baseSha: 'a'.repeat(40), harnessHeadSha: HEAD_SHA })
  await reportResult(server, blockedExecution, { fromStatus: 'verifying', status: 'blocked', gateResults: [], block: { cause: 'context_conflict', headSha: HEAD_SHA } })

  // Beta のタスクを失敗させ、その通知に失敗したことを記録する。
  const failed = await createTask(server, PROJECTS[1].id, 'Beta の失敗タスク')
  const failedExecution = await start(server, failed.id)
  await reportResult(server, failedExecution, { fromStatus: 'running', status: 'failed', errorClass: 'execution_failed' })
  recordNotificationFailure(server, failedExecution, { kind: 'failed', channel: 'macos', reason: 'permission_denied' })

  // Alpha のタスクを実行中にする。同一プロジェクトは1本までなので、続く Alpha の2件は待機中のまま残る。
  const running = await createTask(server, PROJECTS[0].id, 'Alpha の実行中タスク')
  await start(server, running.id)
  const waiting = [await createTask(server, PROJECTS[0].id, 'Alpha の待機中タスク 1'), await createTask(server, PROJECTS[0].id, 'Alpha の待機中タスク 2')]
  expect((await getTask(server, blocked.id)).status).toBe('blocked')
  expect((await getTask(server, running.id)).status).toBe('running')
  for (const task of waiting) expect((await getTask(server, task.id)).status).toBe('queued')

  // 通知失敗は一覧 API（12 5、Q41）で読む。
  const requested: string[] = []
  page.on('request', (request) => requested.push(new URL(request.url()).pathname))

  await page.goto('/projects')
  await page.getByRole('navigation', { name: '画面' }).getByRole('link', { name: 'Dashboard' }).click()
  await expect(page).toHaveURL('/')
  await expect(page.getByRole('heading', { name: 'Dashboard' })).toBeVisible()

  await expect(count(page, 'runner-count-online')).toHaveText('1件')
  await expect(count(page, 'runner-count-offline')).toHaveText('1件')
  const runners = page.getByRole('table', { name: 'Runner' })
  await expect(runners.getByRole('row').filter({ hasText: 'ローカル Runner' })).toContainText('online')
  await expect(runners.getByRole('row').filter({ hasText: '予備 Runner' })).toContainText('offline')

  await expect(count(page, 'task-count-running')).toHaveText('1件')
  await expect(count(page, 'task-count-waiting')).toHaveText('2件')
  await expect(count(page, 'task-count-blocked')).toHaveText('1件')
  await expect(page.getByRole('list', { name: '実行中のタスク' })).toContainText('Alpha の実行中タスク')
  await expect(page.getByRole('list', { name: '待機中のタスク' })).toContainText('Alpha の待機中タスク 1')
  await expect(page.getByRole('list', { name: '待機中のタスク' })).toContainText('Alpha の待機中タスク 2')

  // エラー状況: 失敗したタスクと、その通知失敗。
  await expect(count(page, 'task-count-error')).toHaveText('1件')
  await expect(page.getByRole('list', { name: '失敗したタスク' })).toContainText('Beta の失敗タスク')
  // 実環境の Notifier も失敗を記録しうる（ヘルパー未配置なら launch_failed）ので、入れた1件を内容で特定する。
  const notification = page.getByRole('list', { name: '通知失敗' }).getByRole('listitem').filter({ hasText: 'reason: permission_denied' })
  await expect(notification).toHaveCount(1)
  await expect(notification).toContainText('Beta の失敗タスク')
  await expect(notification).toContainText('通知が許可されていない')
  await expect(notification).toContainText('kind: failed')
  await expect(notification).toContainText('channel: macos')
  await expect(count(page, 'notification-failure-count')).not.toHaveText('0件')
  expect(requested).toContain('/api/notification-failures')
  // まだ動きうるタスクの SSE は状態の追従（Tasks 一覧と同じ購読）で開くが、通知失敗のために失敗済みの Execution の SSE は開かない。
  expect(requested).not.toContain(`/api/executions/${failedExecution}/events`)

  mkdirSync(SCREENSHOTS, { recursive: true })
  await page.screenshot({ path: join(SCREENSHOTS, 'evaloom-52y.7-dashboard.png'), fullPage: true })

  // 新しい通知失敗を 50 件足すと、上の permission_denied は 1ページ目（既定 50 件、01 5.3）から外れる。
  // 「さらに表示」で nextCursor の続きを読むと、欠けずに 1 件だけ出る（それより古い実環境の通知失敗も2ページ目に入る）。
  for (let i = 0; i < 50; i++) recordNotificationFailure(server, failedExecution, { kind: 'failed', channel: 'macos', reason: 'timeout' })
  await page.getByRole('button', { name: '再読み込み' }).click()
  const failures = page.getByRole('list', { name: '通知失敗' }).getByRole('listitem')
  await expect(failures.filter({ hasText: 'reason: timeout' })).toHaveCount(50)
  await expect(notification).toHaveCount(0)
  await expect(count(page, 'notification-failure-count')).toContainText('以上')
  await page.getByRole('button', { name: 'さらに表示' }).click()
  await expect(notification).toHaveCount(1)
  await expect(notification).toContainText('Beta の失敗タスク')
  await expect(failures.filter({ hasText: 'reason: timeout' })).toHaveCount(50)

  // Blocked のタスクからタスク詳細へ移れる。
  await page.getByRole('list', { name: 'Blocked のタスク' }).getByRole('link', { name: 'Beta の Blocked タスク' }).click()
  await expect(page).toHaveURL(`/tasks/${blocked.id}`)
})

test('Settings に同時実行の上限が表示されるが、変更する入力欄は無い', async ({ page, server }) => {
  await heartbeat(server)
  await page.goto('/')
  await page.getByRole('navigation', { name: '画面' }).getByRole('link', { name: 'Settings' }).click()
  await expect(page).toHaveURL('/settings')
  await expect(page.getByRole('heading', { name: 'Settings' })).toBeVisible()

  await expect(page.getByTestId('concurrency-global').locator('dd')).toHaveText('2 本')
  await expect(page.getByTestId('concurrency-per-project').locator('dd')).toHaveText('1 本')
  await expect(page.getByText('表示のみです。画面からは変更できません。')).toBeVisible()
  // Linear のラベル（evaloom-iao.10、15 5章）は編集できるので、同時実行の上限の欄に入力欄もボタンも無いことを確かめる。
  const main = page.getByTestId('settings-concurrency')
  await expect(main.getByRole('textbox')).toHaveCount(0)
  await expect(main.getByRole('spinbutton')).toHaveCount(0)
  await expect(main.getByRole('combobox')).toHaveCount(0)
  await expect(main.getByRole('button')).toHaveCount(0)
  // 数字を直接クリックしても編集にならない。
  await page.getByTestId('concurrency-global').locator('dd').click()
  await page.keyboard.type('5')
  await expect(page.getByTestId('concurrency-global').locator('dd')).toHaveText('2 本')

  const runners = page.getByRole('table', { name: 'Runner' })
  await expect(runners.getByRole('row').filter({ hasText: 'ローカル Runner' })).toContainText('online')
  await expect(runners.getByRole('row').filter({ hasText: '予備 Runner' })).toContainText('offline')

  mkdirSync(SCREENSHOTS, { recursive: true })
  await page.screenshot({ path: join(SCREENSHOTS, 'evaloom-52y.7-settings.png'), fullPage: true })
})
