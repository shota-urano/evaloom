import { spawnSync } from 'node:child_process'
import { mkdtempSync, realpathSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

import { expect, test as base, type Page } from '@playwright/test'

import { loginOwner, run, setupOwner, startEvaloomd } from './evaloomd.ts'

// evaloom-oxe.3: 本人の判断待ちの一覧と詳細（13 3.4、02 5.6）。
// - open の判断待ちが種類で絞り込める。
// - route で提案のラベルと理由、候補が出て、候補から別のラベルを選んで承認できる。
// - approval で操作・対象・content_id・確かめた結果が出て、送信に表示中の contentId が含まれる。
// - check_blocked の続行の送信に表示中の contentId が含まれる。
// - context_conflict と workspace_missing では続行ボタンが出ない。
// - question で質問と選択肢が出て、回答を送れる。
// - 422 で読み直しを促し、409 で最新の状態を出す。withdrawn の理由が文字で出る。
// - `/decisions/<id>` を直接開ける。幅 375px の画面で詳細を読めて回答できる。
//
// global-setup がビルドしたバイナリを一時 DB と空きポートで別に起動する。判断待ちは Runner の段の報告から作られるので、
// 報告の後と同じ形の行（タスク・Execution・owner_decisions・approvals・route_proposals・routes）を DB へ直接入れ、
// 本物の一覧・詳細・回答の API を通して確かめる。回答はサーバー側の状態遷移まで通る（200）ことを確かめる。
// 追跡ファイル（docs/design/screenshots など）は書かない。

const NOW = '2026-11-19T00:00:00Z'
const PROFILE = '01JE2E0P0000000000000000A1'
const RUNNER = 'local'

/** ULID の形（01 3.1）の ID。n は表示順（大きいほど新しい）。 */
const ulid = (kind: string, n: number) => `01JE2E${kind}${String(n).padStart(19, '0')}`

const D = {
  route: ulid('R', 10),
  approvalA: ulid('A', 20),
  approvalB: ulid('A', 21),
  checkBlocked: ulid('B', 30),
  checkBlockedStale: ulid('B', 31),
  publishUnknown: ulid('B', 32),
  contextConflict: ulid('B', 33),
  workspaceMissing: ulid('B', 34),
  question: ulid('Q', 40),
  questionMobile: ulid('Q', 41),
  withdrawn: ulid('B', 50),
} as const
const PROPOSAL = ulid('P', 10)
const APPROVAL_A = ulid('V', 20)
const APPROVAL_B = ulid('V', 21)

type Server = { origin: string; cookie: string; db: string }

const sqlText = (value: unknown) => `'${(typeof value === 'string' ? value : JSON.stringify(value)).replaceAll("'", "''")}'`

function seed(db: string) {
  const statements: string[] = []
  const definition = {
    description: 'E2E の版',
    acceptanceCriteria: 'optional',
    approvals: [
      { operation: 'merge_pull_request', description: 'PR を main へマージする' },
      { operation: 'deploy', description: '本番環境へ出す' },
    ],
  }
  statements.push(
    `INSERT INTO harnesses (id, name, repo_url, repo_path, default_profile_id, approval_waivers, created_at, updated_at)
      VALUES ('e2e-harness', 'E2E ハーネス', 'https://example.com/h.git', '/tmp/e2e-harness', NULL, '[]', '${NOW}', '${NOW}');`,
    `INSERT INTO harness_profiles (id, harness_id, version, commit_sha, tag_name, contract_version, definition, definition_hash, model_id, operation_status, registered_at)
      VALUES ('${PROFILE}', 'e2e-harness', '1.0.0', '${'a'.repeat(40)}', 'v1.0.0', '1', ${sqlText(definition)}, 'hash', 'model', 'provisional', '${NOW}');`,
    ...['e2e-app', 'other-app'].map(
      (id) => `INSERT INTO projects (id, name, harness_id, profile_id, params, approval_waivers, runner_id, created_at, updated_at)
        VALUES ('${id}', '${id}', 'e2e-harness', '${PROFILE}', '{}', '[]', '${RUNNER}', '${NOW}', '${NOW}');`,
    ),
    `INSERT INTO routes (label, project_id, harness_id, created_at, updated_at) VALUES ('repo:e2e-app', 'e2e-app', NULL, '${NOW}', '${NOW}');`,
    `INSERT INTO routes (label, project_id, harness_id, created_at, updated_at) VALUES ('repo:other-app', 'other-app', NULL, '${NOW}', '${NOW}');`,
  )
  let taskNo = 300
  const task = (status: string, title: string, projectId: string | null = 'e2e-app') => {
    taskNo += 1
    const id = `task-${taskNo}`
    statements.push(`INSERT INTO tasks (id, source, project_id, harness_id, title, body, acceptance_criteria, collaboration_mode, status, idempotency_key, created_at, updated_at)
      VALUES ('${id}', 'manual', ${projectId ? `'${projectId}'` : 'NULL'}, NULL, ${sqlText(title)}, '本文です', '条件', 'solo', '${status}', 'idem-${id}', '${NOW}', '${NOW}');`)
    return id
  }
  const execution = (taskId: string, status: string) => {
    const id = ulid('E', taskNo)
    statements.push(`INSERT INTO executions (id, task_id, seq, mode, status, runner_id, route_kind, project_id, profile_id, profile_version, created_at)
      VALUES ('${id}', '${taskId}', 1, 'continue', '${status}', '${RUNNER}', 'project', 'e2e-app', '${PROFILE}', '1.0.0', '${NOW}');`)
    return id
  }
  const decision = (
    id: string,
    kind: string,
    taskId: string,
    executionId: string | null,
    fields: { subjectId?: string; cause?: string; basis?: string; detail: unknown; status?: string; withdrawnReason?: string },
  ) => {
    const value = (v: string | undefined) => (v === undefined ? 'NULL' : sqlText(v))
    statements.push(`INSERT INTO owner_decisions (id, kind, task_id, execution_id, subject_id, cause, basis, detail, status, withdrawn_reason, created_at)
      VALUES ('${id}', '${kind}', '${taskId}', ${value(executionId ?? undefined)}, ${value(fields.subjectId)}, ${value(fields.cause)}, ${value(fields.basis)},
        ${sqlText(fields.detail)}, '${fields.status ?? 'open'}', ${value(fields.withdrawnReason)}, '${NOW}');`)
  }

  // route: 振り分けのラベルが2つ付いていて、AI が repo:e2e-app を提案した。
  const routeTask = task('routing', '振り分けを決めるタスク', null)
  const candidates = [
    { label: 'repo:e2e-app', kind: 'project', id: 'e2e-app', description: 'E2E アプリの説明' },
    { label: 'repo:other-app', kind: 'project', id: 'other-app', description: '別アプリの説明' },
  ]
  statements.push(`INSERT INTO route_proposals (id, task_id, trigger, seen_labels, candidates, status, proposed_label, reason, decision_id, created_at, finished_at)
    VALUES ('${PROPOSAL}', '${routeTask}', 'multiple_labels', ${sqlText(['repo:e2e-app', 'repo:other-app'])}, ${sqlText(candidates)}, 'proposed',
      'repo:e2e-app', 'Issue の本文が E2E アプリの画面に触れているため', '${D.route}', '${NOW}', '${NOW}');`)
  decision(D.route, 'route', routeTask, null, { subjectId: PROPOSAL, basis: PROPOSAL, detail: {} })

  // approval: 同じ Execution で2つの操作が承認待ち（Q155）。
  const approvalTask = task('waiting_approval', '承認を待つタスク')
  const approvalExecution = execution(approvalTask, 'waiting_approval')
  // 確かめる段の結果（needs_approval は pass、02 3.4 check_results）。承認の詳細に summary と finding が出る。
  statements.push(
    `INSERT INTO stage_runs (id, execution_id, stage, attempt, status, started_at, finished_at)
      VALUES ('${ulid('S', 20)}', '${approvalExecution}', 'check', 1, 'needs_approval', '${NOW}', '${NOW}');`,
    `INSERT INTO check_results (id, execution_id, stage_run_id, result, content_id, summary, findings, created_at)
      VALUES ('${ulid('C', 20)}', '${approvalExecution}', '${ulid('S', 20)}', 'pass', 'sha-merge-0001', '変更した 3 ファイルを確かめた',
        ${sqlText([{ rule: 'large_diff', severity: 'warning', message: '差分が大きい', location: { path: 'src/app.ts', line: 40 } }])}, '${NOW}');`,
  )
  for (const [decisionId, approvalId, operation, contentId, target] of [
    [D.approvalA, APPROVAL_A, 'merge_pull_request', 'sha-merge-0001', { label: 'example/repo#12', url: 'https://example.com/example/repo/pull/12' }],
    [D.approvalB, APPROVAL_B, 'deploy', 'sha-deploy-0002', { label: '本番環境', url: 'https://example.com/deploy' }],
  ] as const) {
    decision(decisionId, 'approval', approvalTask, approvalExecution, {
      subjectId: approvalId,
      basis: contentId,
      detail: { operation, target, summary: `${operation} の概要`, contentId, requestedStage: 'publish' },
    })
    statements.push(`INSERT INTO approvals (id, task_id, execution_id, decision_id, requested_stage, operation, target, summary, content_id, params_hash, status, created_at)
      VALUES ('${approvalId}', '${approvalTask}', '${approvalExecution}', '${decisionId}', 'publish', '${operation}', ${sqlText(target)}, '${operation} の概要',
        '${contentId}', '${'f'.repeat(64)}', 'pending', '${NOW}');`)
  }

  const blocked = (id: string, cause: string, detail: unknown, basis?: string, title = `${cause} のタスク`) => {
    const taskId = task('blocked', title)
    decision(id, 'blocked', taskId, execution(taskId, 'blocked'), { cause, basis, detail })
  }
  const checkDetail = (summary: string) => ({
    message: 'check blocked',
    summary,
    findings: [{ rule: 'no_secrets', severity: 'error', message: '鍵らしい文字列がある', location: { path: 'src/config.ts', line: 12 } }],
    approvalRequests: [],
  })
  blocked(D.checkBlocked, 'check_blocked', checkDetail('確かめる段が止まった'), 'content-check-0001')
  blocked(D.checkBlockedStale, 'check_blocked', checkDetail('読み直しを確かめる'), 'content-stale-v1')
  blocked(D.publishUnknown, 'publish_unknown', { message: '出す段の結果が分からない', summary: null })
  blocked(D.contextConflict, 'context_conflict', { message: '固定した文脈に矛盾がある' })
  blocked(D.workspaceMissing, 'workspace_missing', { message: '再開に要る作業領域が削除済みです。中止して fresh で再実行してください。', stage: 'work' })

  for (const id of [D.question, D.questionMobile]) {
    const taskId = task('blocked', '質問のあるタスク')
    decision(id, 'question', taskId, execution(taskId, 'blocked'), { cause: 'question', detail: { question: 'テストの古い期待値を残しますか、消しますか？', options: ['残す', '消す'] } })
  }

  // 取り下げ済み（期限切れ）。
  const withdrawnTask = task('cancelled', '取り下げのタスク')
  decision(D.withdrawn, 'blocked', withdrawnTask, execution(withdrawnTask, 'cancelled'), {
    cause: 'publish_unknown',
    detail: { message: '取り下げた' },
    status: 'withdrawn',
    withdrawnReason: 'expired',
  })

  run('sqlite3', [db, `BEGIN;\n${statements.join('\n')}\nCOMMIT;`])
}

async function login(origin: string): Promise<string> {
  const response = await loginOwner(origin)
  const cookie = /evaloom_session=([^;]+)/.exec(response.headers.getSetCookie().join('\n'))?.[1]
  if (!response.ok || !cookie) throw new Error(`ログインに失敗した: ${response.status}`)
  return cookie
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
      const temp = realpathSync(mkdtempSync(join(tmpdir(), 'evaloom-e2e-oxe3-')))
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
  storageState: async ({ worker }, provide) => provide({ cookies: [sessionCookie(worker.cookie)], origins: [] }),
})

