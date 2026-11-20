import { spawnSync } from 'node:child_process'
import { mkdirSync, mkdtempSync, realpathSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { expect, test as base, type Page } from '@playwright/test'

import { loginOwner, run, setupOwner, startEvaloomd } from './evaloomd.ts'

// evaloom-vhe.6: 判断待ちの画面の承認の表示と操作（13 3.4、17 3.4）と Dashboard の承認待ちの件数。
// - 承認待ちの対象（操作、Profile の description、target の label と url、summary、content_id、確かめた結果）が表示される。
// - 承認の送信の本文に、表示中の contentId が入る。
// - approval で 422 を受けたら読み直しを促し、読み直すと新しい contentId で送れる。
// - 承認待ちの件数が Dashboard に出る。
//
// global-setup がビルドしたバイナリを一時 DB と空きポートで別に起動する。承認待ちは check の段の報告から作られるので、
// 報告の後と同じ形の行（タスク・Execution・stage_runs・check_results・owner_decisions・approvals）を DB へ直接入れ、
// 本物の一覧・詳細・回答の API を通して確かめる。スクリーンショットは evaloom-vhe.6-approval.png だけを書く。

const NOW = '2026-11-20T00:00:00Z'
const PROFILE = '01JE2E0P0000000000000000B6'
const RUNNER = 'local'
const SCREENSHOTS = fileURLToPath(new URL('../../docs/design/screenshots/', import.meta.url))

/** ULID の形（01 3.1）の ID。 */
const ulid = (kind: string, n: number) => `01JE2E${kind}${String(n).padStart(19, '0')}`

const D = {
  shown: ulid('A', 10),
  stale: ulid('A', 11),
  answered: ulid('A', 12),
  route: ulid('R', 13),
} as const

type Server = { origin: string; cookie: string; db: string }

const sqlText = (value: unknown) => `'${(typeof value === 'string' ? value : JSON.stringify(value)).replaceAll("'", "''")}'`

function seed(db: string) {
  const statements: string[] = []
  const definition = {
    description: 'E2E の版',
    acceptanceCriteria: 'optional',
    approvals: [
      { operation: 'merge_pull_request', description: 'PR を main へマージする' },
      { operation: 'publish_post', description: '下書きを公開する' },
    ],
  }
  statements.push(
    `INSERT INTO harnesses (id, name, repo_url, repo_path, default_profile_id, approval_waivers, created_at, updated_at)
      VALUES ('e2e-harness', 'E2E ハーネス', 'https://example.com/h.git', '/tmp/e2e-harness', NULL, '[]', '${NOW}', '${NOW}');`,
    `INSERT INTO harness_profiles (id, harness_id, version, commit_sha, tag_name, contract_version, definition, definition_hash, model_id, operation_status, registered_at)
      VALUES ('${PROFILE}', 'e2e-harness', '1.0.0', '${'a'.repeat(40)}', 'v1.0.0', '1', ${sqlText(definition)}, 'hash', 'model', 'provisional', '${NOW}');`,
    `INSERT INTO projects (id, name, harness_id, profile_id, params, approval_waivers, runner_id, created_at, updated_at)
      VALUES ('e2e-app', 'e2e-app', 'e2e-harness', '${PROFILE}', '{}', '[]', '${RUNNER}', '${NOW}', '${NOW}');`,
  )

  let n = 100
  /** 承認待ちの Execution を1つ作り、確かめる段の結果と承認待ち1件を入れる。 */
  const approval = (
    decisionId: string,
    fields: { title: string; operation: string; target: { label: string; url: string }; summary: string; contentId: string; check: { summary: string; findings: unknown[] }; status?: string },
  ) => {
    n += 1
    const taskId = `task-${n}`
    const executionId = ulid('E', n)
    const stageRun = ulid('S', n)
    const approvalId = ulid('V', n)
    const status = fields.status ?? 'open'
    statements.push(
      `INSERT INTO tasks (id, source, project_id, harness_id, title, body, acceptance_criteria, collaboration_mode, status, idempotency_key, created_at, updated_at)
        VALUES ('${taskId}', 'manual', 'e2e-app', NULL, ${sqlText(fields.title)}, '本文です', '条件', 'solo', 'waiting_approval', 'idem-${taskId}', '${NOW}', '${NOW}');`,
      `INSERT INTO executions (id, task_id, seq, mode, status, runner_id, route_kind, project_id, profile_id, profile_version, created_at)
        VALUES ('${executionId}', '${taskId}', 1, 'continue', 'waiting_approval', '${RUNNER}', 'project', 'e2e-app', '${PROFILE}', '1.0.0', '${NOW}');`,
      `INSERT INTO stage_runs (id, execution_id, stage, attempt, status, started_at, finished_at)
        VALUES ('${stageRun}', '${executionId}', 'check', 1, 'needs_approval', '${NOW}', '${NOW}');`,
      `INSERT INTO check_results (id, execution_id, stage_run_id, result, content_id, summary, findings, created_at)
        VALUES ('${ulid('C', n)}', '${executionId}', '${stageRun}', 'pass', '${fields.contentId}', ${sqlText(fields.check.summary)}, ${sqlText(fields.check.findings)}, '${NOW}');`,
      `INSERT INTO owner_decisions (id, kind, task_id, execution_id, subject_id, cause, basis, detail, status, withdrawn_reason, created_at)
        VALUES ('${decisionId}', 'approval', '${taskId}', '${executionId}', '${approvalId}', NULL, '${fields.contentId}',
          ${sqlText({ operation: fields.operation, target: fields.target, summary: fields.summary, contentId: fields.contentId, requestedStage: 'publish' })},
          '${status}', NULL, '${NOW}');`,
      `INSERT INTO approvals (id, task_id, execution_id, decision_id, requested_stage, operation, target, summary, content_id, params_hash, status, created_at)
        VALUES ('${approvalId}', '${taskId}', '${executionId}', '${decisionId}', 'publish', '${fields.operation}', ${sqlText(fields.target)}, ${sqlText(fields.summary)},
          '${fields.contentId}', '${'f'.repeat(64)}', '${status === 'open' ? 'pending' : 'approved'}', '${NOW}');`,
    )
    return { taskId, executionId, approvalId }
  }

  approval(D.shown, {
    title: 'PR をマージするタスク',
    operation: 'merge_pull_request',
    target: { label: 'example/repo#42', url: 'https://example.com/example/repo/pull/42' },
    summary: 'ログイン画面の文言を直す PR をマージする',
    contentId: 'sha-shown-0001',
    check: {
      summary: '変更した 2 ファイルを確かめた',
      findings: [{ rule: 'todo_left', severity: 'warning', message: 'TODO が残っている', location: { path: 'src/login.ts', line: 18 } }],
    },
  })
  approval(D.stale, {
    title: '下書きを公開するタスク',
    operation: 'publish_post',
    target: { label: '週報の下書き', url: 'https://example.com/drafts/7' },
    summary: '週報を公開する',
    contentId: 'draft-rev-1',
    check: { summary: '下書きを確かめた', findings: [] },
  })
  // 判断済みの承認待ちは Dashboard の件数に入らない。
  approval(D.answered, {
    title: '判断済みのタスク',
    operation: 'merge_pull_request',
    target: { label: 'example/repo#1', url: 'https://example.com/example/repo/pull/1' },
    summary: '判断済み',
    contentId: 'sha-answered',
    check: { summary: '確かめた', findings: [] },
    status: 'answered',
  })
  // ほかの種類の判断待ちは承認待ちの件数に入らない。
  statements.push(
    `INSERT INTO tasks (id, source, project_id, harness_id, title, body, acceptance_criteria, collaboration_mode, status, idempotency_key, created_at, updated_at)
      VALUES ('task-route', 'manual', NULL, NULL, '振り分けを決めるタスク', '本文です', '条件', 'solo', 'routing', 'idem-task-route', '${NOW}', '${NOW}');`,
    `INSERT INTO route_proposals (id, task_id, trigger, seen_labels, candidates, status, proposed_label, reason, decision_id, created_at, finished_at)
      VALUES ('${ulid('P', 13)}', 'task-route', 'multiple_labels', '[]', '[]', 'failed', NULL, NULL, '${D.route}', '${NOW}', '${NOW}');`,
    `INSERT INTO owner_decisions (id, kind, task_id, execution_id, subject_id, cause, basis, detail, status, withdrawn_reason, created_at)
      VALUES ('${D.route}', 'route', 'task-route', NULL, '${ulid('P', 13)}', NULL, '${ulid('P', 13)}', '{}', 'open', NULL, '${NOW}');`,
  )

  run('sqlite3', [db, `BEGIN;\n${statements.join('\n')}\nCOMMIT;`])
}

async function login(origin: string): Promise<string> {
  const response = await loginOwner(origin)
  const cookie = /evaloom_session=([^;]+)/.exec(response.headers.getSetCookie().join('\n'))?.[1]
  if (!response.ok || !cookie) throw new Error(`ログインに失敗した: ${response.status}`)
  return cookie
}

const test = base.extend<{ server: Server }, { worker: Server }>({
  worker: [
    // oxlint-disable-next-line no-empty-pattern
    async ({}, provide) => {
      const binary = process.env.EVALOOMD_BINARY
      if (!binary) throw new Error('EVALOOMD_BINARY が無い（e2e/global-setup.ts が設定する）')
      const temp = realpathSync(mkdtempSync(join(tmpdir(), 'evaloom-e2e-vhe6-')))
      const db = join(temp, 'evaloom.db')
      const registered = spawnSync(binary, ['-db', db, 'runner', 'register', '-worktree-root', join(temp, 'worktrees'), '-name', 'ローカル Runner', RUNNER], { encoding: 'utf8' })
      if (registered.status !== 0) throw new Error(`runner register が失敗した: ${registered.stderr}`)
      setupOwner(binary, db)
      seed(db)
      const evaloomd = await startEvaloomd(binary, db, '0')
      try {
        await provide({ origin: evaloomd.origin, cookie: await login(evaloomd.origin), db })
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

/** ボタンを押し、回答の要求の本文と応答の状態を返す。 */
async function answerBy(page: Page, id: string, click: () => Promise<void>) {
  const [request] = await Promise.all([page.waitForRequest((r) => new URL(r.url()).pathname === `/api/decisions/${id}/answer`), click()])
  const response = await request.response()
  return { body: request.postDataJSON() as Record<string, unknown>, status: response?.status() }
}

// 回答で件数が変わる前に数える。テストは playwright.config.ts のとおり順番に1つずつ動く。
test('Dashboard: open の承認待ちの件数が出て、判断済みとほかの種類は数えない', async ({ page }) => {
  await page.goto('/')
  const section = page.getByTestId('dashboard-decisions')
  await expect(section.getByTestId('decision-count-approval')).toHaveText('操作の承認2件')
  await expect(section.getByTestId('decision-count-route')).toHaveText('振り分けの承認1件')
  await expect(section.getByTestId('decision-count-total')).toHaveText('合計3件')
})

test('approval: 操作・description・target・summary・content_id・確かめた結果がすべて出て、送信に表示中の contentId が入る', async ({ page }) => {
  await page.goto(`/decisions/${D.shown}`)
  const item = page.getByRole('region', { name: '操作の承認 merge_pull_request' })
  await expect(item.getByTestId('approval-operation')).toHaveText('merge_pull_request')
  await expect(item.getByText('PR を main へマージする')).toBeVisible()
  const target = item.getByTestId('approval-target').getByRole('link', { name: 'example/repo#42' })
  await expect(target).toHaveAttribute('href', 'https://example.com/example/repo/pull/42')
  await expect(item.getByText('ログイン画面の文言を直す PR をマージする')).toBeVisible()
  await expect(item.getByTestId('approval-content-id')).toHaveText('sha-shown-0001')
  const check = item.getByTestId('approval-check-result')
  await expect(check).toContainText('変更した 2 ファイルを確かめた')
  await expect(check).toContainText('TODO が残っている')
  await expect(check).toContainText('src/login.ts:18')
  // 同じ Execution の承認待ちはこの1件だけ。
  await expect(page.getByTestId('approval-item')).toHaveCount(1)

  mkdirSync(SCREENSHOTS, { recursive: true })
  await page.screenshot({ path: join(SCREENSHOTS, 'evaloom-vhe.6-approval.png'), fullPage: true })

  const sent = await answerBy(page, D.shown, () => item.getByRole('button', { name: '承認する' }).click())
  expect(sent.body).toEqual({ action: 'approve', contentId: 'sha-shown-0001' })
  expect(sent.status).toBe(200)
  await expect(page.getByText('回答を受け付けました。')).toBeVisible()
})

test('approval: 422 を受けたら読み直しを促し、読み直すと新しい contentId で送れる', async ({ page, server }) => {
  await page.goto(`/decisions/${D.stale}`)
  const item = () => page.getByRole('region', { name: '操作の承認 publish_post' })
  await expect(item().getByTestId('approval-content-id')).toHaveText('draft-rev-1')
  // 画面を開いた後で、承認を求める中身の識別子が変わる。
  run('sqlite3', [
    server.db,
    `UPDATE owner_decisions SET basis = 'draft-rev-2', detail = json_set(detail, '$.contentId', 'draft-rev-2') WHERE id = '${D.stale}';
     UPDATE approvals SET content_id = 'draft-rev-2' WHERE decision_id = '${D.stale}';`,
  ])
  const first = await answerBy(page, D.stale, () => item().getByRole('button', { name: '承認する' }).click())
  expect(first.body).toEqual({ action: 'approve', contentId: 'draft-rev-1' })
  expect(first.status).toBe(422)
  await expect(page.getByTestId('decision-stale')).toContainText('内容が変わりました')
  await expect(item().getByRole('button', { name: '承認する' })).toBeDisabled()
  await expect(item().getByRole('button', { name: '却下する' })).toBeDisabled()

  await page.getByRole('button', { name: '画面を読み直す' }).click()
  await expect(page.getByTestId('decision-stale')).toHaveCount(0)
  await expect(item().getByTestId('approval-content-id')).toHaveText('draft-rev-2')
  const second = await answerBy(page, D.stale, () => item().getByRole('button', { name: '承認する' }).click())
  expect(second.body).toEqual({ action: 'approve', contentId: 'draft-rev-2' })
  expect(second.status).toBe(200)
  await expect(page.getByText('回答を受け付けました。')).toBeVisible()
})
