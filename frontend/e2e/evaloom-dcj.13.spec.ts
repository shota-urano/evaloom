import { spawnSync } from 'node:child_process'
import { mkdirSync, mkdtempSync, realpathSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { expect, test as base, type Page } from '@playwright/test'

import { loginOwner, run, setupOwner, startEvaloomd, type Evaloomd } from './evaloomd.ts'

// evaloom-dcj.13: タスク一覧・詳細での Linear 起因のタスクの表示（15-linear-intake 3.6・3.6.3・3.6.4・5章、02 5.7）。
// - source=linear のタスクに Issue の識別子とリンクが出る。
// - routing のタスクに、振り分けを待っていることと判断待ちへのリンクが出る（判断待ちがまだ無ければ提案を作っていること）。
// - 保留中のタスクに理由が文字で出て、続行ボタンが出ず中止だけが出る。
//
// Linear の実 API には接続しない。evaloomd は HOME を空の一時ディレクトリにして起動するので、API キーのファイルが無く、
// Linear Poller は照会を始めない（15 6章）。取り込みが書くタスク・linear_issue_links・route_proposals・owner_decisions は
// SQLite に直接入れる。スペシャルハーネスは既定の版を持たない（config_missing_profile の保留のため）。

const SCREENSHOTS = fileURLToPath(new URL('../../docs/design/screenshots/', import.meta.url))
const NOW = '2026-11-20T00:00:00Z'
const HARNESS = { id: 'e2e-web', name: 'E2E Web ハーネス' }
const ulid = (kind: string, n: number) => `01JE2E${kind}${String(n).padStart(19, '0')}`

type Server = { origin: string; cookie: string; db: string }

type LinearTask = {
  id: string
  title: string
  status: 'routing' | 'blocked'
  harnessId: string | null
  acceptanceCriteria: string
  cause: string | null
  identifier: string
}

const sql = (value: string | null) => (value === null ? 'NULL' : `'${value.replaceAll("'", "''")}'`)
const issueURL = (identifier: string) => `https://linear.app/acme/issue/${identifier}/sample`

const ROUTING_OPEN: LinearTask = {
  id: 'task-201',
  title: '請求書の CSV 出力',
  status: 'routing',
  harnessId: null,
  acceptanceCriteria: '',
  cause: null,
  identifier: 'WEB-201',
}
const ROUTING_PENDING: LinearTask = { ...ROUTING_OPEN, id: 'task-202', title: '検索の並び順', identifier: 'WEB-202' }
const HELD_PROFILE: LinearTask = {
  id: 'task-203',
  title: 'ログイン画面のエラー表示',
  status: 'blocked',
  harnessId: HARNESS.id,
  acceptanceCriteria: 'パスワードを誤るとエラーが出る',
  cause: 'config_missing_profile',
  identifier: 'WEB-203',
}
const HELD_CRITERIA: LinearTask = {
  ...HELD_PROFILE,
  id: 'task-204',
  title: '通知メールの文面',
  acceptanceCriteria: '',
  cause: 'missing_acceptance_criteria',
  identifier: 'WEB-204',
}
const ROUTE_DECISION = ulid('R', 201)
const OPEN_PROPOSAL = ulid('P', 201)
const PENDING_PROPOSAL = ulid('P', 202)

/** 取り込み（15 3.6）が書くのと同じ形でタスクと Issue の対応を入れる。振り分けを待つタスクと保留は Execution を持たない。 */
function linearTask(task: LinearTask): string[] {
  const issueId = `issue-${task.identifier}`
  return [
    `INSERT INTO tasks (id, source, project_id, harness_id, title, body, acceptance_criteria, collaboration_mode, status, idempotency_key, created_at, updated_at, intake_block_cause)
      VALUES (${sql(task.id)}, 'linear', NULL, ${sql(task.harnessId)}, ${sql(task.title)}, ${sql(`${task.title} の本文`)}, ${sql(task.acceptanceCriteria)}, 'solo',
        ${sql(task.status)}, ${sql(`linear:${issueId}:1`)}, '${NOW}', '${NOW}', ${sql(task.cause)});`,
    `INSERT INTO linear_issue_links (id, linear_issue_id, issue_identifier, issue_url, team_id, task_id, attach_seq, created_at)
      VALUES (${sql(`link-${task.id}`)}, ${sql(issueId)}, ${sql(task.identifier)}, ${sql(issueURL(task.identifier))}, 'TEAM-USLAB', ${sql(task.id)}, 1, '${NOW}');`,
  ]
}

function seed(db: string) {
  const statements = [
    `INSERT INTO harnesses (id, name, repo_url, repo_path, default_profile_id, approval_waivers, created_at, updated_at)
      VALUES (${sql(HARNESS.id)}, ${sql(HARNESS.name)}, 'https://example.com/web.git', '/tmp/e2e-web', NULL, '[]', '${NOW}', '${NOW}');`,
    ...[ROUTING_OPEN, ROUTING_PENDING, HELD_PROFILE, HELD_CRITERIA].flatMap(linearTask),
    // 提案が失敗して判断待ちが開いたタスク（15 3.6.4 手順2・5）。
    `INSERT INTO route_proposals (id, task_id, trigger, seen_labels, candidates, status, proposed_label, reason, decision_id, created_at, finished_at)
      VALUES ('${OPEN_PROPOSAL}', '${ROUTING_OPEN.id}', 'no_label', '[]', '[]', 'failed', NULL, NULL, '${ROUTE_DECISION}', '${NOW}', '${NOW}');`,
    `INSERT INTO owner_decisions (id, kind, task_id, execution_id, subject_id, cause, basis, detail, status, withdrawn_reason, created_at)
      VALUES ('${ROUTE_DECISION}', 'route', '${ROUTING_OPEN.id}', NULL, '${OPEN_PROPOSAL}', NULL, '${OPEN_PROPOSAL}', '{}', 'open', NULL, '${NOW}');`,
    // まだ Runner が提案を作っていないタスク。判断待ちは無い。
    `INSERT INTO route_proposals (id, task_id, trigger, seen_labels, candidates, status, proposed_label, reason, decision_id, created_at, finished_at)
      VALUES ('${PENDING_PROPOSAL}', '${ROUTING_PENDING.id}', 'no_label', '[]', '[]', 'queued', NULL, NULL, NULL, '${NOW}', NULL);`,
  ]
  run('sqlite3', [db, `BEGIN;\n${statements.join('\n')}\nCOMMIT;`])
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

/** 本人用 API をセッション Cookie で呼ぶ（確認用）。 */
async function owner(server: Server, method: string, path: string) {
  const cookie = `evaloom_session=${server.cookie}`
  const session = await fetch(`${server.origin}/api/session`, { headers: { Cookie: cookie, Accept: 'application/json' } })
  const { csrfToken } = (await session.json()) as { csrfToken: string }
  const response = await fetch(`${server.origin}${path}`, {
    method,
    headers: { Cookie: cookie, Origin: server.origin, 'X-CSRF-Token': csrfToken, Accept: 'application/json' },
  })
  const text = await response.text()
  if (!response.ok) throw new Error(`${method} ${path}: ${response.status} ${text}`)
  return text === '' ? undefined : JSON.parse(text)
}

const test = base.extend<{ server: Server }, { worker: Server }>({
  worker: [
    // oxlint-disable-next-line no-empty-pattern
    async ({}, provide) => {
      const binary = process.env.EVALOOMD_BINARY
      if (!binary) throw new Error('EVALOOMD_BINARY が無い（e2e/global-setup.ts が設定する）')
      const temp = realpathSync(mkdtempSync(join(tmpdir(), 'evaloom-e2e-dcj13-')))
      const db = join(temp, 'evaloom.db')
      const home = join(temp, 'home')
      mkdirSync(home)
      const registered = spawnSync(binary, ['-db', db, 'runner', 'register', '-worktree-root', join(temp, 'worktrees'), '-name', 'ローカル Runner', 'local'], {
        encoding: 'utf8',
      })
      if (registered.status !== 0) throw new Error(`runner register が失敗した: ${registered.stderr}`)
      setupOwner(binary, db)
      seed(db)
      const evaloomd = await startWithoutLinearKey(binary, db, home)
      try {
        const response = await loginOwner(evaloomd.origin)
        const cookie = /evaloom_session=([^;]+)/.exec(response.headers.getSetCookie().join('\n'))?.[1]
        if (!response.ok || !cookie) throw new Error(`ログインに失敗した: ${response.status}`)
        await provide({ origin: evaloomd.origin, cookie, db })
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

async function screenshot(page: Page, screen: string) {
  mkdirSync(SCREENSHOTS, { recursive: true })
  await page.screenshot({ path: join(SCREENSHOTS, `evaloom-dcj.13-${screen}.png`), fullPage: true })
}

const row = (page: Page, title: string) => page.getByRole('table', { name: 'タスク' }).getByRole('row').filter({ hasText: title })

test('一覧: Linear 起因のタスクに Issue の識別子とリンク、振り分けを待つタスクに判断待ちへのリンク、保留中のタスクに理由が出る', async ({ page }) => {
  await page.goto('/')
  await page.getByRole('navigation', { name: '画面' }).getByRole('link', { name: 'Tasks' }).click()
  await expect(page).toHaveURL('/tasks')

  for (const task of [ROUTING_OPEN, ROUTING_PENDING, HELD_PROFILE, HELD_CRITERIA]) {
    const link = row(page, task.title).getByRole('link', { name: task.identifier })
    await expect(link).toHaveAttribute('href', issueURL(task.identifier))
    await expect(link).toHaveAttribute('target', '_blank')
  }

  const open = row(page, ROUTING_OPEN.title)
  await expect(open).toContainText('Routing')
  await expect(open.getByTestId('routing-wait')).toContainText('振り分けを待っています')
  await expect(open.getByRole('link', { name: '判断待ちを開く' })).toHaveAttribute('href', `/decisions/${ROUTE_DECISION}`)
  const pending = row(page, ROUTING_PENDING.title)
  await expect(pending.getByTestId('routing-wait')).toHaveText('振り分けを待っています振り分けの提案を作っています')
  await expect(pending.getByRole('link', { name: '判断待ちを開く' })).toHaveCount(0)

  await expect(row(page, HELD_PROFILE.title).getByTestId('held-reason')).toHaveText(
    '保留の理由: スペシャルハーネスへ直接振り分けたが、そのスペシャルハーネスに既定の版が無い',
  )
  await expect(row(page, HELD_CRITERIA.title).getByTestId('held-reason')).toHaveText(
    '保留の理由: 振り分け先の版が受け入れ条件を求めているのに、Issue に受け入れ条件が無い',
  )
  await expect(row(page, HELD_CRITERIA.title).getByTestId('routing-wait')).toHaveCount(0)
  await expect(page.getByTestId('api-error')).toHaveCount(0)
  await screenshot(page, 'tasks')

  // 一覧の判断待ちへのリンクから、振り分けの判断待ちの詳細へ進める。
  await open.getByRole('link', { name: '判断待ちを開く' }).click()
  await expect(page).toHaveURL(`/decisions/${ROUTE_DECISION}`)
  await expect(page.getByText(ROUTING_OPEN.title).first()).toBeVisible()
})

test('詳細: 振り分けを待つタスクに、そのことと振り分けの判断待ちへのリンクが出る', async ({ page }) => {
  await page.goto('/tasks')
  await row(page, ROUTING_OPEN.title).getByRole('link', { name: ROUTING_OPEN.title }).click()
  await expect(page).toHaveURL(`/tasks/${ROUTING_OPEN.id}`)

  const routing = page.getByTestId('routing-review')
  await expect(routing.getByRole('heading', { name: '振り分けを待っています' })).toBeVisible()
  await expect(routing.getByRole('link', { name: '振り分けの判断待ちを開く' })).toHaveAttribute('href', `/decisions/${ROUTE_DECISION}`)
  const issueRow = page.locator('dl > div').filter({ has: page.getByText('Linear Issue', { exact: true }) }).locator('dd')
  await expect(issueRow.getByRole('link', { name: 'WEB-201' })).toHaveAttribute('href', issueURL('WEB-201'))
  await expect(page.getByTestId('held-review')).toHaveCount(0)
  await expect(page.getByRole('button', { name: /続行|再実行/ })).toHaveCount(0)
  await expect(page.getByRole('button', { name: '停止（中止）' })).toBeVisible()
  await expect(page.getByTestId('api-error')).toHaveCount(0)
  await screenshot(page, 'task-routing')

  await routing.getByRole('link', { name: '振り分けの判断待ちを開く' }).click()
  await expect(page).toHaveURL(`/decisions/${ROUTE_DECISION}`)

  // 判断待ちがまだ開いていないタスクは、提案を作っていることを出す。
  await page.goto(`/tasks/${ROUTING_PENDING.id}`)
  await expect(page.getByTestId('routing-pending')).toHaveText('振り分けの提案を作っています。提案ができると判断待ちが開きます。')
  await expect(page.getByTestId('routing-review').getByRole('link', { name: '振り分けの判断待ちを開く' })).toHaveCount(0)
})

test('詳細: 保留中のタスクは理由が文字で出て、続行・再実行が出ずに中止だけが出る。中止すると Cancelled になる', async ({ page, server }) => {
  await page.goto('/tasks')
  await row(page, HELD_PROFILE.title).getByRole('link', { name: HELD_PROFILE.title }).click()
  await expect(page).toHaveURL(`/tasks/${HELD_PROFILE.id}`)

  const held = page.getByTestId('held-review')
  await expect(held.getByRole('heading', { name: '取り込みの保留' })).toBeVisible()
  await expect(held.getByTestId('held-cause')).toContainText('スペシャルハーネスへ直接振り分けたが、そのスペシャルハーネスに既定の版が無い')
  await expect(held.getByTestId('held-cause')).toContainText('config_missing_profile')
  await expect(held.getByTestId('held-hint')).toContainText('このスペシャルハーネスに既定の版を設定すると、次の照会で判定をやり直します。')
  await expect(held.getByRole('link', { name: 'スペシャルハーネスを開く' })).toHaveAttribute('href', `/harnesses/${HARNESS.id}`)
  await expect(page.getByRole('link', { name: 'WEB-203' })).toHaveAttribute('href', issueURL('WEB-203'))
  // 続行・再実行のボタンは無く、中止のボタンは1つだけ。
  await expect(page.getByRole('button', { name: /続行/ })).toHaveCount(0)
  await expect(page.getByRole('button', { name: /再実行/ })).toHaveCount(0)
  await expect(page.getByRole('button', { name: /中止/ })).toHaveCount(1)
  await expect(page.getByTestId('routing-review')).toHaveCount(0)
  await screenshot(page, 'task-held')

  await held.getByRole('button', { name: '中止する' }).click()
  await expect(page.getByRole('status').filter({ hasText: '中止を受け付けました。' })).toBeVisible()
  await expect(page.getByTestId('held-review')).toHaveCount(0)
  await expect(page.locator('[data-status="cancelled"]')).toHaveText('Cancelled')
  await expect(page.getByRole('button', { name: /再実行|中止/ })).toHaveCount(0)
  const task = (await owner(server, 'GET', `/api/tasks/${HELD_PROFILE.id}`)) as { status: string; executions: unknown[] }
  expect(task).toMatchObject({ status: 'cancelled', executions: [] })

  // 受け入れ条件が無い保留は、Linear の Issue を直す案内を出す。
  await page.goto(`/tasks/${HELD_CRITERIA.id}`)
  const criteria = page.getByTestId('held-review')
  await expect(criteria.getByTestId('held-cause')).toContainText('振り分け先の版が受け入れ条件を求めているのに、Issue に受け入れ条件が無い')
  await expect(criteria.getByTestId('held-hint')).toContainText('Linear の Issue の本文に「受け入れ条件」の見出しで受け入れ条件を書いて更新すると')
  await expect(page.getByRole('button', { name: /続行/ })).toHaveCount(0)
  await expect(page.getByRole('button', { name: /中止/ })).toHaveCount(1)
})