/** ボタンを押し、回答の要求の本文と応答の状態を返す。 */
async function answerBy(page: Page, id: string, click: () => Promise<void>) {
  const [request] = await Promise.all([page.waitForRequest((r) => new URL(r.url()).pathname === `/api/decisions/${id}/answer`), click()])
  const response = await request.response()
  return { body: request.postDataJSON() as Record<string, unknown>, status: response?.status() }
}

test('open の判断待ちを種類で絞り込める', async ({ page }) => {
  await page.goto('/decisions')
  const list = page.getByRole('list', { name: '判断待ち' })
  // 開いているのは route 1、approval 2、blocked 5、question 2 の 10件。取り下げ済みは出ない。
  await expect(list.getByRole('listitem')).toHaveCount(10)
  await page.getByLabel('種類').selectOption('question')
  await expect(list.getByRole('listitem')).toHaveCount(2)
  for (const item of await list.getByRole('listitem').all()) await expect(item).toHaveAttribute('data-kind', 'question')
  await page.getByLabel('種類').selectOption('blocked')
  await expect(list.getByRole('listitem')).toHaveCount(5)
  await page.getByLabel('種類').selectOption('approval')
  await expect(list.getByRole('listitem')).toHaveCount(2)
  await page.getByLabel('種類').selectOption('route')
  await expect(list.getByRole('listitem')).toHaveCount(1)
  // 一覧から詳細へ移れる。
  await list.getByRole('link', { name: '振り分けの承認' }).click()
  await expect(page).toHaveURL(new RegExp(`/decisions/${D.route}$`))
})

