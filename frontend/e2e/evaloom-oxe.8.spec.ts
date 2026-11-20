import { spawnSync } from 'node:child_process'
import { mkdirSync, mkdtempSync, realpathSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { expect, test as base, type Page } from '@playwright/test'

import { loginOwner, run, setupOwner, startEvaloomd } from './evaloomd.ts'
import { createRepo, git, tagProfile } from '../../scripts/fixtures.mts'

// evaloom-oxe.8: Tasks 一覧とタスク作成（13 3.2）。
// - 振り分け先の Profile が required のとき受け入れ条件が空だと送信できない。
// - 二重クリックでタスクが2件にならない。
// - 状態と振り分け先（Project かスペシャルハーネス）で絞り込める。
// - 一覧に outputs のリンクと Linear の識別子が出る。
//
// global-setup がビルドしたバイナリを一時 DB と空きポートで別に起動する。スペシャルハーネス・版・既定の版・Project は
// 本物の API で登録し、タスクも本物の POST /api/tasks で作る。outputs（出す段の報告の後に Control Plane が書く値）と
// Linear の Issue とのつながり（取り込みが作る行）は、その後と同じ形の行を DB へ直接入れ、本物の GET /api/tasks を通して確かめる。
// 追跡ファイルはスクリーンショットだけを書く。

const SCREENSHOTS = fileURLToPath(new URL('../../docs/design/screenshots/', import.meta.url))
const RUNNER = 'local'
// engineer の版は受け入れ条件を必須にする。writer の版は必須にしない（scripts/fixtures.mts の tagProfile）。
const ENGINEER = { id: 'engineer', name: 'エンジニア' }
const WRITER = { id: 'writer', name: 'ライター' }
const PROJECT = { id: 'alpha-app', name: 'Alpha アプリ' }

type Server = { origin: string; cookie: string; db: string }

/** 受け入れ条件を必須にした契約 v1 の Profile 定義（契約 5章・5.2）を commit し、profile/v<版> を付ける。 */
function tagRequiredProfile(repo: string, version: string) {
  const files: Record<string, string> = {
    'evaloom/work/AGENTS.md': 'E2E instructions\n',
    'evaloom/check/run': '#!/bin/sh\n',
    'evaloom/publish/run': '#!/bin/sh\n',
    'evaloom/profile.yaml': `contractVersion: "1"
profileVersion: "${version}"
name: "e2e-engineer"
description: "受け入れ条件を必須にするハーネス"
acceptanceCriteria: required
stages:
  work:
    microHarness: "claude_code"
    model: "claude-e2e-model"
    instructions: ["evaloom/work/AGENTS.md"]
    timeoutSec: 600
  check:
    command: ["evaloom/check/run"]
    timeoutSec: 60
  publish:
    command: ["evaloom/publish/run"]
    timeoutSec: 60
`,
  }
  for (const [name, content] of Object.entries(files)) {
    mkdirSync(join(repo, dirname(name)), { recursive: true })
    writeFileSync(join(repo, name), content)
  }
  git(repo, 'add', 'evaloom')
  git(repo, 'commit', '--quiet', '-m', `profile ${version}`)
  git(repo, 'tag', '-f', `profile/v${version}`)
}

/** 本人用 API をセッション Cookie で呼ぶ（テストの前提づくりと件数の確認用）。evaloomd は Origin も照合する。 */
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

type ListedTask = { id: string; title: string; projectId: string; harnessId: string; status: string; acceptanceCriteria: string }

async function listTasks(server: Server, query = ''): Promise<ListedTask[]> {
  return ((await owner(server, 'GET', `/api/tasks?limit=200${query}`)) as { items: ListedTask[] }).items
}

async function createTask(server: Server, destination: { projectId: string } | { harnessId: string }, title: string): Promise<ListedTask> {
  return (await owner(
    server,
    'POST',
    '/api/tasks',
    { ...destination, title, body: `${title} の本文`, acceptanceCriteria: `${title} が動く` },
    { 'Idempotency-Key': crypto.randomUUID() },
  )) as ListedTask
}

const test = base.extend<{ server: Server }, { worker: Server }>({
  worker: [
    // oxlint-disable-next-line no-empty-pattern
    async ({}, provide) => {
      const binary = process.env.EVALOOMD_BINARY
      if (!binary) throw new Error('EVALOOMD_BINARY が無い（e2e/global-setup.ts が設定する）')
      // macOS の tmpdir はシンボリックリンク経由なので、evaloomd が保存する実パスに揃える。
      const temp = realpathSync(mkdtempSync(join(tmpdir(), 'evaloom-e2e-oxe8-')))
      const db = join(temp, 'evaloom.db')
      const engineer = createRepo(temp, ENGINEER.id)
      const writer = createRepo(temp, WRITER.id)
      tagRequiredProfile(engineer.path, '1.0.0')
      tagProfile(writer.path, '1.0.0', 'writer')
      const registered = spawnSync(binary, ['-db', db, 'runner', 'register', '-worktree-root', join(temp, 'worktrees'), '-name', 'ローカル Runner', RUNNER], { encoding: 'utf8' })
      if (registered.status !== 0) throw new Error(`runner register が失敗した: ${registered.stderr}`)
      setupOwner(binary, db)
      const evaloomd = await startEvaloomd(binary, db, '0')
      try {
        const response = await loginOwner(evaloomd.origin)
        const cookie = /evaloom_session=([^;]+)/.exec(response.headers.getSetCookie().join('\n'))?.[1]
        if (!response.ok || !cookie) throw new Error(`ログインに失敗した: ${response.status}`)
        const server = { origin: evaloomd.origin, cookie, db }
        const profiles: Record<string, string> = {}
        for (const [harness, repo] of [
          [ENGINEER, engineer],
          [WRITER, writer],
        ] as const) {
          await owner(server, 'POST', '/api/harnesses', { id: harness.id, name: harness.name, repoPath: repo.path, repoUrl: `https://example.com/${harness.id}.git` })
          const profile = (await owner(server, 'POST', '/api/harness-profiles', { harnessId: harness.id, version: '1.0.0' })) as { id: string }
          profiles[harness.id] = profile.id
          await owner(server, 'PATCH', `/api/harnesses/${harness.id}`, { defaultProfileId: profile.id })
        }
        await owner(server, 'POST', '/api/projects', {
          id: PROJECT.id,
          name: PROJECT.name,
          harnessId: WRITER.id,
          profileId: profiles[WRITER.id],
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
  storageState: async ({ worker }, provide) =>
    provide({
      cookies: [
        { name: 'evaloom_session', value: worker.cookie, domain: '127.0.0.1', path: '/', expires: -1, httpOnly: true, secure: false, sameSite: 'Strict' },
      ],
      origins: [],
    }),
})

async function screenshot(page: Page, screen: string) {
  mkdirSync(SCREENSHOTS, { recursive: true })
  await page.screenshot({ path: join(SCREENSHOTS, `evaloom-oxe.8-${screen}.png`), fullPage: true })
}

function field(page: Page, label: string) {
  return page.locator('[data-field]').filter({ has: page.getByLabel(label, { exact: true }) })
}

const taskRows = (page: Page) => page.getByRole('table', { name: 'タスク' }).locator('tbody tr')

async function openForm(page: Page) {
  await page.goto('/')
  await page.getByRole('navigation', { name: '画面' }).getByRole('link', { name: 'Tasks' }).click()
  await expect(page.getByRole('heading', { name: 'Tasks' })).toBeVisible()
  await page.getByRole('link', { name: 'タスクを作成' }).click()
  await expect(page).toHaveURL('/tasks/_new')
  await expect(page.getByRole('heading', { name: 'タスクの作成' })).toBeVisible()
}

async function fillForm(page: Page, destination: string, title: string, acceptanceCriteria: string) {
  await page.getByLabel('振り分け先').selectOption({ label: destination })
  await page.getByLabel('件名').fill(title)
  await page.getByLabel('本文').fill(`${title} の本文`)
  await page.getByLabel('受け入れ条件').fill(acceptanceCriteria)
}

test('振り分け先の Profile が required のとき受け入れ条件が空だと送信できない', async ({ page, server }) => {
  await openForm(page)
  const title = '受け入れ条件の無いタスク'
  const posts: string[] = []
  page.on('request', (request) => {
    if (request.method() === 'POST' && new URL(request.url()).pathname === '/api/tasks') posts.push(request.url())
  })
  await fillForm(page, ENGINEER.name, title, '')
  await expect(page.getByLabel('受け入れ条件')).toHaveAttribute('required', '')
  await page.getByRole('button', { name: '作成' }).click()
  await expect(field(page, '受け入れ条件').locator('[data-slot=field-error]')).toHaveText('入力してください')
  await expect(page.getByLabel('受け入れ条件')).toHaveAttribute('aria-invalid', 'true')
  await expect(page).toHaveURL('/tasks/_new')
  await screenshot(page, 'task-form')

  // 空白だけでも送らない（04 3.1）。
  await page.getByLabel('受け入れ条件').fill('   \n  ')
  await page.getByRole('button', { name: '作成' }).click()
  await expect(field(page, '受け入れ条件').locator('[data-slot=field-error]')).toHaveText('入力してください')
  await expect(page).toHaveURL('/tasks/_new')
  expect(posts).toHaveLength(0)
  expect((await listTasks(server)).filter((t) => t.title === title)).toHaveLength(0)

  // 必須にしない Profile の振り分け先（Project）へ変えると、受け入れ条件が空でも作れる。
  await page.getByLabel('振り分け先').selectOption({ label: PROJECT.name })
  await expect(page.getByLabel('受け入れ条件')).not.toHaveAttribute('required', '')
  await page.getByLabel('受け入れ条件').fill('')
  await page.getByRole('button', { name: '作成' }).click()
  await expect(page).toHaveURL('/tasks')
  await expect(page.getByRole('status')).toContainText(`（${title}）を作成しました。`)
  const created = (await listTasks(server)).filter((t) => t.title === title)
  expect(created).toHaveLength(1)
  expect(created[0]).toMatchObject({ projectId: PROJECT.id, harnessId: '', acceptanceCriteria: '', status: 'queued' })
})

test('二重クリックでタスクが2件にならない', async ({ page, server }) => {
  await openForm(page)
  const title = '二重クリックのタスク'
  await fillForm(page, ENGINEER.name, title, 'ログインのテストが通る')

  const posts: string[] = []
  page.on('request', (request) => {
    if (request.method() === 'POST' && new URL(request.url()).pathname === '/api/tasks') posts.push(request.headers()['idempotency-key'] ?? '')
  })
  // 送信ボタンを素早く2回クリックする。
  await page.getByRole('button', { name: '作成' }).dblclick()

  await expect(page).toHaveURL('/tasks')
  await expect(page.getByRole('status')).toContainText(`（${title}）を作成しました。`)
  await expect(taskRows(page).filter({ hasText: title })).toHaveCount(1)
  await expect(taskRows(page).filter({ hasText: title })).toContainText('Queued')
  await expect(taskRows(page).filter({ hasText: title })).toContainText(ENGINEER.name)
  const created = (await listTasks(server)).filter((t) => t.title === title)
  expect(created).toHaveLength(1)
  expect(created[0]).toMatchObject({ harnessId: ENGINEER.id, projectId: '', status: 'queued' })
  expect(posts.length).toBeGreaterThanOrEqual(1)
  expect(posts.every((key) => key !== '' && key === posts[0])).toBe(true)

  // 再読み込みしても1件のまま。
  await page.reload()
  await expect(taskRows(page).filter({ hasText: title })).toHaveCount(1)
})

test('状態と振り分け先で絞り込める', async ({ page, server }) => {
  const queuedEngineer = await createTask(server, { harnessId: ENGINEER.id }, 'エンジニアの改修')
  const queuedAlpha = await createTask(server, { projectId: PROJECT.id }, 'Alpha の記事')
  const cancelledAlpha = await createTask(server, { projectId: PROJECT.id }, 'Alpha の古い依頼')
  await owner(server, 'POST', `/api/tasks/${cancelledAlpha.id}/cancel`)

  await page.goto('/tasks')
  for (const task of [queuedEngineer, queuedAlpha, cancelledAlpha]) await expect(taskRows(page).filter({ hasText: task.title })).toHaveCount(1)
  await expect(taskRows(page).filter({ hasText: cancelledAlpha.title })).toContainText('Cancelled')
  await expect(taskRows(page).filter({ hasText: queuedEngineer.title })).toContainText(`スペシャルハーネス${ENGINEER.name}`)
  await expect(taskRows(page).filter({ hasText: queuedAlpha.title })).toContainText(`Project${PROJECT.name}`)

  // スペシャルハーネスの振り分け先で絞り込む。
  await page.getByLabel('振り分け先').selectOption({ label: ENGINEER.name })
  await expect(page).toHaveURL(`/tasks?harnessId=${ENGINEER.id}`)
  await expect(taskRows(page).filter({ hasText: queuedAlpha.title })).toHaveCount(0)
  await expect(taskRows(page).filter({ hasText: queuedEngineer.title })).toHaveCount(1)
  await expect(taskRows(page)).toHaveCount((await listTasks(server, `&harnessId=${ENGINEER.id}`)).length)
  await expect(taskRows(page).filter({ hasText: PROJECT.name })).toHaveCount(0)

  // Project の振り分け先と状態で絞り込む。
  await page.getByLabel('振り分け先').selectOption({ label: PROJECT.name })
  await expect(page).toHaveURL(`/tasks?projectId=${PROJECT.id}`)
  await expect(taskRows(page).filter({ hasText: queuedEngineer.title })).toHaveCount(0)
  await expect(taskRows(page)).toHaveCount((await listTasks(server, `&projectId=${PROJECT.id}`)).length)
  await page.getByLabel('状態').selectOption({ label: 'Cancelled' })
  await expect(page).toHaveURL(`/tasks?status=cancelled&projectId=${PROJECT.id}`)
  await expect(taskRows(page)).toHaveCount(1)
  await expect(taskRows(page).first()).toContainText(cancelledAlpha.title)
  await screenshot(page, 'tasks-filtered')

  // 状態とスペシャルハーネスを組み合わせる。
  await page.getByLabel('振り分け先').selectOption({ label: ENGINEER.name })
  await expect(page).toHaveURL(`/tasks?status=cancelled&harnessId=${ENGINEER.id}`)
  await expect(page.getByText('条件に合うタスクはありません。')).toBeVisible()
  await page.getByLabel('状態').selectOption({ label: 'Queued' })
  await expect(taskRows(page).filter({ hasText: queuedEngineer.title })).toHaveCount(1)
  await expect(taskRows(page)).toHaveCount((await listTasks(server, `&status=queued&harnessId=${ENGINEER.id}`)).length)

  // 条件は URL に残り、再読み込みしても保たれる。
  await page.reload()
  await expect(page.getByLabel('状態')).toHaveValue('queued')
  await expect(page.getByLabel('振り分け先')).toHaveValue(`harness:${ENGINEER.id}`)
  await expect(taskRows(page).filter({ hasText: queuedAlpha.title })).toHaveCount(0)

  await page.getByRole('button', { name: '絞り込みを解除' }).click()
  await expect(page).toHaveURL('/tasks')
  await expect(taskRows(page).filter({ hasText: cancelledAlpha.title })).toHaveCount(1)
})

test('一覧に outputs のリンクと Linear の識別子が出る', async ({ page, server }) => {
  const published = await createTask(server, { harnessId: ENGINEER.id }, 'PR を出したタスク')
  const plain = await createTask(server, { projectId: PROJECT.id }, 'まだ出していないタスク')
  const outputs = JSON.stringify([
    { kind: 'pull_request', label: 'PR #42', url: 'https://github.com/acme/app/pull/42' },
    { kind: 'preview', label: 'プレビュー', url: 'https://preview.example.com/42' },
  ])
  // 出す段まで成功した後（executions.outputs、02 3.2）と、Linear からの取り込み（linear_issue_links、15 5章）と同じ形の行。
  run('sqlite3', [
    server.db,
    `UPDATE executions SET status = 'succeeded', outputs = '${outputs}' WHERE task_id = '${published.id}';
     UPDATE tasks SET status = 'completed', source = 'linear' WHERE id = '${published.id}';
     INSERT INTO linear_issue_links (id, linear_issue_id, issue_identifier, issue_url, team_id, task_id, attach_seq, created_at)
       VALUES ('01JE2ELINK0000000000000042', 'issue-42', 'ENG-42', 'https://linear.app/acme/issue/ENG-42', 'team-1', '${published.id}', 1, '2026-11-20T00:00:00Z');`,
  ])
  // 本物の API が outputs と Linear の識別子を返している。
  const listed = (await owner(server, 'GET', '/api/tasks?limit=200')) as {
    items: { id: string; linear: { identifier: string } | null; latestExecution: { outputs?: unknown[] } | null }[]
  }
  const item = listed.items.find((t) => t.id === published.id)!
  expect(item.linear?.identifier).toBe('ENG-42')
  expect(item.latestExecution?.outputs).toHaveLength(2)

  await page.goto('/tasks')
  const row = taskRows(page).filter({ hasText: published.title })
  await expect(row).toContainText('Completed')
  await expect(row.getByRole('link', { name: 'PR #42' })).toHaveAttribute('href', 'https://github.com/acme/app/pull/42')
  await expect(row.getByRole('link', { name: 'プレビュー' })).toHaveAttribute('href', 'https://preview.example.com/42')
  await expect(row.getByRole('link', { name: 'ENG-42' })).toHaveAttribute('href', 'https://linear.app/acme/issue/ENG-42')
  // outputs の無いタスクは「なし」、手動作成のタスクに Linear の識別子は出ない。
  const other = taskRows(page).filter({ hasText: plain.title })
  await expect(other.locator('td').nth(4)).toHaveText('なし')
  await expect(other.getByRole('link', { name: /^ENG-/ })).toHaveCount(0)
  await screenshot(page, 'tasks')
})

test('状態の選択肢は 02 5.1 の10状態で、Routing と Waiting Approval で絞り込める', async ({ page, server }) => {
  const routing = await createTask(server, { harnessId: ENGINEER.id }, '振り分けを待つタスク')
  const waiting = await createTask(server, { projectId: PROJECT.id }, '承認を待つタスク')
  // 振り分けの提案待ち（15 3.6）と操作の承認待ち（17）は、Control Plane が状態を進めた後と同じ値を DB に入れる。
  run('sqlite3', [
    server.db,
    `UPDATE tasks SET status = 'routing' WHERE id = '${routing.id}';
     UPDATE tasks SET status = 'waiting_approval' WHERE id = '${waiting.id}';
     UPDATE executions SET status = 'waiting_approval' WHERE task_id = '${waiting.id}';`,
  ])

  await page.goto('/tasks')
  const options = await page.getByLabel('状態').locator('option').allTextContents()
  expect(options).toEqual(['すべて', 'Received', 'Routing', 'Queued', 'Running', 'Waiting Approval', 'Blocked', 'Completed', 'Failed', 'Cancelled', 'Timed Out'])

  // 選択肢の値はどれも本物の API が受け付ける（廃止済みの状態は 400 になる）。
  for (const value of await page.getByLabel('状態').locator('option').evaluateAll((os) => os.map((o) => o.getAttribute("value") ?? ""))) {
    expect((await page.request.get(`/api/tasks?status=${value}`)).status(), value).toBe(200)
  }

  await page.getByLabel('状態').selectOption({ label: 'Routing' })
  await expect(page).toHaveURL('/tasks?status=routing')
  await expect(taskRows(page)).toHaveCount((await listTasks(server, '&status=routing')).length)
  await expect(taskRows(page).filter({ hasText: routing.title })).toContainText('Routing')
  await expect(taskRows(page).filter({ hasText: waiting.title })).toHaveCount(0)

  await page.getByLabel('状態').selectOption({ label: 'Waiting Approval' })
  await expect(page).toHaveURL('/tasks?status=waiting_approval')
  await expect(taskRows(page)).toHaveCount((await listTasks(server, '&status=waiting_approval')).length)
  await expect(taskRows(page).filter({ hasText: waiting.title })).toContainText('Waiting Approval')
  await expect(taskRows(page).filter({ hasText: routing.title })).toHaveCount(0)
  await screenshot(page, 'tasks-waiting-approval')

  // URL に廃止済みの状態があっても、API へ送らずすべてを出す。
  const sent: string[] = []
  page.on('request', (request) => {
    if (new URL(request.url()).pathname === '/api/tasks') sent.push(request.url())
  })
  await page.goto('/tasks?status=verifying')
  await expect(taskRows(page).filter({ hasText: routing.title })).toHaveCount(1)
  await expect(page.getByLabel('状態')).toHaveValue('')
  await expect(page.getByRole('alert')).toHaveCount(0)
  expect(sent.length).toBeGreaterThan(0)
  expect(sent.every((url) => !url.includes('verifying'))).toBe(true)
})
