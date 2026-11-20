import { spawnSync } from 'node:child_process'
import { mkdirSync, mkdtempSync, realpathSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { expect, test as base, type Page } from '@playwright/test'

import { loginOwner, run, setupOwner, startEvaloomd } from './evaloomd.ts'

// evaloom-oxe.10: Dashboard と Settings（13 3.2・4節、02 3.8）。
// - Runner の online / offline、実行中・待機中のタスクの件数、判断待ちの件数（種類ごと）が表示される。
// - Linear の照会が続けて失敗しているとき、失敗の分類と最後に成功した時刻が出る。
// - 同時実行の上限が表示されるが変更できない。
// - Settings で 02 3.8 のキーを表示・変更でき、未知のキーは送れない。
//
// global-setup がビルドしたバイナリを一時 DB と空きポートで別に起動する。Runner は runner register で2台登録し、
// 1台だけ Runner 用 API の heartbeat で online にする。タスク・Execution・判断待ち・Linear の照会の状況は、
// 段の報告や Poller の後と同じ形の行を DB へ直接入れ、本物の一覧 API を通して確かめる。

const SCREENSHOTS = fileURLToPath(new URL('../../docs/design/screenshots/', import.meta.url))
const NOW = '2026-11-19T00:00:00Z'
const PROFILE = '01JE2E0P0000000000000000A1'
const ONLINE = 'local'
const OFFLINE = 'spare'
const LAST_SUCCESS = '2026-11-08T00:30:00Z'

const ulid = (kind: string, n: number) => `01JE2E${kind}${String(n).padStart(19, '0')}`
const RUNNING_EXECUTIONS = [ulid('E', 1), ulid('E', 2)]

type Server = { origin: string; cookie: string; db: string; runnerToken: string }

const sqlText = (value: unknown) => `'${(typeof value === 'string' ? value : JSON.stringify(value)).replaceAll("'", "''")}'`

function seed(db: string) {
  const statements: string[] = [
    `INSERT INTO harnesses (id, name, repo_url, repo_path, default_profile_id, approval_waivers, created_at, updated_at)
      VALUES ('e2e-harness', 'E2E ハーネス', 'https://example.com/h.git', '/tmp/e2e-harness', NULL, '[]', '${NOW}', '${NOW}');`,
    `INSERT INTO harness_profiles (id, harness_id, version, commit_sha, tag_name, contract_version, definition, definition_hash, model_id, operation_status, registered_at)
      VALUES ('${PROFILE}', 'e2e-harness', '1.0.0', '${'a'.repeat(40)}', 'v1.0.0', '1', ${sqlText({ description: 'E2E', acceptanceCriteria: 'optional' })}, 'hash', 'model', 'provisional', '${NOW}');`,
    `INSERT INTO projects (id, name, harness_id, profile_id, params, approval_waivers, runner_id, created_at, updated_at)
      VALUES ('e2e-app', 'E2E アプリ', 'e2e-harness', '${PROFILE}', '{}', '[]', '${ONLINE}', '${NOW}', '${NOW}');`,
  ]
  let taskNo = 0
  const task = (status: string, title: string) => {
    taskNo += 1
    const id = `task-${String(taskNo).padStart(3, '0')}`
    statements.push(`INSERT INTO tasks (id, source, project_id, harness_id, title, body, acceptance_criteria, collaboration_mode, status, idempotency_key, created_at, updated_at)
      VALUES ('${id}', 'manual', 'e2e-app', NULL, ${sqlText(title)}, '本文', '条件', 'solo', '${status}', 'idem-${id}', '${NOW}', '${NOW}');`)
    return id
  }
  const execution = (id: string, taskId: string, status: string, runner: string | null) =>
    statements.push(`INSERT INTO executions (id, task_id, seq, mode, status, runner_id, route_kind, project_id, profile_id, profile_version, created_at)
      VALUES ('${id}', '${taskId}', 1, 'continue', '${status}', ${runner ? `'${runner}'` : 'NULL'}, 'project', 'e2e-app', '${PROFILE}', '1.0.0', '${NOW}');`)
  const decision = (n: number, kind: string, taskId: string, executionId: string | null, cause: string | null, status = 'open', subject: string | null = null) =>
    statements.push(`INSERT INTO owner_decisions (id, kind, task_id, execution_id, subject_id, cause, basis, detail, status, withdrawn_reason, created_at)
      VALUES ('${ulid('D', n)}', '${kind}', '${taskId}', ${executionId ? `'${executionId}'` : 'NULL'}, ${subject ? `'${subject}'` : 'NULL'}, ${cause ? `'${cause}'` : 'NULL'}, ${subject ? `'${subject}'` : 'NULL'},
        ${sqlText({ message: `${kind} の判断待ち` })}, '${status}', NULL, '${NOW}');`)

  // 実行中2件（online の Runner が動かしている）、待機中1件。
  execution(RUNNING_EXECUTIONS[0], task('running', '実行中のタスク A'), 'working', ONLINE)
  execution(RUNNING_EXECUTIONS[1], task('running', '実行中のタスク B'), 'checking', ONLINE)
  execution(ulid('E', 3), task('queued', '待機中のタスク'), 'pending', null)

  // 判断待ち: route 1、approval 2、blocked 3、question 1（open）。判断済み1件は数えない。
  // route と approval は対象の行（route_proposals・approvals）を subject_id で指す。
  const routeTask = task('routing', '振り分けを待つタスク')
  statements.push(`INSERT INTO route_proposals (id, task_id, trigger, seen_labels, candidates, status, proposed_label, reason, decision_id, created_at, finished_at)
    VALUES ('${ulid('P', 10)}', '${routeTask}', 'no_label', '[]', '[]', 'proposed', NULL, NULL, '${ulid('D', 10)}', '${NOW}', '${NOW}');`)
  decision(10, 'route', routeTask, null, null, 'open', ulid('P', 10))
  const approvalTask = task('waiting_approval', '承認を待つタスク')
  execution(ulid('E', 20), approvalTask, 'waiting_approval', ONLINE)
  for (const [n, operation] of [
    [20, 'merge_pull_request'],
    [21, 'deploy'],
  ] as const) {
    decision(n, 'approval', approvalTask, ulid('E', 20), null, 'open', ulid('V', n))
    statements.push(`INSERT INTO approvals (id, task_id, execution_id, decision_id, requested_stage, operation, target, summary, content_id, params_hash, status, created_at)
      VALUES ('${ulid('V', n)}', '${approvalTask}', '${ulid('E', 20)}', '${ulid('D', n)}', 'publish', '${operation}', ${sqlText({ label: operation })}, '${operation} の概要',
        '${ulid('V', n)}', '${'f'.repeat(64)}', 'pending', '${NOW}');`)
  }
  for (const [n, cause] of [
    [30, 'check_blocked'],
    [31, 'publish_unknown'],
    [32, 'context_conflict'],
  ] as const) {
    const taskId = task('blocked', `${cause} のタスク`)
    execution(ulid('E', n), taskId, 'blocked', ONLINE)
    decision(n, 'blocked', taskId, ulid('E', n), cause)
  }
  const questionTask = task('blocked', '質問のあるタスク')
  execution(ulid('E', 40), questionTask, 'blocked', ONLINE)
  decision(40, 'question', questionTask, ulid('E', 40), 'question')
  const answeredTask = task('completed', '判断済みのタスク')
  execution(ulid('E', 50), answeredTask, 'succeeded', ONLINE)
  decision(50, 'question', answeredTask, ulid('E', 50), 'question', 'answered')

  run('sqlite3', [db, `BEGIN;\n${statements.join('\n')}\nCOMMIT;`])
}

function register(binary: string, db: string, temp: string, id: string, name: string): string {
  // runner register はトークンを一度だけ標準エラーに出す。
  const registered = spawnSync(binary, ['-db', db, 'runner', 'register', '-worktree-root', join(temp, `worktrees-${id}`), '-name', name, id], { encoding: 'utf8' })
  const token = registered.stderr.trim()
  if (registered.status !== 0 || !/^[A-Za-z0-9_-]{43}$/.test(token)) throw new Error(`runner register が失敗した: ${registered.stderr}`)
  return token
}

/** Runner として生存報告を送る（POST /api/runners/{id}/heartbeat）。送った Runner は online になる。 */
async function heartbeat(server: Server) {
  const response = await fetch(`${server.origin}/api/runners/${ONLINE}/heartbeat`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${server.runnerToken}`, 'Content-Type': 'application/json', Accept: 'application/json' },
    body: JSON.stringify({ bootId: 'e2e-boot', runningExecutionIds: RUNNING_EXECUTIONS }),
  })
  expect(response.status, `heartbeat: ${await response.text()}`).toBe(200)
}

const test = base.extend<{ server: Server }, { worker: Server }>({
  worker: [
    // oxlint-disable-next-line no-empty-pattern
    async ({}, provide) => {
      const binary = process.env.EVALOOMD_BINARY
      if (!binary) throw new Error('EVALOOMD_BINARY が無い（e2e/global-setup.ts が設定する）')
      const temp = realpathSync(mkdtempSync(join(tmpdir(), 'evaloom-e2e-oxe10-')))
      const db = join(temp, 'evaloom.db')
      const runnerToken = register(binary, db, temp, ONLINE, 'ローカル Runner')
      register(binary, db, temp, OFFLINE, '予備機')
      setupOwner(binary, db)
      seed(db)
      const evaloomd = await startEvaloomd(binary, db, '0')
      try {
        const response = await loginOwner(evaloomd.origin)
        const cookie = /evaloom_session=([^;]+)/.exec(response.headers.getSetCookie().join('\n'))?.[1]
        if (!response.ok || !cookie) throw new Error(`ログインに失敗した: ${response.status}`)
        await provide({ origin: evaloomd.origin, cookie, db, runnerToken })
      } finally {
        await evaloomd.stop()
        rmSync(temp, { recursive: true, force: true })
      }
    },
    { scope: 'worker' },
  ],
  server: async ({ worker }, provide) => {
    await heartbeat(worker)
    await provide(worker)
  },
  baseURL: async ({ worker }, provide) => provide(worker.origin),
  storageState: async ({ worker }, provide) =>
    provide({
      cookies: [{ name: 'evaloom_session', value: worker.cookie, domain: '127.0.0.1', path: '/', expires: -1, httpOnly: true, secure: false, sameSite: 'Strict' }],
      origins: [],
    }),
})

async function screenshot(page: Page, screen: string) {
  mkdirSync(SCREENSHOTS, { recursive: true })
  await page.screenshot({ path: join(SCREENSHOTS, `evaloom-oxe.10-${screen}.png`), fullPage: true })
}

test('Dashboard: Runner の online / offline、実行中・待機中の件数、判断待ちの件数（種類ごと）が出る', async ({ page, server }) => {
  expect(server.origin).toBeTruthy()
  await page.goto('/')
  await expect(page.getByTestId('runner-count-online')).toHaveText('online1件')
  await expect(page.getByTestId('runner-count-offline')).toHaveText('offline1件')
  const runners = page.locator('tr[data-runner]')
  await expect(runners.filter({ hasText: 'ローカル Runner' })).toContainText('online')
  await expect(runners.filter({ hasText: '予備機' })).toContainText('offline')

  await expect(page.getByTestId('task-count-running')).toHaveText('実行中2件')
  await expect(page.getByTestId('task-count-waiting')).toHaveText('待機中1件')

  await expect(page.getByTestId('decision-count-total')).toHaveText('合計7件')
  await expect(page.getByTestId('decision-count-route')).toHaveText('振り分けの承認1件')
  await expect(page.getByTestId('decision-count-approval')).toHaveText('操作の承認2件')
  await expect(page.getByTestId('decision-count-blocked')).toHaveText('止まった仕事の続行3件')
  await expect(page.getByTestId('decision-count-question')).toHaveText('作業中の AI の質問1件')
  await page.getByRole('link', { name: '判断待ちの画面を開く' }).click()
  await expect(page).toHaveURL(/\/decisions$/)
  await expect(page.getByRole('list', { name: '判断待ち' }).getByRole('listitem')).toHaveCount(7)
})

test('Dashboard: 続けて失敗している Linear の照会に、失敗の分類と最後に成功した時刻が出る', async ({ page, server }) => {
  // TEAM-OPS は 5 回続けて失敗（15 6章の上限）、TEAM-WEB は直近に成功。Poller の後と同じ行を起動後に入れる。
  // API キーのファイルが無いので、Poller が照会しても last_error は auth のまま、最後に成功した時刻は変わらない。
  run('sqlite3', [
    server.db,
    `INSERT INTO linear_poll_teams (team_id, baseline_done_at, last_success_at, last_error, consecutive_failures)
      VALUES ('TEAM-OPS', '2026-11-07T00:00:00Z', '${LAST_SUCCESS}', 'auth', 5),
             ('TEAM-WEB', '2026-11-07T00:00:00Z', '${new Date().toISOString()}', NULL, 0)
      ON CONFLICT (team_id) DO UPDATE SET last_success_at = excluded.last_success_at, last_error = excluded.last_error,
        consecutive_failures = excluded.consecutive_failures;`,
  ])
  await page.goto('/')
  const failures = page.getByRole('list', { name: 'Linear の照会の失敗' })
  await expect(failures.getByRole('listitem')).toHaveCount(1)
  const item = failures.getByRole('listitem').first()
  await expect(item).toContainText('TEAM-OPS')
  await expect(item).toContainText('（auth）')
  await expect(item).toContainText(/\d+ 回続けて失敗/)
  await expect(item.locator('time')).toHaveAttribute('datetime', LAST_SUCCESS)
  await expect(item).toContainText(`最後に成功した時刻: ${new Date(LAST_SUCCESS).toLocaleString('ja-JP')}`)
  await expect(failures).not.toContainText('TEAM-WEB')
  await screenshot(page, 'dashboard')
})

test('Settings: 同時実行の上限が表示されるが、変更する入力欄もボタンも無い', async ({ page }) => {
  await page.goto('/settings')
  const card = page.getByTestId('settings-concurrency')
  await expect(card.getByTestId('concurrency-global')).toHaveText(/全体\s*2 本/)
  await expect(card.getByTestId('concurrency-per-project')).toHaveText(/振り分け先ごと\s*1 本/)
  await expect(card).toContainText('画面からは変更できません')
  await expect(card.locator('input, select, textarea, button')).toHaveCount(0)
})

const KNOWN: Record<string, readonly string[]> = {
  linear: ['triggerLabel', 'routingLabelPrefix'],
  routing: ['proposer', 'defaultRunnerId'],
  limits: ['stageTimeoutMaxSec'],
  approval: ['ttlSec'],
}

test('Settings: 02 3.8 のキーを表示・変更でき、送る本文には 02 3.8 のキーしか入らない', async ({ page }) => {
  await page.goto('/settings')
  const form = page.getByRole('form', { name: '全体設定' })
  // 行が無いキーは既定値（02 3.8）。
  await expect(form.getByLabel(/linear\.triggerLabel/)).toHaveValue('ai-ready')
  const prefix = await form.getByLabel(/linear\.routingLabelPrefix/).inputValue()
  expect(prefix.length).toBeGreaterThan(0)
  await expect(form.getByLabel(/提案に使うミクロハーネス/)).toHaveValue('claude_code')
  await expect(form.getByLabel(/提案に使うモデル/)).toHaveValue('')
  await expect(form.getByLabel(/routing\.defaultRunnerId/)).toHaveValue('')
  await expect(form.getByLabel('prepare（秒）')).toHaveValue('1800')
  await expect(form.getByLabel('work（秒）')).toHaveValue('14400')
  await expect(form.getByLabel('check（秒）')).toHaveValue('3600')
  await expect(form.getByLabel('publish（秒）')).toHaveValue('1800')
  await expect(form.getByLabel(/approval\.ttlSec/)).toHaveValue('86400')

  // 6キーすべてを変える。
  await form.getByLabel(/linear\.triggerLabel/).fill('evaloom-go')
  await form.getByLabel(/linear\.routingLabelPrefix/).fill('to:')
  await form.getByLabel(/提案に使うミクロハーネス/).selectOption('codex')
  await form.getByLabel(/提案に使うモデル/).fill('gpt-e2e')
  await form.getByLabel(/routing\.defaultRunnerId/).selectOption(ONLINE)
  await form.getByLabel('work（秒）').fill('7200')
  await form.getByLabel(/approval\.ttlSec/).fill('3600')

  const [request] = await Promise.all([
    page.waitForRequest((r) => new URL(r.url()).pathname === '/api/settings' && r.method() === 'PATCH'),
    form.getByRole('button', { name: '保存' }).click(),
  ])
  const response = await request.response()
  expect(response?.status()).toBe(200)
  const body = request.postDataJSON() as Record<string, Record<string, unknown>>
  for (const [group, keys] of Object.entries(body)) {
    expect(Object.keys(KNOWN)).toContain(group)
    for (const key of Object.keys(keys)) expect(KNOWN[group]).toContain(key)
  }
  expect(body).toEqual({
    linear: { triggerLabel: 'evaloom-go', routingLabelPrefix: 'to:' },
    routing: { proposer: { microHarness: 'codex', model: 'gpt-e2e' }, defaultRunnerId: ONLINE },
    limits: { stageTimeoutMaxSec: { prepare: 1800, work: 7200, check: 3600, publish: 1800 } },
    approval: { ttlSec: 3600 },
  })
  await expect(form.getByRole('status')).toHaveText('保存しました。')

  // 読み直しても変えた値が出る（サーバーに保存されている）。
  await page.reload()
  await expect(form.getByLabel(/linear\.triggerLabel/)).toHaveValue('evaloom-go')
  await expect(form.getByLabel(/linear\.routingLabelPrefix/)).toHaveValue('to:')
  await expect(form.getByLabel(/提案に使うミクロハーネス/)).toHaveValue('codex')
  await expect(form.getByLabel(/提案に使うモデル/)).toHaveValue('gpt-e2e')
  await expect(form.getByLabel(/routing\.defaultRunnerId/)).toHaveValue(ONLINE)
  await expect(form.getByLabel('work（秒）')).toHaveValue('7200')
  await expect(form.getByLabel(/approval\.ttlSec/)).toHaveValue('3600')
  await screenshot(page, 'settings')

  // Runner を未設定に戻すと null を送る。正の整数でない値は送らない。
  await form.getByLabel(/approval\.ttlSec/).fill('0')
  await form.getByRole('button', { name: '保存' }).click()
  await expect(form.getByText('1 以上の整数を入力してください')).toBeVisible()
  await form.getByLabel(/approval\.ttlSec/).fill('3600')
  await form.getByLabel(/routing\.defaultRunnerId/).selectOption('')
  const [clear] = await Promise.all([
    page.waitForRequest((r) => new URL(r.url()).pathname === '/api/settings' && r.method() === 'PATCH'),
    form.getByRole('button', { name: '保存' }).click(),
  ])
  expect(clear.postDataJSON()).toEqual({ routing: { defaultRunnerId: null } })
  expect((await clear.response())?.status()).toBe(200)
  await page.reload()
  await expect(form.getByLabel(/routing\.defaultRunnerId/)).toHaveValue('')
})