test('取り下げの一覧と詳細で、取り下げの理由が文字で出る', async ({ page }) => {
  await page.goto('/decisions')
  await page.getByRole('button', { name: '取り下げ' }).click()
  await expect(page.getByRole('list', { name: '判断待ち' }).getByRole('listitem')).toHaveCount(1)
  await expect(page.getByText('取り下げの理由: 期限切れ')).toBeVisible()
  await page.goto(`/decisions/${D.withdrawn}`)
  await expect(page.getByTestId('withdrawn-reason')).toHaveText('期限切れ')
  await expect(page.getByRole('button', { name: '続行する' })).toHaveCount(0)
})

test('route: 提案のラベルと理由、候補が出て、候補から別のラベルを選んで承認できる', async ({ page }) => {
  await page.goto(`/decisions/${D.route}`)
  await expect(page.getByTestId('proposed-label')).toHaveText('repo:e2e-app')
  await expect(page.getByTestId('proposal-reason')).toHaveText('Issue の本文が E2E アプリの画面に触れているため')
  await expect(page.getByTestId('seen-labels')).toHaveText('repo:e2e-app, repo:other-app')
  await expect(page.getByText('別アプリの説明')).toBeVisible()
  await expect(page.getByRole('radio', { name: /repo:e2e-app/ })).toBeChecked()
  await page.getByRole('radio', { name: /repo:other-app/ }).check()
  const sent = await answerBy(page, D.route, () => page.getByRole('button', { name: '選んだラベルで承認する' }).click())
  expect(sent.body).toEqual({ action: 'approve', label: 'repo:other-app', basis: PROPOSAL })
  expect(sent.status).toBe(200)
  await expect(page.getByText('回答を受け付けました。')).toBeVisible()
  await expect(page.getByText('判断済み').first()).toBeVisible()
})

