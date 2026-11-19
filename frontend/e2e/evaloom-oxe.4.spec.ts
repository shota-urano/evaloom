import { spawnSync } from 'node:child_process'
import { mkdirSync, mkdtempSync, realpathSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

import { expect, test as base, type Page } from '@playwright/test'

import { loginOwner, run, setupOwner, startEvaloomd } from './evaloomd.ts'

// evaloom-oxe.4: Projects 一覧・登録・編集・詳細（13 3.5）。
// - 6項目以外の入力欄が無い。
// - 2つの Project を画面から登録でき、それぞれに振り分けのラベルが付く。
// - 承認を省く設定に、選んだ版で waivable の操作だけが並ぶ。
// - 承認を省く操作を足すと確認ダイアログが出て、確認しないと保存されない。
// - 渡す値がスキーマに合わないと 422 の内容が入力欄に出る。
// - ラベルの登録だけが失敗したとき、Project は登録されラベルが無いことが表示される。
//
// global-setup がビルドしたバイナリを一時 DB と空きポートで別に起動する。版（harness_profiles）は Git の tag と
// スペシャルハーネスのリポジトリが要るので、一時の Git リポジトリに params.schema を commit し、その commit を
// 固定した版の行を DB へ直接入れる。Project とラベルの登録は本物の API を画面から通す。追跡ファイルは書かない。

const NOW = '2026-11-19T00:00:00Z'
const RUNNER = 'local'
const PROFILE_V1 = '01JE2E0P0000000000000000V1'
const PROFILE_V2 = '01JE2E0P0000000000000000V2'
const PREFIX = 'repo:'

type Server = { origin: string; cookie: string; db: string }

const sqlText = (value: unknown) => `'${(typeof value === 'string' ? value : JSON.stringify(value)).replaceAll("'", "''")}'`

/** params.schema を commit した一時のリポジトリを作り、commit SHA を返す。 */
function harnessRepo(dir: string): string {
  mkdirSync(join(dir, 'evaloom'), { recursive: true })
  writeFileSync(
    join(dir, 'evaloom', 'params.schema.json'),
    JSON.stringify({
      $schema: 'https://json-schema.org/draft/2020-12/schema',
      type: 'object',
      properties: { repository: { type: 'string' } },
      required: ['repository'],
      additionalProperties: false,
    }),
  )
  const git = (...args: string[]) => run('git', ['-C', dir, '-c', 'user.name=e2e', '-c', 'user.email=e2e@example.com', ...args])
  git('init', '-q')
  git('add', '.')
  git('commit', '-q', '-m', 'schema')
  return git('rev-parse', 'HEAD').trim()
}

function seed(db: string, repo: string, commit: string) {
  const v1 = {
    description: 'E2E の版 1',
    params: { schema: 'evaloom/params.schema.json' },
    approvals: [
      { operation: 'merge_pull_request', description: 'PR を main へマージする', waivable: true },
      { operation: 'deploy', description: '本番環境へ出す' },
      { operation: 'post_comment', description: 'Issue にコメントする', waivable: true },
    ],
  }
  const v2 = { description: 'E2E の版 2', params: { schema: 'evaloom/params.schema.json' }, approvals: [{ operation: 'deploy', description: '本番環境へ出す', waivable: true }] }
  const profile = (id: string, version: string, definition: unknown) =>
    `INSERT INTO harness_profiles (id, harness_id, version, commit_sha, tag_name, contract_version, definition, definition_hash, model_id, operation_status, registered_at)
      VALUES ('${id}', 'e2e-harness', '${version}', '${commit}', 'v${version}', '1', ${sqlText(definition)}, 'hash-${version}', 'model', 'provisional', '${NOW}');`
  const statements = [
    `INSERT INTO settings (key, value, updated_at) VALUES ('linear.routingLabelPrefix', ${sqlText(JSON.stringify(PREFIX))}, '${NOW}');`,
    `INSERT INTO harnesses (id, name, repo_url, repo_path, default_profile_id, approval_waivers, created_at, updated_at)
      VALUES ('e2e-harness', 'E2E ハーネス', 'https://example.com/h.git', ${sqlText(repo)}, NULL, '[]', '${NOW}', '${NOW}');`,
    profile(PROFILE_V1, '1.0.0', v1),
    profile(PROFILE_V2, '2.0.0', v2),
    // ラベルの登録だけを失敗させるため、repo:taken を使っている Project。
    `INSERT INTO projects (id, name, harness_id, profile_id, params, approval_waivers, runner_id, created_at, updated_at)
      VALUES ('holder', 'holder', 'e2e-harness', '${PROFILE_V1}', '{"repository":"owner/holder"}', '[]', '${RUNNER}', '${NOW}', '${NOW}');`,
    `INSERT INTO routes (label, project_id, harness_id, created_at, updated_at) VALUES ('repo:taken', 'holder', NULL, '${NOW}', '${NOW}');`,
  ]
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
      const temp = realpathSync(mkdtempSync(join(tmpdir(), 'evaloom-e2e-oxe4-')))
      const db = join(temp, 'evaloom.db')
      const repo = join(temp, 'harness')
      const registered = spawnSync(binary, ['-db', db, 'runner', 'register', '-worktree-root', join(temp, 'worktrees'), '-name', 'ローカル Runner', RUNNER], { encoding: 'utf8' })
      if (registered.status !== 0) throw new Error(`runner register が失敗した: ${registered.stderr}`)
      setupOwner(binary, db)
      seed(db, repo, harnessRepo(repo))
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

/** 登録画面で6項目を埋める。送信はしない。 */
async function fillProject(page: Page, input: { id: string; label?: string; params?: string; profile?: string }) {
  await page.goto('/projects/_new')
  await page.getByLabel('Project ID').fill(input.id)
  await page.getByLabel('名前', { exact: true }).fill(`${input.id} の名前`)
  await page.getByLabel('振り分けのラベル').fill(input.label ?? `${PREFIX}${input.id}`)
  await page.getByLabel('動かすマシン').selectOption(RUNNER)
  await page.getByLabel('頼むスペシャルハーネス').selectOption('e2e-harness')
  await expect(page.getByLabel('版').locator('option', { hasText: 'v1.0.0' })).toHaveCount(1)
  await page.getByLabel('版').selectOption(input.profile ?? PROFILE_V1)
  await page.getByLabel('渡す値（JSON）').fill(input.params ?? JSON.stringify({ repository: `owner/${input.id}` }))
}

async function getJSON<T>(page: Page, path: string): Promise<T> {
  const response = await page.request.get(path)
  expect(response.status()).toBe(200)
  return (await response.json()) as T
}

type ProjectView = { id: string; routingLabel: string | null; approvalWaivers: string[] }

test('6項目以外の入力欄が無い', async ({ page }) => {
  await page.goto('/projects/_new')
  const form = page.getByRole('form', { name: 'プロジェクトの登録' })
  await expect(form.getByLabel('Project ID')).toBeVisible()
  const names = await form.locator('input, select, textarea').evaluateAll((elements) => elements.map((e) => e.getAttribute('name')))
  // 名前（登録時は Project ID も）、振り分けのラベル、頼むスペシャルハーネスと版、渡す値、動かすマシン。
  // 承認を省く設定は版を選ぶまで並ばない。
  expect(new Set(names)).toEqual(new Set(['id', 'name', 'routingLabel', 'harnessId', 'profileId', 'params', 'runnerId']))
  await page.getByLabel('頼むスペシャルハーネス').selectOption('e2e-harness')
  await page.getByLabel('版').selectOption(PROFILE_V1)
  const after = await form.locator('input, select, textarea').evaluateAll((elements) => elements.map((e) => e.getAttribute('name')))
  expect(new Set(after)).toEqual(new Set([...names, 'approvalWaivers']))
  for (const label of ['既定の役割', 'Linear Team ID', 'パス', 'Git URL', 'ベースブランチ', 'テストコマンド']) {
    await expect(page.getByLabel(label, { exact: true })).toHaveCount(0)
  }
})

test('2つの Project を画面から登録でき、それぞれに振り分けのラベルが付く', async ({ page }) => {
  for (const id of ['first-app', 'second-app']) {
    await fillProject(page, { id, label: `${PREFIX}${id}` })
    await page.getByRole('button', { name: '登録' }).click()
    await expect(page).toHaveURL(new RegExp(`/projects/${id}$`))
    await expect(page.getByTestId('routing-label')).toHaveText(`${PREFIX}${id}`)
  }
  const routes = await getJSON<{ items: { label: string; projectId: string | null }[] }>(page, '/api/routes')
  expect(routes.items).toEqual(
    expect.arrayContaining([
      expect.objectContaining({ label: 'repo:first-app', projectId: 'first-app' }),
      expect.objectContaining({ label: 'repo:second-app', projectId: 'second-app' }),
    ]),
  )
  await page.goto('/projects')
  for (const id of ['first-app', 'second-app']) {
    const row = page.getByRole('row').filter({ has: page.getByRole('link', { name: `${id} の名前` }) })
    await expect(row.getByTestId('routing-label')).toHaveText(`${PREFIX}${id}`)
    await expect(row).toContainText('e2e-harness')
    await expect(row).toContainText('v1.0.0')
  }
})

test('承認を省く設定に、選んだ版で waivable の操作だけが並ぶ', async ({ page }) => {
  await fillProject(page, { id: 'waiver-list' })
  const group = page.getByRole('group', { name: '承認を省く設定' })
  await expect(group.getByRole('checkbox')).toHaveCount(2)
  await expect(group.getByRole('checkbox', { name: /merge_pull_request/ })).toBeVisible()
  await expect(group.getByRole('checkbox', { name: /post_comment/ })).toBeVisible()
  await expect(group.getByRole('checkbox', { name: /deploy/ })).toHaveCount(0)
  // 版を変えると、その版の宣言で並び直す。
  await page.getByLabel('版').selectOption(PROFILE_V2)
  await expect(group.getByRole('checkbox')).toHaveCount(1)
  await expect(group.getByRole('checkbox', { name: /deploy/ })).toBeVisible()
  await expect(page.getByTestId('params-schema')).toContainText('evaloom/params.schema.json')
})

test('承認を省く操作を足すと確認ダイアログが出て、確認しないと保存されない', async ({ page }) => {
  await fillProject(page, { id: 'waiver-confirm', label: `${PREFIX}waiver-confirm` })
  const group = page.getByRole('group', { name: '承認を省く設定' })
  const merge = group.getByRole('checkbox', { name: /merge_pull_request/ })
  await merge.click()
  const dialog = page.getByRole('alertdialog', { name: '承認を省く操作を足しますか' })
  await expect(dialog).toContainText('merge_pull_request')
  await dialog.getByRole('button', { name: 'やめる' }).click()
  await expect(dialog).toHaveCount(0)
  await expect(merge).not.toBeChecked()
  await page.getByRole('button', { name: '登録' }).click()
  await expect(page).toHaveURL(/\/projects\/waiver-confirm$/)
  expect((await getJSON<ProjectView>(page, '/api/projects/waiver-confirm')).approvalWaivers).toEqual([])

  // 確認すると足され、保存される。
  await page.getByRole('link', { name: '編集' }).click()
  await page.getByRole('group', { name: '承認を省く設定' }).getByRole('checkbox', { name: /merge_pull_request/ }).click()
  await page.getByRole('alertdialog').getByRole('button', { name: '承認を省く' }).click()
  await expect(page.getByRole('group', { name: '承認を省く設定' }).getByRole('checkbox', { name: /merge_pull_request/ })).toBeChecked()
  await page.getByRole('button', { name: '保存' }).click()
  await expect(page).toHaveURL(/\/projects\/waiver-confirm$/)
  expect((await getJSON<ProjectView>(page, '/api/projects/waiver-confirm')).approvalWaivers).toEqual(['merge_pull_request'])
})

test('渡す値がスキーマに合わないと 422 の内容が入力欄に出る', async ({ page }) => {
  await fillProject(page, { id: 'bad-params', label: `${PREFIX}bad-params`, params: JSON.stringify({ repository: 42 }) })
  const [response] = await Promise.all([
    page.waitForResponse((r) => new URL(r.url()).pathname === '/api/projects' && r.request().method() === 'POST'),
    page.getByRole('button', { name: '登録' }).click(),
  ])
  expect(response.status()).toBe(422)
  const field = page.locator('[data-field="params"]')
  await expect(field.locator('[data-slot="field-error"]')).toHaveText('値が正しくありません')
  await expect(page.getByLabel('渡す値（JSON）')).toHaveAttribute('aria-invalid', 'true')
  expect((await page.request.get('/api/projects/bad-params')).status()).toBe(404)
})

test('ラベルの登録だけが失敗したとき、Project は登録されラベルが無いことが表示される', async ({ page }) => {
  await fillProject(page, { id: 'label-fail', label: 'repo:taken' })
  const [route] = await Promise.all([
    page.waitForResponse((r) => new URL(r.url()).pathname === '/api/routes' && r.request().method() === 'POST'),
    page.getByRole('button', { name: '登録' }).click(),
  ])
  expect(route.status()).toBe(409)
  const alert = page.getByTestId('label-missing')
  await expect(alert).toContainText('振り分けのラベルがありません')
  await expect(alert).toContainText('すでに登録されています')
  // 入力は残る。
  await expect(page.getByLabel('振り分けのラベル')).toHaveValue('repo:taken')
  await expect(page.getByLabel('Project ID')).toBeDisabled()
  expect((await getJSON<ProjectView>(page, '/api/projects/label-fail')).routingLabel).toBeNull()

  await page.goto('/projects')
  const row = page.getByRole('row').filter({ has: page.getByRole('link', { name: 'label-fail の名前' }) })
  await expect(row.getByTestId('routing-label')).toHaveText('ラベルなし')
})
