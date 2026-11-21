import { spawnSync } from 'node:child_process'
import { mkdirSync, mkdtempSync, realpathSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { expect, test as base, type Page, type Response } from '@playwright/test'

import { loginOwner, run, setupOwner, startEvaloomd } from './evaloomd.ts'

// evaloom-oxe.11: Dashboard の通知失敗の一覧に linear_outbox の行を出す（13 3.2、12 5章、16 3.4.2、Q82・Q85）。
// - eventId と executionId が null で outboxId が入った項目が、タスクの件名（タスク詳細へのリンク）、kind、
//   channel=linear、reason、記録した時刻とともに出る。Execution の ID の欄に null や undefined が出ない。
// - eventId が null の項目が複数あり execution_events の項目と混ざっても、欠けず重複せず、応答の順のまま出る。
//   「さらに表示」で続きを読んでも同じ。
// - reason に 16 3.4.2 の表に沿った説明が出る。コメントの本文とエラーの文字列は出ない。
//
// global-setup がビルドしたバイナリを一時 DB と空きポートで別に起動する。Worker が failed にした後と同じ形の
// linear_outbox の行（execution_id が NULL）と execution_events の notification_failed を DB へ直接入れ、
// 本物の一覧 API（既定の limit 50、01 5.3）を通して確かめる。全部で 60 件にして2ページ目を作る。

const SCREENSHOTS = fileURLToPath(new URL('../../docs/design/screenshots/', import.meta.url))
const NOW = '2026-11-19T00:00:00Z'
const PROFILE = '01JE2E0P0000000000000000B1'
const RUNNER = 'local'
const PAIRS = 30
const COMMENT_BODY = 'E2E-COMMENT-BODY-must-not-be-shown'
const ERROR_TEXT = 'E2E-ERROR-STRING-must-not-be-shown'

// 16 3.4.2 の表（Q85）の reason と、画面に出る説明。
const LINEAR_REASONS: ReadonlyArray<readonly [string, string]> = [
  ['auth', 'Linear API の認証エラー'],
  ['issue_removed', 'Issue が削除・アーカイブされていた'],
  ['retries_exhausted', '一時的な失敗が続き、5回目も失敗した'],
  ['state_not_found', '状態の変更で、付ける名前の状態が Issue の Team に無かった'],
  ['label_not_found', '振り分けのラベルの追加で、付けるラベルが Linear の Team に無かった'],
  ['other', 'その他の失敗'],
]
const OUTBOX_KINDS = ['missing_acceptance_criteria', 'decision_open', 'cancelled'] as const
const MACOS_REASONS = ['permission_denied', 'launch_failed', 'timeout', 'other'] as const

const pad = (n: number) => String(n).padStart(2, '0')
const executionId = (n: number) => `01JE2EX${String(n).padStart(19, '0')}`
const outboxId = (n: number) => `01JE2EO${String(n).padStart(19, '0')}`
/** k 番目の組の記録した時刻。k が大きいほど古い。 */
const recordedAt = (k: number) => new Date(Date.UTC(2026, 10, 10, 12, 0, 0) - k * 60_000).toISOString()
const outboxTitle = (k: number) => `Linear 送信失敗のタスク ${pad(k)}`
const eventTitle = (k: number) => `macOS 通知失敗のタスク ${pad(k)}`

type Server = { origin: string; cookie: string }
type Item = {
  eventId: number | null
  executionId: string | null
  outboxId: string | null
  taskId: string
  taskTitle: string
  kind: string
  channel: string
  reason: string
  createdAt: string
}

const sqlText = (value: unknown) => `'${(typeof value === 'string' ? value : JSON.stringify(value)).replaceAll("'", "''")}'`

function seed(db: string) {
  const statements: string[] = [
    `INSERT INTO harnesses (id, name, repo_url, repo_path, default_profile_id, approval_waivers, created_at, updated_at)
      VALUES ('e2e-harness', 'E2E ハーネス', 'https://example.com/h.git', '/tmp/e2e-harness', NULL, '[]', '${NOW}', '${NOW}');`,
    `INSERT INTO harness_profiles (id, harness_id, version, commit_sha, tag_name, contract_version, definition, definition_hash, model_id, operation_status, registered_at)
      VALUES ('${PROFILE}', 'e2e-harness', '1.0.0', '${'a'.repeat(40)}', 'v1.0.0', '1', ${sqlText({ description: 'E2E', acceptanceCriteria: 'optional' })}, 'hash', 'model', 'provisional', '${NOW}');`,
    `INSERT INTO projects (id, name, harness_id, profile_id, params, approval_waivers, runner_id, created_at, updated_at)
      VALUES ('e2e-app', 'E2E アプリ', 'e2e-harness', '${PROFILE}', '{}', '[]', '${RUNNER}', '${NOW}', '${NOW}');`,
  ]
  const task = (id: string, status: string, title: string) =>
    statements.push(`INSERT INTO tasks (id, source, project_id, harness_id, title, body, acceptance_criteria, collaboration_mode, status, idempotency_key, created_at, updated_at)
      VALUES ('${id}', 'manual', 'e2e-app', NULL, ${sqlText(title)}, '本文', '条件', 'solo', '${status}', 'idem-${id}', '${NOW}', '${NOW}');`)

  for (let k = 0; k < PAIRS; k += 1) {
    // Execution の無い契機の Linear への送信の失敗（16 3.4.2）。execution_id は NULL、status=failed。
    const outboxTask = `task-o${pad(k)}`
    task(outboxTask, 'cancelled', outboxTitle(k))
    statements.push(`INSERT INTO linear_outbox (id, task_id, linear_issue_id, kind, action, dedupe_key, body, status, attempts, created_at, execution_id, failure_reason, failed_at)
      VALUES ('${outboxId(k)}', '${outboxTask}', 'lin-issue-${k}', '${OUTBOX_KINDS[k % OUTBOX_KINDS.length]}', 'comment', 'e2e-dedupe-${k}',
        ${sqlText(`${COMMENT_BODY} ${k}`)}, 'failed', 5, '${NOW}', NULL, '${LINEAR_REASONS[k % LINEAR_REASONS.length][0]}', '${recordedAt(k)}');`)

    // Execution のある契機の通知の失敗（12 3.4）。偶数番は outbox の行と同じ時刻にして、時刻の並びを混ぜる。
    const eventTask = `task-e${pad(k)}`
    task(eventTask, 'failed', eventTitle(k))
    statements.push(`INSERT INTO executions (id, task_id, seq, mode, status, runner_id, route_kind, project_id, profile_id, profile_version, created_at)
      VALUES ('${executionId(k)}', '${eventTask}', 1, 'continue', 'failed', '${RUNNER}', 'project', 'e2e-app', '${PROFILE}', '1.0.0', '${NOW}');`)
    const eventAt = k % 2 === 0 ? recordedAt(k) : new Date(Date.parse(recordedAt(k)) - 30_000).toISOString()
    statements.push(`INSERT INTO execution_events (execution_id, type, payload, created_at)
      VALUES ('${executionId(k)}', 'notification_failed',
        ${sqlText({ kind: 'failed', channel: 'macos', reason: MACOS_REASONS[k % MACOS_REASONS.length], error: ERROR_TEXT })}, '${eventAt}');`)
  }
  run('sqlite3', [db, `BEGIN;\n${statements.join('\n')}\nCOMMIT;`])
}

function register(binary: string, db: string, temp: string, id: string, name: string) {
  const registered = spawnSync(binary, ['-db', db, 'runner', 'register', '-worktree-root', join(temp, `worktrees-${id}`), '-name', name, id], { encoding: 'utf8' })
  if (registered.status !== 0) throw new Error(`runner register が失敗した: ${registered.stderr}`)
}

const test = base.extend<{ server: Server }, { worker: Server }>({
  worker: [
    // oxlint-disable-next-line no-empty-pattern
    async ({}, provide) => {
      const binary = process.env.EVALOOMD_BINARY
      if (!binary) throw new Error('EVALOOMD_BINARY が無い（e2e/global-setup.ts が設定する）')
      const temp = realpathSync(mkdtempSync(join(tmpdir(), 'evaloom-e2e-oxe11-')))
      const db = join(temp, 'evaloom.db')
      register(binary, db, temp, RUNNER, 'ローカル Runner')
      setupOwner(binary, db)
      seed(db)
      const evaloomd = await startEvaloomd(binary, db, '0')
      try {
        const response = await loginOwner(evaloomd.origin)
        const cookie = /evaloom_session=([^;]+)/.exec(response.headers.getSetCookie().join('\n'))?.[1]
        if (!response.ok || !cookie) throw new Error(`ログインに失敗した: ${response.status}`)
        await provide({ origin: evaloomd.origin, cookie })
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
      cookies: [{ name: 'evaloom_session', value: worker.cookie, domain: '127.0.0.1', path: '/', expires: -1, httpOnly: true, secure: false, sameSite: 'Strict' }],
      origins: [],
    }),
})

const isFailuresResponse = (response: Response) => new URL(response.url()).pathname === '/api/notification-failures' && response.request().method() === 'GET'
const itemKey = (item: Item) => (item.outboxId != null ? `outbox:${item.outboxId}` : `event:${item.eventId}`)

async function screenshot(page: Page) {
  mkdirSync(SCREENSHOTS, { recursive: true })
  await page.screenshot({ path: join(SCREENSHOTS, 'evaloom-oxe.11-dashboard.png'), fullPage: true })
}

/** 一覧の各項目を、画面上の並びのまま、出所と ID（outbox は件名から引く）で読み出す。 */
async function displayedKeys(page: Page, byTitle: Map<string, string>): Promise<string[]> {
  const items = page.getByRole('list', { name: '通知失敗' }).getByRole('listitem')
  const titles = await items.getByRole('link').allTextContents()
  return titles.map((title) => byTitle.get(title) ?? `unknown:${title}`)
}

test('Dashboard: linear_outbox の項目が件名・kind・channel=linear・reason・時刻とともに出て、さらに表示しても応答の順のまま欠けも重複も無い', async ({ page, server }) => {
  expect(server.origin).toBeTruthy()
  const firstResponse = page.waitForResponse(isFailuresResponse)
  await page.goto('/')
  const first = (await (await firstResponse).json()) as { items: Item[]; nextCursor: string | null }
  expect(first.items).toHaveLength(50)
  expect(first.nextCursor).not.toBeNull()

  const list = page.getByRole('list', { name: '通知失敗' })
  const items = list.getByRole('listitem')
  await expect(items).toHaveCount(50)

  // 続きを「さらに表示」で読む。
  const moreResponse = page.waitForResponse((r) => isFailuresResponse(r) && new URL(r.url()).searchParams.has('cursor'))
  await page.getByRole('button', { name: 'さらに表示' }).click()
  const second = (await (await moreResponse).json()) as { items: Item[]; nextCursor: string | null }
  expect(second.nextCursor).toBeNull()
  await expect(items).toHaveCount(PAIRS * 2)
  await expect(page.getByRole('button', { name: 'さらに表示' })).toHaveCount(0)

  // 応答の順（記録した時刻の新しい順、12 5章）を、出所をまたいで欠けも重複も無く並べたものが画面の順と一致する。
  const all = [...first.items, ...second.items]
  const keys = all.map(itemKey)
  expect(new Set(keys).size).toBe(PAIRS * 2)
  expect(all.filter((item) => item.outboxId != null)).toHaveLength(PAIRS)
  for (let i = 1; i < all.length; i += 1) expect(Date.parse(all[i - 1].createdAt)).toBeGreaterThanOrEqual(Date.parse(all[i].createdAt))
  const byTitle = new Map(all.map((item) => [item.taskTitle, itemKey(item)]))
  expect(await displayedKeys(page, byTitle)).toEqual(keys)

  // linear_outbox の項目: eventId と executionId が null で outboxId が入る。
  const outboxItems = items.and(page.locator('[data-source="linear_outbox"]'))
  await expect(outboxItems).toHaveCount(PAIRS)
  for (let k = 0; k < PAIRS; k += 1) {
    const response = all.find((item) => item.outboxId === outboxId(k))
    expect(response).toMatchObject({ eventId: null, executionId: null, channel: 'linear', taskId: `task-o${pad(k)}` })
    const item = outboxItems.filter({ has: page.getByRole('link', { name: outboxTitle(k), exact: true }) })
    await expect(item).toHaveCount(1)
    // タスクの件名はタスク詳細へのリンク。
    await expect(item.getByRole('link')).toHaveAttribute('href', `/tasks/task-o${pad(k)}`)
    const [reason, description] = LINEAR_REASONS[k % LINEAR_REASONS.length]
    await expect(item).toContainText(description)
    await expect(item).toContainText(`reason: ${reason}`)
    await expect(item).toContainText(`kind: ${OUTBOX_KINDS[k % OUTBOX_KINDS.length]}`)
    await expect(item).toContainText('channel: linear')
    await expect(item.locator('time')).toHaveAttribute('dateTime', response!.createdAt)
    expect(Date.parse(response!.createdAt)).toBe(Date.parse(recordedAt(k)))
    // Execution の ID の欄を出さず、null や undefined の文字も出ない。
    await expect(item.getByTestId('notification-failure-execution')).toHaveCount(0)
    await expect(item).not.toContainText('null')
    await expect(item).not.toContainText('undefined')
  }

  // execution_events の項目は Execution の ID を出す。
  const eventItems = items.and(page.locator('[data-source="execution_event"]'))
  await expect(eventItems).toHaveCount(PAIRS)
  await expect(eventItems.filter({ has: page.getByRole('link', { name: eventTitle(3), exact: true }) }).getByTestId('notification-failure-execution')).toContainText(
    executionId(3),
  )

  // コメントの本文とエラーの文字列は出ない。
  const section = page.getByTestId('notification-failures')
  await expect(section).not.toContainText(COMMENT_BODY)
  await expect(section).not.toContainText(ERROR_TEXT)
  await expect(page.locator('body')).not.toContainText(COMMENT_BODY)
  await expect(page.locator('body')).not.toContainText(ERROR_TEXT)
  for (const item of all) expect(JSON.stringify(item)).not.toContain(COMMENT_BODY)

  await screenshot(page)

  // 件名のリンクからタスク詳細を開ける。
  await outboxItems.first().getByRole('link').click()
  await expect(page).toHaveURL(new RegExp(`/tasks/${all.find((item) => item.outboxId != null)!.taskId}$`))
})
