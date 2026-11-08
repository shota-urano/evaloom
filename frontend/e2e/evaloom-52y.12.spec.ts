import { spawnSync } from 'node:child_process'
import { mkdirSync, mkdtempSync, realpathSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { expect, test as base, type Locator } from '@playwright/test'

import { run, startEvaloomd } from './evaloomd.ts'
import { createRepo } from '../../scripts/fixtures.mts'

// evaloom-52y.12: Dashboard の通知失敗の一覧に、Execution の無い契機の Linear コメントの送信の失敗
// （linear_outbox の行。eventId と executionId が null、outboxId あり、13 3.2、16 3.4.2、12 5、Q82・Q85）を出す。
//
// global-setup がビルドしたバイナリを空きポートで別に起動する。送信 Worker の失敗は Linear の応答に左右されるので、
// Worker が failed にした後と同じ形の linear_outbox の行（status=failed、failure_reason、failed_at、execution_id は NULL）と、
// Dispatcher が書くのと同じ形の notification_failed イベントを DB へ直接入れ、本物の一覧 API（並び順と cursor）を通して表示を確かめる。

const SCREENSHOTS = fileURLToPath(new URL('../../docs/design/screenshots/', import.meta.url))
const PROJECT = 'outbox-app'
const LINEAR_REASONS = [
  ['auth', 'Linear API の認証エラー'],
  ['issue_removed', 'Issue が削除・アーカイブされていた'],
  ['retries_exhausted', '一時的な失敗が続き、5回目も失敗した'],
  ['other', 'その他の失敗'],
] as const
const OUTBOX_KINDS = ['intake_blocked', 'missing_acceptance_criteria', 'cancelled'] as const

/** 一覧の1件。i が大きいほど新しい。偶数は linear_outbox の行、奇数は execution_events の行。 */
type Seeded = { i: number; taskId: string; title: string; source: 'linear_outbox' | 'execution_event'; reason: string; label: string; kind: string; at: string; executionId: string | null }

const TOTAL = 60 // 既定の limit（01 5.3、50）を超え、「さらに表示」で続きを読む件数。
const BASE_TIME = Date.parse('2026-11-08T00:00:00.000Z')

const SEEDED: Seeded[] = Array.from({ length: TOTAL }, (_, i) => {
  const n = String(i).padStart(2, '0')
  const at = new Date(BASE_TIME + i * 1000).toISOString().replace('.000Z', 'Z')
  if (i % 2 === 0) {
    const [reason, label] = LINEAR_REASONS[(i / 2) % LINEAR_REASONS.length]
    return { i, taskId: `task-${1000 + i}`, title: `通知失敗 ${n}`, source: 'linear_outbox', reason, label, kind: OUTBOX_KINDS[(i / 2) % OUTBOX_KINDS.length], at, executionId: null }
  }
  return { i, taskId: `task-${1000 + i}`, title: `通知失敗 ${n}`, source: 'execution_event', reason: 'permission_denied', label: '通知が許可されていない', kind: 'succeeded', at, executionId: `01JE2E${'0'.repeat(18)}${n}` }
})
/** 応答の順（記録した時刻の新しい順、12 5）。 */
const EXPECTED = [...SEEDED].sort((a, b) => b.i - a.i)

type Server = { origin: string; cookie: string }

async function login(loginURL: string): Promise<string> {
  const response = await fetch(loginURL, { headers: { Accept: 'application/json' } })
  const cookie = /evaloom_session=([^;]+)/.exec(response.headers.getSetCookie().join('\n'))?.[1]
  if (!response.ok || !cookie) throw new Error(`ログインに失敗した: ${response.status}`)
  return cookie
}

/** 本人用 API をセッション Cookie で呼ぶ（テストの前提づくり用）。 */
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

/** タスク・Execution・linear_outbox の行・notification_failed イベントを入れる。コメントの本文には目印の文字を入れる。 */
function seed(db: string) {
  const statements = SEEDED.map((row) => {
    const n = String(row.i).padStart(2, '0')
    const task = row.taskId
    const insertTask = `INSERT INTO tasks (id, project_id, title, body, acceptance_criteria, short_name, task_type, collaboration_mode, status, idempotency_key, created_at, updated_at)
      VALUES ('${task}', '${PROJECT}', '${row.title}', '本文', '条件', 'e2e', 'development', 'solo', '${row.source === 'linear_outbox' ? 'cancelled' : 'completed'}', 'idem-${n}', '${row.at}', '${row.at}');`
    if (row.source === 'linear_outbox') {
      return `${insertTask}
        INSERT INTO linear_outbox (id, task_id, linear_issue_id, kind, dedupe_key, body, status, attempts, created_at, execution_id, failure_reason, failed_at)
        VALUES ('outbox-${n}', '${task}', 'issue-${n}', '${row.kind}', '${task}:${row.kind}:issue-${n}', 'COMMENT-BODY-SECRET-${n}', 'failed', 5, '${row.at}', NULL, '${row.reason}', '${row.at}');`
    }
    return `${insertTask}
      INSERT INTO executions (id, task_id, seq, mode, status, created_at) VALUES ('${row.executionId}', '${task}', 1, 'continue', 'succeeded', '${row.at}');
      INSERT INTO execution_events (execution_id, type, payload, created_at)
      VALUES ('${row.executionId}', 'notification_failed', '${JSON.stringify({ kind: row.kind, channel: 'macos', reason: row.reason })}', '${row.at}');`
  })
  run('sqlite3', [db, `BEGIN;\n${statements.join('\n')}\nCOMMIT;`])
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
      const temp = realpathSync(mkdtempSync(join(tmpdir(), 'evaloom-e2e-52y12-')))
      const db = join(temp, 'evaloom.db')
      const registered = spawnSync(binary, ['-db', db, 'runner', 'register', '-worktree-root', join(temp, 'worktrees'), '-name', 'ローカル Runner', 'local'], { encoding: 'utf8' })
      if (registered.status !== 0) throw new Error(`runner register が失敗した: ${registered.stderr}`)
      const evaloomd = await startEvaloomd(binary, db, '0')
      try {
        const server: Server = { origin: evaloomd.origin, cookie: await login(evaloomd.loginURL) }
        const repo = createRepo(temp, PROJECT)
        await owner(server, 'POST', '/api/projects', {
          id: PROJECT, name: 'Outbox アプリ', localRepoPath: repo.path, gitRemoteUrl: repo.url, runnerId: 'local', baseBranch: 'main', defaultRole: 'developer',
        })
        seed(db)
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

async function expectRow(item: Locator, row: Seeded) {
  await expect(item).toHaveAttribute('data-source', row.source)
  await expect(item.getByRole('link', { name: row.title, exact: true })).toHaveAttribute('href', `/tasks/${row.taskId}`)
  await expect(item).toContainText(row.label)
  await expect(item).toContainText(`reason: ${row.reason}`)
  await expect(item).toContainText(`kind: ${row.kind}`)
  await expect(item).toContainText(`channel: ${row.source === 'linear_outbox' ? 'linear' : 'macos'}`)
  await expect(item.locator('time')).toHaveAttribute('datetime', /^2026-11-08T00:/)
  if (row.executionId) await expect(item.getByTestId('notification-failure-execution')).toHaveText(` · ${row.executionId}`)
  else await expect(item.getByTestId('notification-failure-execution')).toHaveCount(0)
}

test('linear_outbox の行が execution_events の項目と混ざって、記録した時刻の新しい順に欠けず重複せずに出て、「さらに表示」でも同じ', async ({ page }) => {
  await page.goto('/')
  const list = page.getByRole('list', { name: '通知失敗' })
  const items = list.getByRole('listitem')
  await expect(items).toHaveCount(50)
  await expect(page.getByTestId('notification-failure-count')).toContainText('50件以上')
  expect(await list.getByRole('link').allTextContents()).toEqual(EXPECTED.slice(0, 50).map((r) => r.title))

  // linear_outbox の項目: 件名（タスク詳細へのリンク）、kind、channel=linear、reason の説明、記録した時刻。Execution の ID は出さない。
  for (const index of [0, 1, 2, 4, 6]) await expectRow(items.nth(index), EXPECTED[index])
  const newestOutbox = items.nth(0)
  await expect(newestOutbox.locator('time')).toHaveAttribute('datetime', EXPECTED[0].at)
  await expect(newestOutbox.locator('time')).toHaveText(await page.evaluate((at) => new Date(at).toLocaleString('ja-JP'), EXPECTED[0].at))

  mkdirSync(SCREENSHOTS, { recursive: true })
  await page.getByTestId('notification-failures').scrollIntoViewIfNeeded()
  await page.screenshot({ path: join(SCREENSHOTS, 'evaloom-52y.12-dashboard.png'), fullPage: true })

  await page.getByRole('button', { name: 'さらに表示' }).click()
  await expect(items).toHaveCount(TOTAL)
  await expect(page.getByRole('button', { name: 'さらに表示' })).toHaveCount(0)
  await expect(page.getByTestId('notification-failure-count')).toContainText('通知失敗60件')
  expect(await list.getByRole('link').allTextContents()).toEqual(EXPECTED.map((r) => r.title))
  for (const index of [50, 51, 58, 59]) await expectRow(items.nth(index), EXPECTED[index])

  // reason の4つの値すべてに 16 3.4.2 の表の説明が出る。
  for (const [reason, label] of LINEAR_REASONS) {
    const outboxItems = list.locator('li[data-source="linear_outbox"]').filter({ hasText: `reason: ${reason} ` })
    await expect(outboxItems.first()).toContainText(label)
  }
  // Execution の ID の欄に null・undefined が出ない。コメントの本文とエラーの文字列も出さない。
  const text = (await list.textContent()) ?? ''
  expect(text).not.toMatch(/null|undefined/)
  expect(text).not.toContain('COMMENT-BODY-SECRET')
  await expect(list.getByTestId('notification-failure-execution')).toHaveCount(TOTAL / 2)
})

test('linear_outbox の項目の件名からタスク詳細へ移れる', async ({ page }) => {
  await page.goto('/')
  const row = EXPECTED.find((r) => r.source === 'linear_outbox')!
  await page.getByRole('list', { name: '通知失敗' }).getByRole('link', { name: row.title, exact: true }).click()
  await expect(page).toHaveURL(`/tasks/${row.taskId}`)
  await expect(page.getByRole('heading', { name: row.title })).toBeVisible()
})