test('approval: 操作・対象・content_id・確かめた結果が出て、承認の送信に表示中の contentId が含まれる', async ({ page }) => {
  await page.goto(`/decisions/${D.approvalA}`)
  // 同じ Execution の承認待ちを並べて出す（Q155）。
  await expect(page.getByTestId('approval-siblings')).toContainText('2 件')
  const items = page.getByTestId('approval-item')
  await expect(items).toHaveCount(2)
  const merge = page.getByRole('region', { name: '操作の承認 merge_pull_request' })
  await expect(merge.getByTestId('approval-operation')).toHaveText('merge_pull_request')
  await expect(merge.getByText('PR を main へマージする')).toBeVisible()
  await expect(merge.getByTestId('approval-target').getByRole('link', { name: 'example/repo#12' })).toHaveAttribute('href', 'https://example.com/example/repo/pull/12')
  await expect(merge.getByTestId('approval-content-id')).toHaveText('sha-merge-0001')
  // 確かめた結果（check_results の summary と finding）。
  await expect(merge.getByTestId('approval-check-result')).toContainText('変更した 3 ファイルを確かめた')
  await expect(merge.getByTestId('approval-check-result')).toContainText('差分が大きい')
  await expect(merge.getByTestId('approval-check-result')).toContainText('src/app.ts:40')
  const deploy = page.getByRole('region', { name: '操作の承認 deploy' })
  await expect(deploy.getByTestId('approval-content-id')).toHaveText('sha-deploy-0002')

  await merge.getByLabel(/承認の有効時間/).fill('7200')
  const sent = await answerBy(page, D.approvalA, () => merge.getByRole('button', { name: '承認する' }).click())
  expect(sent.body).toEqual({ action: 'approve', contentId: 'sha-merge-0001', ttlSec: 7200 })
  expect(sent.status).toBe(200)
  await expect(page.getByText('回答を受け付けました。')).toBeVisible()
})

