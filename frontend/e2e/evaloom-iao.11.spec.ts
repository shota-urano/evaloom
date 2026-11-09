import { spawnSync } from 'node:child_process'
import { mkdirSync, mkdtempSync, realpathSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { expect, test as base } from '@playwright/test'

import { loginOwner, run, setupOwner, startEvaloomd, type Evaloomd } from './evaloomd.ts'
import { createRepo } from '../../scripts/fixtures.mts'

// evaloom-iao.11: タスク一覧・詳細での Linear 起因のタスクの表示（Issue へのリンク、保留の理由。15-linear-intake 3.6・3.6.3・5章）。
//
// Linear の実 API には接続しない。evaloomd は HOME を空の一時ディレクトリにして起動し（API キーのファイルが無い）、
// Project にも Linear Team ID を登録しないので、Linear Poller は照会しない（15 3.2）。
// Linear 起因のタスク（source=linear）と linear_issue_links の行は SQLite に直接用意する。

const SCREENSHOTS = fileURLToPath(new URL('../../docs/design/screenshots/', import.meta.url))
const PROJECT = { id: 'web-app', name: 'Web アプリ' }

type Server = { db: string; temp: string; origin: string; cookie: string }

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

function sessionCookie(value: string) {
  return { name: 'evaloom_session', value, domain: '127.0.0.1', path: '/', expires: -1, httpOnly: true, secure: false, sameSite: 'Strict' as const }
}

/** HOME を一時ディレクトリにして evaloomd を起動する。spawn は呼び出した時点の環境を写すので、起動の要求を出したら元に戻す。 */
function startWithoutLinearKey(binary: string, db: string, home: string): Promise<Evaloomd> {
  const original = process.env.HOME
  process.env.HOME = home
  try {
    return startEvaloomd(binary, db, '0')
  } finally {
    process.env.HOME = original
  }
}

type LinearTask = {
  id: string
  title: string
  status: 'queued' | 'blocked'
  projectId: string | null
  acceptanceCriteria: string
  cause: string | null
  identifier: string
  executionId?: string
}

const sql = (value: string | null) => (value === null ? 'NULL' : `'${value.replaceAll("'", "''")}'`)
const issueURL = (identifier: string) => `https://linear.app/acme/issue/${identifier}/sample`

/** Linear から取り込んだタスクを、取り込み（15 3.6）が書くのと同じ形で SQLite に入れる。保留（cause あり）は Execution を持たない。 */
function seedLinearTask(db: string, task: LinearTask) {
  const now = new Date().toISOString()
  const issueId = `issue-${task.identifier}`
  const statements = [
    `INSERT INTO tasks (id, project_id, title, body, acceptance_criteria, short_name, task_type, collaboration_mode, status,
       idempotency_key, created_at, updated_at, source, intake_block_cause)
       VALUES (${sql(task.id)}, ${sql(task.projectId)}, ${sql(task.title)}, ${sql(`${task.title} の本文`)}, ${sql(task.acceptanceCriteria)},
       ${sql(task.identifier.toLowerCase())}, 'development', 'solo', ${sql(task.status)}, ${sql(`linear:${issueId}:1`)},
       '${now}', '${now}', 'linear', ${sql(task.cause)});`,
    `INSERT INTO linear_issue_links (id, linear_issue_id, issue_identifier, issue_url, team_id, task_id, attach_seq, created_at)
       VALUES (${sql(`link-${task.id}`)}, ${sql(issueId)}, ${sql(task.identifier)}, ${sql(issueURL(task.identifier))}, 'TEAM-WEB', ${sql(task.id)}, 1, '${now}');`,
  ]
  if (task.executionId) {
    statements.push(`INSERT INTO executions (id, task_id, seq, mode, status, created_at) VALUES (${sql(task.executionId)}, ${sql(task.id)}, 1, 'continue', 'pending', '${now}');`)
  }
  run('sqlite3', [db, statements.join('\n')])
}

const STARTED: LinearTask = {
  id: 'task-001',
  title: 'ログイン画面のエラー表示',
  status: 'queued',
  projectId: PROJECT.id,
  acceptanceCriteria: 'パスワードを誤るとエラーが出る',
  cause: null,
  identifier: 'WEB-101',
  executionId: '01JC000000000000000000ABC1', // ULID（Crockford Base32 の26文字）
}
const HELD_ROUTING: LinearTask = {
  id: 'task-002',
  title: '請求書の CSV 出力',
  status: 'blocked',
  projectId: null,
  acceptanceCriteria: 'CSV をダウンロードできる',
  cause: 'routing_multiple_labels',
  identifier: 'WEB-102',
}
const HELD_CRITERIA: LinearTask = {
  id: 'task-003',
  title: '検索の並び順',
  status: 'blocked',
  projectId: PROJECT.id,
  acceptanceCriteria: '',
  cause: 'missing_acceptance_criteria',
  identifier: 'WEB-103',
}

const test = base.extend<{ server: Server }, { worker: Server }>({
  worker: [
    // oxlint-disable-next-line no-empty-pattern
    async ({}, provide) => {
      const binary = process.env.EVALOOMD_BINARY
      if (!binary) throw new Error('EVALOOMD_BINARY が無い（e2e/global-setup.ts が設定する）')
      const temp = realpathSync(mkdtempSync(join(tmpdir(), 'evaloom-e2e-iao11-')))
      const db = join(temp, 'evaloom.db')
      const home = join(temp, 'home')
      mkdirSync(home)
      const registered = spawnSync(binary, ['-db', db, 'runner', 'register', '-worktree-root', join(temp, 'worktrees'), '-name', 'ローカル Runner', 'local'], { encoding: 'utf8' })
      if (registered.status !== 0) throw new Error(`runner register が失敗した: ${registered.stderr}`)
      setupOwner(binary, db)
      const evaloomd = await startWithoutLinearKey(binary, db, home)
      try {
        const server = { db, temp, origin: evaloomd.origin, cookie: await login(evaloomd.origin) }
        const repo = createRepo(temp, PROJECT.id)
        await owner(server, 'POST', '/api/projects', {
          id: PROJECT.id, name: PROJECT.name, localRepoPath: repo.path, gitRemoteUrl: repo.url, runnerId: 'local', baseBranch: 'main', defaultRole: 'developer',
        })
        for (const task of [STARTED, HELD_ROUTING, HELD_CRITERIA]) seedLinearTask(db, task)
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

test('タスク一覧で Linear 起因のタスクに Issue の識別子とリンクが出て、保留中のタスクには理由が文字で出る', async ({ page }) => {
  await page.goto('/')
  await page.getByRole('navigation', { name: '画面' }).getByRole('link', { name: 'Tasks' }).click()
  await expect(page).toHaveURL('/tasks')
  const table = page.getByRole('table', { name: 'タスク' })
  const row = (title: string) => table.getByRole('row').filter({ hasText: title })

  // 起動したタスク: Issue へのリンクがあり、保留の理由は無い。
  const started = row(STARTED.title)
  await expect(started.getByRole('link', { name: 'WEB-101' })).toHaveAttribute('href', issueURL('WEB-101'))
  await expect(started.getByRole('link', { name: 'WEB-101' })).toHaveAttribute('target', '_blank')
  await expect(started).toContainText('Queued')
  await expect(started.getByTestId('held-reason')).toHaveCount(0)

  // 保留中のタスク: 状態に加えて理由が文字で出る。Project が決まっていなければ「なし」。
  const routing = row(HELD_ROUTING.title)
  await expect(routing.getByRole('link', { name: 'WEB-102' })).toHaveAttribute('href', issueURL('WEB-102'))
  await expect(routing).toContainText('Blocked')
  await expect(routing.getByTestId('held-reason')).toHaveText('保留の理由: 実行先ラベルが2つ以上ある')
  await expect(routing.locator('td').nth(2)).toHaveText('なし')
  const criteria = row(HELD_CRITERIA.title)
  await expect(criteria.getByTestId('held-reason')).toHaveText('保留の理由: 受け入れ条件が書かれていない')
  await expect(criteria.getByRole('link', { name: PROJECT.name })).toBeVisible()

  mkdirSync(SCREENSHOTS, { recursive: true })
  await page.screenshot({ path: join(SCREENSHOTS, 'evaloom-iao.11-tasks.png'), fullPage: true })

  // 一覧からタスク詳細へ進むと、概要に Issue の識別子とリンクが出る。保留していないので保留の確認は出ず、中止できる。
  await started.getByRole('link', { name: STARTED.title }).click()
  await expect(page).toHaveURL(`/tasks/${STARTED.id}`)
  const issueRow = page.locator('dl > div').filter({ has: page.getByText('Linear Issue', { exact: true }) }).locator('dd')
  await expect(issueRow.getByRole('link', { name: 'WEB-101' })).toHaveAttribute('href', issueURL('WEB-101'))
  await expect(page.getByTestId('held-review')).toHaveCount(0)
  await expect(page.getByRole('button', { name: '停止（中止）' })).toBeVisible()
  await expect(page.getByRole('table', { name: 'Execution 履歴' })).toContainText('#1')
  await expect(page.getByText('受信を停止しました')).toHaveCount(0)
  await expect(page.getByTestId('api-error')).toHaveCount(0)
  await page.screenshot({ path: join(SCREENSHOTS, 'evaloom-iao.11-task-detail.png'), fullPage: true })
})

test('保留中のタスクは理由が文字で出て、続行・再実行が出ずに中止だけが出る。中止すると Cancelled になる', async ({ page, server }) => {
  await page.goto('/tasks')
  await page.getByRole('link', { name: HELD_ROUTING.title }).click()
  await expect(page).toHaveURL(`/tasks/${HELD_ROUTING.id}`)

  const held = page.getByTestId('held-review')
  await expect(held.getByRole('heading', { name: '取り込みの保留' })).toBeVisible()
  await expect(held.getByTestId('held-cause')).toContainText('実行先ラベルが2つ以上ある')
  await expect(held.getByTestId('held-cause')).toContainText('routing_multiple_labels')
  // 続行・再実行のボタンは無く、中止のボタンは1つだけ。
  await expect(page.getByRole('button', { name: /続行/ })).toHaveCount(0)
  await expect(page.getByRole('button', { name: /再実行/ })).toHaveCount(0)
  await expect(page.getByRole('button', { name: /中止/ })).toHaveCount(1)
  await expect(held.getByRole('button', { name: '中止する' })).toBeEnabled()
  // Linear の Issue と、決まっていない Project も概要に出る。
  await expect(page.getByRole('link', { name: 'WEB-102' })).toHaveAttribute('href', issueURL('WEB-102'))
  await expect(page.locator('dl > div').filter({ has: page.getByText('プロジェクト', { exact: true }) }).locator('dd')).toHaveText('なし')
  await expect(page.getByText('Execution はまだありません。')).toBeVisible()
  mkdirSync(SCREENSHOTS, { recursive: true })
  await page.screenshot({ path: join(SCREENSHOTS, 'evaloom-iao.11-held.png'), fullPage: true })

  await held.getByRole('button', { name: '中止する' }).click()
  await expect(page.getByRole('status').filter({ hasText: '中止を受け付けました。' })).toBeVisible()
  await expect(page.getByTestId('held-review')).toHaveCount(0)
  await expect(page.locator('[data-status="cancelled"]')).toHaveText('Cancelled')
  // 保留のまま中止したタスクには Execution が無いので、再実行も出さない。
  await expect(page.getByRole('button', { name: /再実行|中止/ })).toHaveCount(0)
  const task = (await owner(server, 'GET', `/api/tasks/${HELD_ROUTING.id}`)) as { status: string; executions: unknown[] }
  expect(task).toMatchObject({ status: 'cancelled', executions: [] })

  // 一覧でも Cancelled になり、保留の理由は出なくなる。
  await page.goto('/tasks')
  const row = page.getByRole('table', { name: 'タスク' }).getByRole('row').filter({ hasText: HELD_ROUTING.title })
  await expect(row).toContainText('Cancelled')
  await expect(row.getByTestId('held-reason')).toHaveCount(0)
})

test('保留中のタスクが画面を開いた後にほかの操作で中止されていたら、中止は 409 になり状態が変わったことを出す', async ({ page, server }) => {
  await page.goto(`/tasks/${HELD_CRITERIA.id}`)
  const held = page.getByTestId('held-review')
  await expect(held.getByTestId('held-cause')).toContainText('受け入れ条件が書かれていない')
  await expect(held.getByTestId('held-cause')).toContainText('missing_acceptance_criteria')

  // 画面を開いたまま、ほかの操作（API）で中止する。
  await owner(server, 'POST', `/api/tasks/${HELD_CRITERIA.id}/cancel`)
  await held.getByRole('button', { name: '中止する' }).click()

  const alert = page.getByTestId('api-error')
  await expect(alert).toContainText('状態が変わりました')
  // 409 では最新を読み直すので、Cancelled の表示に変わり、保留の確認は消える。
  await expect(page.locator('[data-status="cancelled"]')).toHaveText('Cancelled')
  await expect(page.getByTestId('held-review')).toHaveCount(0)
})
