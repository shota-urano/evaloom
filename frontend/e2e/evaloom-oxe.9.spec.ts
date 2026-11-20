import { randomBytes } from 'node:crypto'
import { spawnSync } from 'node:child_process'
import { mkdirSync, mkdtempSync, realpathSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { expect, test as base, type BrowserContext, type Page } from '@playwright/test'

import { loginOwner, run, setupOwner, startEvaloomd, type Evaloomd } from './evaloomd.ts'
import { createRepo, tagProfile } from '../../scripts/fixtures.mts'

// evaloom-oxe.9: タスク詳細（13 3.3）。
// - 実行中のログが逐次表示される。
// - 再接続で欠けなく続きが表示される。
// - ログを prepare・work・check・publish・runner で切り替えられる。
// - 再実行で新しい Execution が履歴に増える。
// - outputs の label と url が出る。
// - 開いている判断待ちへのリンクが出る。
//
// global-setup がビルドしたバイナリを、一時 DB・成果物の保存先（-data-dir）・空きポートで別に起動する。
// スペシャルハーネス・版・Project・タスクは本物の本人用 API で登録する。Runner の役はテストが Runner 用 API
// （claim、ログの報告 POST /api/executions/{id}/events、成果物の登録 POST /api/executions/{id}/artifacts）を呼んで務める。
// 段の結果・確かめた結果・outputs・承認・判断待ち（Control Plane が結果の報告から書く行）は、その後と同じ形の行を
// DB へ直接入れ、本物の GET /api/tasks/{id}・GET /api/executions/{id}・GET /api/approvals・GET /api/decisions を通して確かめる。
// 再接続の確認では evaloomd を同じポート・同じ DB で起動し直して、表示中の SSE を実際に切る。

const SCREENSHOTS = fileURLToPath(new URL('../../docs/design/screenshots/', import.meta.url))
const RUNNER = 'local'
const HARNESS = { id: 'engineer', name: 'エンジニア' }
const PROJECT = { id: 'alpha-app', name: 'Alpha アプリ' }

type Server = {
  origin: string
  cookie: string
  db: string
  runnerToken: string
  workspaceRoot: string
  /** evaloomd を同じポート・同じ DB で起動し直し、ログインし直す（セッションはプロセスのメモリにだけあるため）。 */
  restart: () => Promise<void>
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

/** Runner 用 API を Runner トークンで呼ぶ。 */
async function runner(server: Server, path: string, body: string, contentType = 'application/json') {
  const response = await fetch(`${server.origin}${path}`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${server.runnerToken}`, 'Content-Type': contentType, Accept: 'application/json' },
    body,
  })
  return { status: response.status, text: await response.text() }
}

type TaskDetail = { id: string; title: string; status: string; executions: { id: string; seq: number; mode: string; status: string; outputs?: unknown[] }[] }

async function createTask(server: Server, destination: { projectId: string } | { harnessId: string }, title: string): Promise<TaskDetail> {
  return (await owner(
    server,
    'POST',
    '/api/tasks',
    { ...destination, title, body: `${title} の本文`, acceptanceCriteria: `${title} が動く` },
    { 'Idempotency-Key': crypto.randomUUID() },
  )) as TaskDetail
}

const getTask = async (server: Server, id: string) => (await owner(server, 'GET', `/api/tasks/${id}`)) as TaskDetail

/** Runner として次の仕事を claim し、取った Execution の ID を返す。 */
async function claim(server: Server, taskId: string): Promise<string> {
  const response = await runner(server, `/api/runners/${RUNNER}/claim`, JSON.stringify({ workspaceRoot: server.workspaceRoot }))
  expect(response.status, `claim: ${response.text}`).toBe(200)
  const body = JSON.parse(response.text) as { job: { kind: string; execution: { id: string; taskId: string } } | null }
  expect(body.job?.kind, response.text).toBe('execution')
  expect(body.job?.execution.taskId).toBe(taskId)
  return body.job!.execution.id
}

let sourceSeq = 0
/** Runner としてログを報告する（POST /api/executions/{id}/events、05 3.8）。 */
async function reportLogs(server: Server, executionId: string, stream: string, lines: string[]) {
  const events = lines.map((text) => ({ sourceId: `e2e-oxe9-${++sourceSeq}`, stream, text }))
  const response = await runner(server, `/api/executions/${executionId}/events`, JSON.stringify({ events }))
  expect(response.status, `log report: ${response.text}`).toBe(204)
}

const CROCKFORD = '0123456789ABCDEFGHJKMNPQRSTVWXYZ'
/** ULID（時刻 48bit ＋ 乱数 80bit）。 */
function ulid(): string {
  let time = Date.now()
  let head = ''
  for (let i = 0; i < 10; i++) {
    head = CROCKFORD[time % 32] + head
    time = Math.floor(time / 32)
  }
  return head + [...randomBytes(16)].map((b) => CROCKFORD[b % 32]).join('')
}

/** Runner として成果物ファイルを登録する（POST /api/executions/{id}/artifacts、05 3.9）。 */
async function registerArtifact(server: Server, executionId: string, stage: string, kind: string, name: string, content: string): Promise<string> {
  const id = ulid()
  const query = new URLSearchParams({ artifactId: id, stage, kind, name, redacted: '0' })
  const response = await runner(server, `/api/executions/${executionId}/artifacts?${query.toString()}`, content, 'application/octet-stream')
  expect(response.status, `artifact: ${response.text}`).toBe(204)
  return id
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
      // macOS の tmpdir はシンボリックリンク経由なので、evaloomd が保存する実パスに揃える。
      const temp = realpathSync(mkdtempSync(join(tmpdir(), 'evaloom-e2e-oxe9-')))
      const db = join(temp, 'evaloom.db')
      const dataDir = join(temp, 'data')
      const workspaceRoot = join(temp, 'worktrees')
      mkdirSync(dataDir, { recursive: true })
      const repo = createRepo(temp, HARNESS.id)
      tagProfile(repo.path, '1.0.0', 'engineer')
      // runner register はトークンを一度だけ標準エラーに出す。
      const registered = spawnSync(binary, ['-db', db, 'runner', 'register', '-worktree-root', workspaceRoot, '-name', 'ローカル Runner', RUNNER], { encoding: 'utf8' })
      const runnerToken = registered.stderr.trim()
      if (registered.status !== 0 || !/^[A-Za-z0-9_-]{43}$/.test(runnerToken)) throw new Error(`runner register が失敗した: ${registered.stderr}`)
      setupOwner(binary, db)
      const extra = ['-data-dir', dataDir, '-artifact-max-bytes', '1048576']
      let evaloomd: Evaloomd = await startEvaloomd(binary, db, '0', undefined, extra)
      try {
        const server: Server = {
          origin: evaloomd.origin,
          cookie: await login(evaloomd.origin),
          db,
          runnerToken,
          workspaceRoot,
          restart: async () => {
            await evaloomd.stop()
            evaloomd = await startEvaloomd(binary, db, new URL(server.origin).port, undefined, extra)
            server.cookie = await login(evaloomd.origin)
          },
        }
        await owner(server, 'POST', '/api/harnesses', { id: HARNESS.id, name: HARNESS.name, repoPath: repo.path, repoUrl: `https://example.com/${HARNESS.id}.git` })
        const profile = (await owner(server, 'POST', '/api/harness-profiles', { harnessId: HARNESS.id, version: '1.0.0' })) as { id: string }
        await owner(server, 'PATCH', `/api/harnesses/${HARNESS.id}`, { defaultProfileId: profile.id })
        await owner(server, 'POST', '/api/projects', {
          id: PROJECT.id,
          name: PROJECT.name,
          harnessId: HARNESS.id,
          profileId: profile.id,
          params: {},
          approvalWaivers: [],
          runnerId: RUNNER,
        })
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

async function screenshot(page: Page, screen: string) {
  mkdirSync(SCREENSHOTS, { recursive: true })
  await page.screenshot({ path: join(SCREENSHOTS, `evaloom-oxe.9-${screen}.png`), fullPage: true })
}

/** Tasks 一覧から件名のリンクでタスク詳細を開く。 */
async function openTask(page: Page, task: { id: string; title: string }) {
  await page.goto('/tasks')
  await page.getByRole('link', { name: task.title }).click()
  await expect(page).toHaveURL(`/tasks/${task.id}`)
  await expect(page.getByRole('heading', { name: task.title })).toBeVisible()
}

const historyRows = (page: Page) => page.getByRole('table', { name: 'Execution 履歴' }).locator('tbody tr')
const logLines = (page: Page) => page.getByRole('log').locator('[data-log-line]')
const tab = (page: Page, name: string) => page.getByRole('tablist', { name: 'ログの stream' }).getByRole('tab', { name: new RegExp(`^${name} \\(`) })

/** 再起動中は SSE の要求を通信エラーにし、新しいセッション Cookie をブラウザに入れてから通す。 */
async function blockEvents(page: Page, context: BrowserContext, server: Server, during: () => Promise<void>) {
  const pattern = '**/api/executions/*/events'
  await page.route(pattern, (route) => route.abort('connectionrefused'))
  await during()
  await context.addCookies([sessionCookie(server.cookie)])
  await page.unroute(pattern)
}

test('実行中のログが逐次表示され、stream で切り替えられ、再接続で欠けなく続きが表示される', async ({ page, context, server }) => {
  const task = await createTask(server, { projectId: PROJECT.id }, 'Alpha のログ表示')
  const executionId = await claim(server, task.id)
  await openTask(page, task)
  await expect(page.getByTestId('log-source')).toContainText('受信中')
  await expect(page.getByRole('tablist', { name: 'ログの stream' }).getByRole('tab')).toHaveText(['prepare (0)', 'work (0)', 'check (0)', 'publish (0)', 'runner (0)'])
  // 画面を読み直していないことの目印。
  await page.evaluate(() => Object.assign(globalThis, { __notReloaded: true }))

  // work の行が、報告するたびに1行ずつ追記される。
  await reportLogs(server, executionId, 'work', ['ログイン画面を実装しています'])
  await expect(logLines(page)).toHaveText(['ログイン画面を実装しています'])
  await reportLogs(server, executionId, 'work', ['テストを追加しました'])
  await expect(logLines(page)).toHaveText(['ログイン画面を実装しています', 'テストを追加しました'])

  // 段ごとの stream と runner に切り替える。
  await reportLogs(server, executionId, 'prepare', ['依存を入れています'])
  await reportLogs(server, executionId, 'check', ['lint を実行しています', 'lint が通りました'])
  await reportLogs(server, executionId, 'publish', ['PR を作っています'])
  await reportLogs(server, executionId, 'runner', ['作業領域を準備しました'])
  await expect(tab(page, 'runner')).toHaveText('runner (1)')
  await expect(logLines(page)).toHaveText(['ログイン画面を実装しています', 'テストを追加しました'])
  for (const [name, lines] of [
    ['prepare', ['依存を入れています']],
    ['check', ['lint を実行しています', 'lint が通りました']],
    ['publish', ['PR を作っています']],
    ['runner', ['作業領域を準備しました']],
    ['work', ['ログイン画面を実装しています', 'テストを追加しました']],
  ] as const) {
    await tab(page, name).click()
    await expect(tab(page, name)).toHaveAttribute('aria-selected', 'true')
    await expect(logLines(page)).toHaveText([...lines])
  }
  await screenshot(page, 'task-detail-log')

  // evaloomd を止めて SSE を切る。切断中も Runner はログを報告し、再接続で続きから受け取る。
  const reconnects: (string | undefined)[] = []
  page.on('response', (response) => {
    const url = new URL(response.url())
    if (url.pathname === `/api/executions/${executionId}/events` && response.status() === 200 && response.request().resourceType() === 'fetch') {
      reconnects.push(response.request().headers()['last-event-id'])
    }
  })
  await blockEvents(page, context, server, async () => {
    await server.restart()
    await expect(page.getByTestId('log-source')).toContainText('再接続しています')
    await reportLogs(server, executionId, 'work', ['切断中の1行目', '切断中の2行目'])
  })
  await expect(logLines(page)).toHaveText(['ログイン画面を実装しています', 'テストを追加しました', '切断中の1行目', '切断中の2行目'], { timeout: 20_000 })
  await expect(page.getByTestId('log-source')).toContainText('受信中')
  // 再接続は Last-Event-ID 付きで、最初から受け直していない（ほかの stream の件数も増えていない）。
  expect(reconnects.length).toBeGreaterThanOrEqual(1)
  expect(reconnects.at(-1)).toMatch(/^[1-9][0-9]*$/)
  await expect(tab(page, 'check')).toHaveText('check (2)')
  await reportLogs(server, executionId, 'work', ['再接続後の行'])
  await expect(logLines(page)).toHaveText(['ログイン画面を実装しています', 'テストを追加しました', '切断中の1行目', '切断中の2行目', '再接続後の行'])
  expect(await page.evaluate(() => (globalThis as { __notReloaded?: boolean }).__notReloaded)).toBe(true)

  // 終了した Execution は保存済みログを表示する。work の保存済みログは成果物（stage=work、kind=log）、runner はイベントから返る。
  await registerArtifact(server, executionId, 'work', 'log', 'work.log', '保存した work の1行目\n保存した work の2行目\n')
  run('sqlite3', [
    server.db,
    `UPDATE executions SET status = 'failed', error_class = 'work_failed', finished_at = '2026-11-20T01:00:00Z' WHERE id = '${executionId}';
     UPDATE tasks SET status = 'failed' WHERE id = '${task.id}';`,
  ])
  await page.reload()
  await expect(page.getByTestId('log-source')).toHaveText('保存済みのログを表示しています')
  await tab(page, 'work').click()
  await expect(logLines(page)).toHaveText(['保存した work の1行目', '保存した work の2行目'])
  await tab(page, 'runner').click()
  await expect(logLines(page)).toHaveText(['作業領域を準備しました'])
})

test('再実行で新しい Execution が履歴に増える（継続・最初から）', async ({ page, server }) => {
  const task = await createTask(server, { projectId: PROJECT.id }, 'Alpha の再実行')
  await openTask(page, task)
  await expect(historyRows(page)).toHaveCount(1)
  await expect(historyRows(page).first()).toContainText('#1')

  await page.getByRole('button', { name: '停止（中止）' }).click()
  await expect(page.getByRole('status').filter({ hasText: '中止を受け付けました。' })).toBeVisible()
  await expect(historyRows(page).first()).toContainText('cancelled')

  await page.getByRole('button', { name: '再実行（継続）' }).click()
  await expect(page.getByRole('status').filter({ hasText: '再実行（継続）を受け付けました。' })).toBeVisible()
  await expect(historyRows(page)).toHaveCount(2)
  await expect(historyRows(page).first()).toContainText('#2')
  await expect(historyRows(page).first()).toContainText('継続')
  await expect(historyRows(page).nth(1)).toContainText('cancelled')
  await expect(page.getByRole('heading', { name: 'ログ（#2）' })).toBeVisible()

  // 最初からの再実行は確認ダイアログを出す。やめると送らない。
  await page.getByRole('button', { name: '停止（中止）' }).click()
  await expect(historyRows(page).first()).toContainText('cancelled')
  await page.getByRole('button', { name: '再実行（最初から）' }).click()
  const dialog = page.getByRole('alertdialog', { name: '最初から再実行しますか' })
  await dialog.getByRole('button', { name: 'やめる' }).click()
  await expect(dialog).toHaveCount(0)
  expect((await getTask(server, task.id)).executions).toHaveLength(2)

  await page.getByRole('button', { name: '再実行（最初から）' }).click()
  await page.getByRole('alertdialog').getByRole('button', { name: '最初から再実行する' }).click()
  await expect(historyRows(page)).toHaveCount(3)
  await expect(historyRows(page).first()).toContainText('#3')
  await expect(historyRows(page).first()).toContainText('最初から')
  expect((await getTask(server, task.id)).executions.map((e) => [e.seq, e.mode])).toEqual([
    [1, 'continue'],
    [2, 'continue'],
    [3, 'fresh'],
  ])
  // 再読み込みしても3件のまま。後のテストの claim 対象にしないよう止めておく。
  await page.reload()
  await expect(historyRows(page)).toHaveCount(3)
  await page.getByRole('button', { name: '停止（中止）' }).click()
  await expect(historyRows(page).first()).toContainText('cancelled')
})

test('outputs の label と url、開いている判断待ちへのリンク、段の結果・確かめた結果・承認・成果物が出る', async ({ page, server }) => {
  // 前のテストの Execution は終了済みなので、同じ Project のタスクを claim できる。
  const task = await createTask(server, { projectId: PROJECT.id }, 'PR を出して結果が分からないタスク')
  const executionId = await claim(server, task.id)
  // Runner が段の要求・結果・out のファイル・作業の概要を成果物として登録する（本物の登録 API）。
  const workResult = await registerArtifact(
    server,
    executionId,
    'work',
    'result_json',
    'work/result.json',
    JSON.stringify({ status: 'succeeded', message: 'ログイン画面を実装した', summary: 'フォームと検証を追加' }),
  )
  await registerArtifact(server, executionId, 'work', 'work_summary', 'out/summary.md', '# 作業の概要\n')
  await registerArtifact(server, executionId, 'check', 'out_file', 'out/report.md', 'lint: ok\n')

  // 出す段が PR を作った後に結果が分からなくなり、本人の判断待ち（blocked・publish_unknown）になった状態。
  const work = ulid()
  const check = ulid()
  const publish = ulid()
  const decision = ulid()
  const approval = ulid()
  const approvalDecision = ulid()
  const outputs = JSON.stringify([
    { kind: 'pull_request', label: 'PR #42', url: 'https://github.com/acme/app/pull/42' },
    { kind: 'preview', label: 'プレビュー', url: 'https://preview.example.com/42' },
  ])
  const findings = JSON.stringify([{ rule: 'no-console', severity: 'warning', message: 'console.log が残っている', location: { path: 'src/login.ts', line: 12 } }])
  const target = JSON.stringify({ label: 'acme/app の PR #42', url: 'https://github.com/acme/app/pull/42' })
  run('sqlite3', [
    server.db,
    `BEGIN;
     INSERT INTO stage_runs (id, execution_id, stage, attempt, status, result_artifact_id, started_at, finished_at)
       VALUES ('${work}', '${executionId}', 'work', 1, 'succeeded', '${workResult}', '2026-11-20T00:00:00Z', '2026-11-20T00:10:00Z');
     INSERT INTO stage_runs (id, execution_id, stage, attempt, status, started_at, finished_at)
       VALUES ('${check}', '${executionId}', 'check', 1, 'succeeded', '2026-11-20T00:10:00Z', '2026-11-20T00:12:00Z');
     INSERT INTO stage_runs (id, execution_id, stage, attempt, status, error_class, started_at, finished_at)
       VALUES ('${publish}', '${executionId}', 'publish', 1, 'unknown', 'publish_unknown', '2026-11-20T00:20:00Z', '2026-11-20T00:21:00Z');
     INSERT INTO check_results (id, execution_id, stage_run_id, result, content_id, summary, findings, created_at)
       VALUES ('${ulid()}', '${executionId}', '${check}', 'pass', 'sha-content-42', 'lint とテストが通った', '${findings}', '2026-11-20T00:12:00Z');
     INSERT INTO owner_decisions (id, kind, task_id, execution_id, subject_id, detail, status, answer, answered_at, created_at)
       VALUES ('${approvalDecision}', 'approval', '${task.id}', '${executionId}', '${approval}', '{}', 'answered', '{"action":"approve","contentId":"sha-content-42"}', '2026-11-20T00:15:00Z', '2026-11-20T00:13:00Z');
     INSERT INTO approvals (id, task_id, execution_id, decision_id, requested_stage, operation, target, content_id, params_hash, status, decided_at, use_count, consumed_at, created_at)
       VALUES ('${approval}', '${task.id}', '${executionId}', '${approvalDecision}', 'publish', 'create_pull_request', '${target}', 'sha-content-42', 'hash-42', 'consumed', '2026-11-20T00:15:00Z', 1, '2026-11-20T00:20:30Z', '2026-11-20T00:13:00Z');
     INSERT INTO owner_decisions (id, kind, task_id, execution_id, cause, detail, status, created_at)
       VALUES ('${decision}', 'blocked', '${task.id}', '${executionId}', 'publish_unknown', '{"message":"PR を作った後に応答が途切れた"}', 'open', '2026-11-20T00:21:00Z');
     UPDATE executions SET status = 'blocked', error_class = 'publish_unknown', outputs = '${outputs}', params = '{"branch":"main"}' WHERE id = '${executionId}';
     UPDATE tasks SET status = 'blocked' WHERE id = '${task.id}';
     COMMIT;`,
  ])
  // 本物の API が outputs を返している。
  expect((await getTask(server, task.id)).executions.at(-1)?.outputs).toHaveLength(2)

  await openTask(page, task)
  // 概要: 外に出したものの label と url、開いている判断待ちへのリンク。
  const outputList = page.getByRole('list', { name: '外に出したもの' })
  await expect(outputList.getByRole('link', { name: 'PR #42' })).toHaveAttribute('href', 'https://github.com/acme/app/pull/42')
  await expect(outputList.getByRole('link', { name: 'プレビュー' })).toHaveAttribute('href', 'https://preview.example.com/42')
  await expect(outputList).toContainText('https://github.com/acme/app/pull/42')
  const openDecisions = page.getByRole('list', { name: '開いている判断待ち' })
  await expect(openDecisions.getByRole('link')).toHaveCount(1)
  await expect(openDecisions.getByRole('link', { name: '止まった仕事の続行（publish_unknown）' })).toHaveAttribute('href', `/decisions/${decision}`)

  // 段の結果（結果 JSON の message と summary を含む）。
  const stages = page.getByRole('table', { name: '段の結果' })
  await expect(stages.locator('tbody tr')).toHaveCount(3)
  await expect(stages.locator('tbody tr').first()).toContainText('ログイン画面を実装した')
  await expect(stages.locator('tbody tr').first()).toContainText('フォームと検証を追加')
  await expect(stages.locator('tbody tr').nth(2)).toContainText('publish_unknown')
  await expect(page.getByTestId('execution-params')).toContainText('"branch": "main"')

  // 確かめた結果、承認、成果物。
  const checkResult = page.getByRole('article', { name: /^確かめた結果 / })
  await expect(checkResult).toContainText('lint とテストが通った')
  await expect(checkResult).toContainText('sha-content-42')
  await expect(checkResult.getByRole('table', { name: 'finding' })).toContainText('src/login.ts:12')
  const approvals = page.getByRole('table', { name: '承認' })
  await expect(approvals.locator('tbody tr')).toHaveCount(1)
  await expect(approvals).toContainText('create_pull_request')
  await expect(approvals).toContainText('consumed')
  await expect(approvals.getByRole('link', { name: 'acme/app の PR #42' })).toHaveAttribute('href', 'https://github.com/acme/app/pull/42')
  const artifacts = page.getByRole('table', { name: '成果物' })
  await expect(artifacts.locator('tbody tr')).toHaveCount(3)
  await expect(artifacts).toContainText('作業の概要')
  await expect(artifacts).toContainText('結果 JSON')
  await expect(artifacts).toContainText('段の out のファイル')
  const summaryLink = artifacts.getByRole('link', { name: 'out/summary.md' })
  const response = await page.request.get((await summaryLink.getAttribute('href'))!)
  expect(response.status()).toBe(200)
  expect(await response.text()).toBe('# 作業の概要\n')
  await screenshot(page, 'task-detail')

  // 判断待ちのリンクから判断待ちの詳細を開ける。
  await openDecisions.getByRole('link').click()
  await expect(page).toHaveURL(`/decisions/${decision}`)
})