test('check_blocked: 確かめた結果が出て、続行の送信に表示中の contentId が含まれる', async ({ page }) => {
  await page.goto(`/decisions/${D.checkBlocked}`)
  await expect(page.getByTestId('blocked-content-id')).toHaveText('content-check-0001')
  await expect(page.getByTestId('check-result')).toContainText('確かめる段が止まった')
  await expect(page.getByTestId('check-result')).toContainText('鍵らしい文字列がある')
  const sent = await answerBy(page, D.checkBlocked, () => page.getByRole('button', { name: '続行する' }).click())
  expect(sent.body).toEqual({ action: 'continue', contentId: 'content-check-0001' })
  expect(sent.status).toBe(200)
  await expect(page.getByText('判断済み').first()).toBeVisible()
})

test('context_conflict と workspace_missing では続行ボタンが出ず、中止だけできる', async ({ page }) => {
  await page.goto(`/decisions/${D.contextConflict}`)
  await expect(page.getByTestId('blocked-message')).toHaveText('固定した文脈に矛盾がある')
  await expect(page.getByRole('button', { name: '中止する' })).toBeVisible()
  await expect(page.getByRole('button', { name: '続行する' })).toHaveCount(0)

  await page.goto(`/decisions/${D.workspaceMissing}`)
  await expect(page.getByTestId('blocked-message')).toContainText('再開に要る作業領域が削除済みです')
  await expect(page.getByRole('button', { name: '続行する' })).toHaveCount(0)
  const sent = await answerBy(page, D.workspaceMissing, () => page.getByRole('button', { name: '中止する' }).click())
  expect(sent.body).toEqual({ action: 'abort' })
  expect(sent.status).toBe(200)
})

test('question: 質問と選択肢が出て、回答を送れる', async ({ page }) => {
  await page.goto(`/decisions/${D.question}`)
  await expect(page.getByTestId('question-text')).toHaveText('テストの古い期待値を残しますか、消しますか？')
  await expect(page.getByRole('radio', { name: '残す' })).toBeVisible()
  await page.getByRole('radio', { name: '消す' }).check()
  await page.getByLabel('回答', { exact: true }).fill('古い期待値は仕様と合わないので消してください')
  const sent = await answerBy(page, D.question, () => page.getByRole('button', { name: '回答を送る' }).click())
  expect(sent.body).toEqual({ action: 'answer', text: '古い期待値は仕様と合わないので消してください', option: '消す' })
  expect(sent.status).toBe(200)
  await expect(page.getByText('回答を受け付けました。')).toBeVisible()
})

