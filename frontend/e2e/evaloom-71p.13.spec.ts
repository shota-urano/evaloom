import { spawnSync } from 'node:child_process'
import { randomBytes } from 'node:crypto'
import { mkdirSync, mkdtempSync, realpathSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { expect, test as base, type Page } from '@playwright/test'

import { loginOwner, run, setupOwner, startEvaloomd, type Evaloomd } from './evaloomd.ts'
import { createRepo, tagProfile, type Repo } from '../../scripts/fixtures.mts'

// evaloom-71p.13: Approvals 画面（承認待ち一覧、詳細、承認・却下、履歴）と Dashboard の承認待ち（17 3.3・3.4・3.6・5章・6章）。
//
// 52y.6 と同じく、global-setup がビルドしたバイナリを空きポートで別に起動し、Runner の役はテストが Runner 用 API を直接呼んで務める。
// 承認待ちは、Git Publisher と同じ工程内報告（pushed・pr）と publishing→succeeded の報告で Control Plane に作らせる（17 3.3）。
// verifying から publishing までの Gate 2 の計測（基準値・テスト実行）は本画面の対象外なので、その結果にあたる行
// （Gate 2・3 の pass、verified_sha、branch_name、Execution の publishing）だけを DB へ直接入れる。

const SCREENSHOTS = fileURLToPath(new URL('../../docs/design/screenshots/', import.meta.url))
const HARNESS = 'claude-developer'
const PROJECTS = [
  { id: 'alpha-app', name: 'Alpha アプリ' },
  { id: 'beta-app', name: 'Beta アプリ' },
] as const
const BASE_SHA = 'a'.repeat(40)

type Server = { origin: string; cookie: string; runnerToken: string; db: string }

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

/** Runner 用 API を Runner トークンで呼ぶ。 */
async function runner(server: Server, method: string, path: string, body?: unknown, contentType = 'application/json') {
  const response = await fetch(`${server.origin}${path}`, {
    method,
    headers: { Authorization: `Bearer ${server.runnerToken}`, 'Content-Type': contentType, Accept: 'application/json' },
    body: body === undefined ? undefined : typeof body === 'string' ? body : JSON.stringify(body),
  })
  return { status: response.status, text: await response.text() }
}

async function reportResult(server: Server, executionId: string, body: Record<string, unknown>) {
  const response = await runner(server, 'POST', `/api/executions/${executionId}/result`, body)
  expect(response.status, `result: ${response.text}`).toBe(204)
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
  const random = [...randomBytes(16)].map((b) => CROCKFORD[b % 32]).join('')
  return head + random
}

const sha = () => randomBytes(20).toString('hex')

type Approval = { id: string; taskId: string; executionId: string; contentVersion: string; status: string; target: { prNumber: number } }
type Pending = { taskId: string; title: string; executionId: string; headSha: string; prNumber: number; prUrl: string; diff: string; approval: Approval }

let prSeq = 100

/**
 * タスクを作り、Runner として claim から PR 作成までを報告して、承認待ち（waiting_approval と pending の承認）にする。
 * 差分は Runner の成果物の登録（kind=diff）で入れる。
 */
async function openApproval(server: Server, projectId: string, title: string): Promise<Pending> {
  const task = (await owner(
    server,
    'POST',
    '/api/tasks',
    { projectId, title, body: `${title} の本文`, acceptanceCriteria: `${title} が動く` },
    { 'Idempotency-Key': crypto.randomUUID() },
  )) as { id: string }
  const claimed = await runner(server, 'POST', '/api/runners/local/claim')
  expect(claimed.status, `claim: ${claimed.text}`).toBe(200)
  const executionId = (JSON.parse(claimed.text) as { execution: { id: string; taskId: string } }).execution.id
  const headSha = sha()
  await reportResult(server, executionId, { fromStatus: 'claimed', status: 'preparing' })
  await reportResult(server, executionId, { fromStatus: 'preparing', status: 'running' })
  await reportResult(server, executionId, { fromStatus: 'running', status: 'verifying', baseSha: BASE_SHA, harnessHeadSha: headSha })

  const diff = `diff --git a/src/${task.id}.ts b/src/${task.id}.ts\n--- a/src/${task.id}.ts\n+++ b/src/${task.id}.ts\n@@ -1 +1 @@\n-export const value = 1\n+export const value = 2 // ${title}\n`
  const artifact = await runner(server, 'POST', `/api/executions/${executionId}/artifacts?artifactId=${ulid()}&kind=diff&redacted=0`, diff, 'application/octet-stream')
  expect(artifact.status, `artifact: ${artifact.text}`).toBe(204)

  // Gate 2・3 を通った結果（10-verification-gates）と、verifying→publishing の後の Execution の値。
  const branch = `evaloom/${task.id}-approval`
  const now = new Date().toISOString()
  run('sqlite3', [
    server.db,
    `BEGIN;
     INSERT INTO gate_results (id, execution_id, gate, result, base_sha, target_sha, findings, created_at)
       VALUES ('${ulid()}', '${executionId}', 2, 'pass', '${BASE_SHA}', '${headSha}', '[]', '${now}');
     INSERT INTO gate_results (id, execution_id, gate, result, base_sha, target_sha, findings, created_at)
       VALUES ('${ulid()}', '${executionId}', 3, 'pass', '${BASE_SHA}', '${headSha}', '[]', '${now}');
     UPDATE executions SET status = 'publishing', verified_sha = '${headSha}', branch_name = '${branch}' WHERE id = '${executionId}';
     COMMIT;`,
  ])

  const prNumber = prSeq++
  const prUrl = `https://github.com/example/${projectId}/pull/${prNumber}`
  await reportResult(server, executionId, { fromStatus: 'publishing', status: 'publishing', pushedSha: headSha, branchName: branch })
  await reportResult(server, executionId, { fromStatus: 'publishing', status: 'publishing', prNumber, prUrl, prCreated: true })
  await reportResult(server, executionId, { fromStatus: 'publishing', status: 'succeeded', usage: { wallTimeMs: 0 } })

  const list = (await owner(server, 'GET', '/api/approvals?status=pending')) as { items: Approval[] }
  const approval = list.items.find((a) => a.executionId === executionId)
  if (!approval) throw new Error(`承認待ちができていない: ${JSON.stringify(list.items)}`)
  expect(approval.contentVersion).toBe(headSha)
  return { taskId: task.id, title, executionId, headSha, prNumber, prUrl, diff, approval }
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
      const temp = realpathSync(mkdtempSync(join(tmpdir(), 'evaloom-e2e-71p13-')))
      const db = join(temp, 'evaloom.db')
      const harness = createRepo(temp, 'harness')
      tagProfile(harness.path, '1.0.0', 'first')
      // runner register はトークンを一度だけ標準エラーに出す。
      const registered = spawnSync(binary, ['-db', db, 'runner', 'register', '-worktree-root', join(temp, 'worktrees'), '-name', 'ローカル Runner', 'local'], { encoding: 'utf8' })
      const runnerToken = registered.stderr.trim()
      if (registered.status !== 0 || !/^[A-Za-z0-9_-]{43}$/.test(runnerToken)) throw new Error(`runner register が失敗した: ${registered.stderr}`)
      seedHarness(db, harness)
      // 差分の成果物を保存できるよう、成果物の保存先を与えて起動する。
      setupOwner(binary, db)
      const evaloomd: Evaloomd = await startEvaloomd(binary, db, '0', undefined, ['-data-dir', join(temp, 'data'), '-artifact-max-bytes', '1048576'])
      try {
        const server: Server = { origin: evaloomd.origin, cookie: await login(evaloomd.origin), runnerToken, db }
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

const pendingCount = async (server: Server) => ((await owner(server, 'GET', '/api/approvals?status=pending')) as { items: unknown[] }).items.length

/** Approvals 一覧の承認待ちから、対象の PR のリンクで詳細を開く。 */
async function openDetail(page: Page, pending: Pending, projectId: string) {
  await page.goto('/approvals')
  const table = page.getByRole('table', { name: '承認待ち' })
  await table.getByRole('link', { name: `example/${projectId}#${pending.prNumber}` }).click()
  await expect(page).toHaveURL(`/approvals/${pending.approval.id}`)
  await expect(page.getByRole('heading', { name: '承認の詳細' })).toBeVisible()
}

test('承認待ちの件数が Dashboard に出て、承認待ちの一覧から詳細へ移れる', async ({ page, server }) => {
  const first = await openApproval(server, PROJECTS[0].id, 'Alpha のログイン修正')
  const second = await openApproval(server, PROJECTS[1].id, 'Beta の設定画面')
  const expected = await pendingCount(server)
  expect(expected).toBeGreaterThanOrEqual(2)

  await page.goto('/')
  await expect(page.getByTestId('approval-count-pending').locator('dd')).toHaveText(`${expected}件`)
  const list = page.getByRole('list', { name: '承認待ち' })
  await expect(list.getByRole('link', { name: `example/${PROJECTS[0].id}#${first.prNumber}` })).toHaveAttribute('href', `/approvals/${first.approval.id}`)
  await expect(list.getByRole('link', { name: `example/${PROJECTS[1].id}#${second.prNumber}` })).toBeVisible()
  mkdirSync(SCREENSHOTS, { recursive: true })
  await page.screenshot({ path: join(SCREENSHOTS, 'evaloom-71p.13-dashboard.png'), fullPage: true })

  await page.getByRole('link', { name: 'Approvals 画面を開く' }).click()
  await expect(page).toHaveURL('/approvals')
  const table = page.getByRole('table', { name: '承認待ち' })
  await expect(table.locator(`[data-approval-id="${first.approval.id}"]`)).toContainText(first.headSha.slice(0, 12))
  await expect(table.locator(`[data-approval-id="${second.approval.id}"]`)).toContainText(second.headSha.slice(0, 12))
  await page.screenshot({ path: join(SCREENSHOTS, 'evaloom-71p.13-approvals.png'), fullPage: true })
})

test('詳細に PR の URL・head SHA・Gate の結果・差分が出て、承認の送信に表示中の contentVersion が含まれる', async ({ page, server }) => {
  const pending = await openApproval(server, PROJECTS[0].id, 'Alpha の承認')
  await openDetail(page, pending, PROJECTS[0].id)

  await expect(page.getByTestId('approval-pr-url').getByRole('link', { name: pending.prUrl })).toHaveAttribute('href', pending.prUrl)
  await expect(page.getByTestId('approval-content-version')).toHaveText(pending.headSha)
  const gates = page.getByRole('list', { name: 'Gate の結果' })
  await expect(gates.locator('[data-gate="2"]')).toContainText('pass')
  await expect(gates.locator('[data-gate="3"]')).toContainText('pass')
  await expect(gates).toContainText(pending.headSha)
  await expect(page.getByLabel('差分')).toContainText('+export const value = 2 // Alpha の承認')
  await page.screenshot({ path: join(SCREENSHOTS, 'evaloom-71p.13-approval-detail.png'), fullPage: true })

  const displayed = await page.getByTestId('approval-content-version').textContent()
  const request = page.waitForRequest((r) => r.url().endsWith(`/api/approvals/${pending.approval.id}/decision`) && r.method() === 'POST')
  await page.getByRole('button', { name: '承認する' }).click()
  expect((await request).postDataJSON()).toEqual({ decision: 'approve', contentVersion: displayed })
  await expect(page.getByRole('status').filter({ hasText: '承認を受け付けました。' })).toBeVisible()
  await expect(page.getByRole('button', { name: '承認する' })).toHaveCount(0)
  await expect(page.locator('[data-status="approved"]')).toBeVisible()
  const task = (await owner(server, 'GET', `/api/tasks/${pending.taskId}`)) as { status: string; executions: { status: string }[] }
  expect(task.status).toBe('waiting_approval')
  expect(task.executions.at(-1)?.status).toBe('merging')

  // 承認済みの操作を Runner として受け取り、消費してマージ結果を報告する（後のテストの claim の対象にしないため）。
  const claimed = await runner(server, 'POST', '/api/runners/local/claim')
  expect(claimed.status).toBe(200)
  expect((JSON.parse(claimed.text) as { operation: { approvalId: string } }).operation.approvalId).toBe(pending.approval.id)
  const consumed = await runner(server, 'POST', `/api/approvals/${pending.approval.id}/consume`, { headSha: pending.headSha, baseBranch: 'main', open: true, merged: false })
  expect(JSON.parse(consumed.text)).toMatchObject({ allowed: true, contentVersion: pending.headSha })
  await reportResult(server, pending.executionId, { fromStatus: 'merging', status: 'succeeded', mergeCommitSha: sha() })

  // 履歴に consumed として出る。
  await page.goto('/approvals')
  const history = page.getByRole('table', { name: '履歴' })
  await expect(history.locator(`[data-approval-id="${pending.approval.id}"]`).locator('[data-status]')).toHaveText('consumed')
  await expect(page.getByRole('table', { name: '承認待ち' }).locator(`[data-approval-id="${pending.approval.id}"]`)).toHaveCount(0)
})

test('表示後に対象の head SHA が変わると承認の送信が 422 になり、読み直しを促す', async ({ page, server }) => {
  const pending = await openApproval(server, PROJECTS[1].id, 'Beta の内容変更')
  await openDetail(page, pending, PROJECTS[1].id)
  await expect(page.getByTestId('approval-content-version')).toHaveText(pending.headSha)

  // 画面に表示した後で、承認待ちの行の content_version を別の head SHA に変える。
  const changed = sha()
  run('sqlite3', [server.db, `UPDATE approvals SET content_version = '${changed}' WHERE id = '${pending.approval.id}';`])

  const response = page.waitForResponse((r) => r.url().endsWith(`/api/approvals/${pending.approval.id}/decision`))
  await page.getByRole('button', { name: '承認する' }).click()
  const answered = await response
  expect(answered.status()).toBe(422)
  expect(answered.request().postDataJSON()).toEqual({ decision: 'approve', contentVersion: pending.headSha })
  const stale = page.getByTestId('approval-stale')
  await expect(stale).toContainText('内容が変わりました')
  await expect(stale).toContainText('画面を読み直して')
  await expect(page.getByRole('button', { name: '承認する' })).toBeDisabled()
  // サーバー側では判断が記録されていない。
  expect(((await owner(server, 'GET', `/api/approvals/${pending.approval.id}`)) as Approval).status).toBe('pending')

  await stale.getByRole('button', { name: '画面を読み直す' }).click()
  await expect(stale).toHaveCount(0)
  await expect(page.getByTestId('approval-content-version')).toHaveText(changed)

  // 読み直した内容で却下する。送信には読み直した後の値が入る。
  const request = page.waitForRequest((r) => r.url().endsWith(`/api/approvals/${pending.approval.id}/decision`))
  await page.getByRole('button', { name: '却下する' }).click()
  expect((await request).postDataJSON()).toEqual({ decision: 'reject', contentVersion: changed })
  await expect(page.getByRole('status').filter({ hasText: '却下を受け付けました。' })).toBeVisible()
  await expect(page.locator('[data-status="rejected"]')).toBeVisible()
  expect(((await owner(server, 'GET', `/api/tasks/${pending.taskId}`)) as { status: string }).status).toBe('failed')
})