test('422: 表示中の内容が変わっていたら読み直しを促し、読み直すと新しい contentId で送れる', async ({ page, server }) => {
  await page.goto(`/decisions/${D.checkBlockedStale}`)
  await expect(page.getByTestId('blocked-content-id')).toHaveText('content-stale-v1')
  // 画面を開いた後で、判断待ちの対象の中身が変わる。
  run('sqlite3', [server.db, `UPDATE owner_decisions SET basis = 'content-stale-v2' WHERE id = '${D.checkBlockedStale}';`])
  const first = await answerBy(page, D.checkBlockedStale, () => page.getByRole('button', { name: '続行する' }).click())
  expect(first.body).toEqual({ action: 'continue', contentId: 'content-stale-v1' })
  expect(first.status).toBe(422)
  await expect(page.getByTestId('decision-stale')).toContainText('内容が変わりました')
  await expect(page.getByRole('button', { name: '続行する' })).toBeDisabled()
  await page.getByRole('button', { name: '画面を読み直す' }).click()
  await expect(page.getByTestId('blocked-content-id')).toHaveText('content-stale-v2')
  const second = await answerBy(page, D.checkBlockedStale, () => page.getByRole('button', { name: '続行する' }).click())
  expect(second.body).toEqual({ action: 'continue', contentId: 'content-stale-v2' })
  expect(second.status).toBe(200)
})

test('409: 判断済み・取り下げ済みだったら最新の状態を読み直して出す', async ({ page, server }) => {
  await page.goto(`/decisions/${D.publishUnknown}`)
  await expect(page.getByRole('button', { name: '続行する' })).toBeVisible()
  // 画面を開いた後で、タスクの中止によって取り下げられる。
  run('sqlite3', [server.db, `UPDATE owner_decisions SET status = 'withdrawn', withdrawn_reason = 'cancelled' WHERE id = '${D.publishUnknown}';`])
  const sent = await answerBy(page, D.publishUnknown, () => page.getByRole('button', { name: '続行する' }).click())
  expect(sent.status).toBe(409)
  await expect(page.getByTestId('decision-conflict')).toContainText('最新の状態を表示しています')
  await expect(page.getByTestId('withdrawn-reason')).toHaveText('タスクの中止')
  await expect(page.getByRole('button', { name: '続行する' })).toHaveCount(0)
})

test('幅 375px の画面で判断待ちの詳細を読めて回答できる', async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 812 })
  await page.goto(`/decisions/${D.questionMobile}`)
  await expect(page.getByTestId('question-text')).toBeVisible()
  // 横にはみ出さない（横スクロールが要らない）。
  const overflow = await page.evaluate(() => {
    const root = (globalThis as unknown as { document: { documentElement: { scrollWidth: number; clientWidth: number } } }).document.documentElement
    return root.scrollWidth - root.clientWidth
  })
  expect(overflow).toBeLessThanOrEqual(0)
  await page.getByRole('radio', { name: '残す' }).check()
  await page.getByLabel('回答', { exact: true }).fill('残してください')
  const button = page.getByRole('button', { name: '回答を送る' })
  await button.scrollIntoViewIfNeeded()
  const box = await button.boundingBox()
  expect(box && box.x >= 0 && box.x + box.width <= 375).toBe(true)
  const sent = await answerBy(page, D.questionMobile, () => button.click())
  expect(sent.body).toEqual({ action: 'answer', text: '残してください', option: '残す' })
  expect(sent.status).toBe(200)
  await expect(page.getByText('回答を受け付けました。')).toBeVisible()
})
