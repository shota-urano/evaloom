import { spawnSync } from 'node:child_process'
import { mkdirSync, mkdtempSync, realpathSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { expect, test as base, type Page, type Request } from '@playwright/test'

import { loginOwner, run, setupOwner, startEvaloomd } from './evaloomd.ts'

// evaloom-vhe.7: Project 編集の承認を省く設定と確認ダイアログ（17 3.2、Q165、13 3.5）。
// - 固定した版で waivable の操作だけが選べる。
// - 操作を足すと確認ダイアログが出て、確認しないと保存されない（PATCH が飛ばない）。
// - 外すときはダイアログが出ない。
//
// global-setup がビルドしたバイナリを一時 DB と空きポートで別に起動する。版と Project の行は DB へ直接入れ、
// 編集画面から本物の PATCH /api/projects/{id} を通す。page.route での差し替えはしない。

const NOW = '2026-11-20T00:00:00Z'
const RUNNER = 'local'
const PROFILE_V1 = '01JE2E0P0000000000000VHE71'
const PROFILE_V2 = '01JE2E0P0000000000000VHE72'
const SCREENSHOTS = fileURLToPath(new URL('../../docs/design/screenshots/', import.meta.url))

type Server = { origin: string; cookie: string }
type ProjectView = { id: string; profileId: string; approvalWaivers: string[] }

const sqlText = (value: unknown) => `'${(typeof value === 'string' ? value : JSON.stringify(value)).replaceAll("'", "''")}'`

function seed(db: string, repo: string) {
  const v1 = {
    description: 'E2E の版 1',
    approvals: [
      { operation: 'merge_pull_request', description: 'PR を main へマージする', waivable: true },
      { operation: 'deploy', description: '本番環境へ出す' },
      { operation: 'post_comment', description: 'Issue にコメントする', waivable: true },
    ],
  }
  const v2 = { description: 'E2E の版 2', approvals: [{ operation: 'deploy', description: '本番環境へ出す', waivable: true }] }
  const profile = (id: string, version: string, definition: unknown) =>
    `INSERT INTO harness_profiles (id, harness_id, version, commit_sha, tag_name, contract_version, definition, definition_hash, model_id, operation_status, registered_at)
      VALUES ('${id}', 'e2e-harness', '${version}', '${'a'.repeat(40)}', 'v${version}', '1', ${sqlText(definition)}, 'hash-${version}', 'model', 'provisional', '${NOW}');`
  const project = (id: string, profileId: string, waivers: string[]) =>
    `INSERT INTO projects (id, name, harness_id, profile_id, params, approval_waivers, runner_id, created_at, updated_at)
      VALUES ('${id}', '${id} の名前', 'e2e-harness', '${profileId}', '{}', ${sqlText(waivers)}, '${RUNNER}', '${NOW}', '${NOW}');`
  const statements = [
    `INSERT INTO harnesses (id, name, repo_url, repo_path, default_profile_id, approval_waivers, created_at, updated_at)
      VALUES ('e2e-harness', 'E2E ハーネス', 'https://example.com/h.git', ${sqlText(repo)}, NULL, '[]', '${NOW}', '${NOW}');`,
    profile(PROFILE_V1, '1.0.0', v1),
    profile(PROFILE_V2, '2.0.0', v2),
    project('pinned-v1', PROFILE_V1, []),
    project('pinned-v2', PROFILE_V2, []),
    project('add-target', PROFILE_V1, []),
    project('remove-target', PROFILE_V1, ['merge_pull_request', 'post_comment']),
  ]
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
      const temp = realpathSync(mkdtempSync(join(tmpdir(), 'evaloom-e2e-vhe7-')))
      const db = join(temp, 'evaloom.db')
      const repo = join(temp, 'harness')
      mkdirSync(repo, { recursive: true })
      const registered = spawnSync(binary, ['-db', db, 'runner', 'register', '-worktree-root', join(temp, 'worktrees'), '-name', 'ローカル Runner', RUNNER], { encoding: 'utf8' })
      if (registered.status !== 0) throw new Error(`runner register が失敗した: ${registered.stderr}`)
      setupOwner(binary, db)
      seed(db, repo)
      const evaloomd = await startEvaloomd(binary, db, '0')
      try {
        await provide({ origin: evaloomd.origin, cookie: await login(evaloomd.origin) })
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

async function getProject(page: Page, id: string): Promise<ProjectView> {
  const response = await page.request.get(`/api/projects/${id}`)
  expect(response.status()).toBe(200)
  return (await response.json()) as ProjectView
}

/** 画面が送った PATCH /api/projects/{id} を記録する（差し替えはしない）。 */
function recordPatches(page: Page, id: string): Request[] {
  const patches: Request[] = []
  page.on('request', (request) => {
    if (request.method() === 'PATCH' && new URL(request.url()).pathname === `/api/projects/${id}`) patches.push(request)
  })
  return patches
}

async function openEdit(page: Page, id: string) {
  await page.goto(`/projects/${id}/edit`)
  const group = page.getByRole('group', { name: '承認を省く設定' })
  await expect(group.getByRole('checkbox').first()).toBeVisible()
  return group
}

async function save(page: Page, id: string) {
  const [response] = await Promise.all([
    page.waitForResponse((r) => r.request().method() === 'PATCH' && new URL(r.url()).pathname === `/api/projects/${id}`),
    page.getByRole('button', { name: '保存' }).click(),
  ])
  expect(response.status()).toBe(200)
  await expect(page).toHaveURL(new RegExp(`/projects/${id}$`))
}

test('固定した版で waivable の操作だけが選べる', async ({ page }) => {
  // v1 を固定した Project: waivable の merge_pull_request と post_comment だけ。deploy（waivable でない）は並ばない。
  let group = await openEdit(page, 'pinned-v1')
  await expect(page.getByLabel('版')).toHaveValue(PROFILE_V1)
  await expect(group.getByRole('checkbox')).toHaveCount(2)
  await expect(group.getByRole('checkbox', { name: /merge_pull_request/ })).toBeVisible()
  await expect(group.getByRole('checkbox', { name: /post_comment/ })).toBeVisible()
  await expect(group.getByRole('checkbox', { name: /deploy/ })).toHaveCount(0)

  // v2 を固定した Project: v2 では deploy が waivable なので deploy だけが並ぶ。
  group = await openEdit(page, 'pinned-v2')
  await expect(page.getByLabel('版')).toHaveValue(PROFILE_V2)
  await expect(group.getByRole('checkbox')).toHaveCount(1)
  await expect(group.getByRole('checkbox', { name: /deploy/ })).toBeVisible()
})

test('操作を足すと確認ダイアログが出て、確認しないと保存されない', async ({ page }) => {
  const patches = recordPatches(page, 'add-target')
  const group = await openEdit(page, 'add-target')
  const merge = group.getByRole('checkbox', { name: /merge_pull_request/ })
  const dialog = page.getByRole('alertdialog', { name: '承認を省く操作を足しますか' })

  // ダイアログは操作の名前と Profile の description と、本人の承認なしに行うことを示す（Q165）。
  await merge.click()
  await expect(dialog).toBeVisible()
  await expect(dialog).toContainText('merge_pull_request')
  await expect(dialog).toContainText('PR を main へマージする')
  await expect(dialog).toContainText('この Project では、この操作を本人の承認なしにスペシャルハーネスが行います。')
  mkdirSync(SCREENSHOTS, { recursive: true })
  await page.screenshot({ path: join(SCREENSHOTS, 'evaloom-vhe.7-project-waiver-confirm.png'), fullPage: true })
  // ダイアログを出しただけでは保存されない。
  expect(patches).toHaveLength(0)

  // やめると足されず、PATCH も飛ばない。Escape でも同じ。
  await dialog.getByRole('button', { name: 'やめる' }).click()
  await expect(dialog).toHaveCount(0)
  await expect(merge).not.toBeChecked()
  await merge.click()
  await expect(dialog).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(dialog).toHaveCount(0)
  await expect(merge).not.toBeChecked()
  expect(patches).toHaveLength(0)
  expect((await getProject(page, 'add-target')).approvalWaivers).toEqual([])

  // 確認しないまま保存しても、足そうとした操作は送られない。
  await save(page, 'add-target')
  expect(patches).toHaveLength(1)
  expect(patches[0].postDataJSON()).toMatchObject({ approvalWaivers: [] })
  expect((await getProject(page, 'add-target')).approvalWaivers).toEqual([])

  // 確認すると足され、保存したときに送られる。確認しただけでは PATCH は飛ばない。
  const again = (await openEdit(page, 'add-target')).getByRole('checkbox', { name: /merge_pull_request/ })
  await again.click()
  await dialog.getByRole('button', { name: '承認を省く' }).click()
  await expect(dialog).toHaveCount(0)
  await expect(again).toBeChecked()
  expect(patches).toHaveLength(1)
  await save(page, 'add-target')
  expect(patches).toHaveLength(2)
  expect(patches[1].postDataJSON()).toMatchObject({ approvalWaivers: ['merge_pull_request'] })
  expect((await getProject(page, 'add-target')).approvalWaivers).toEqual(['merge_pull_request'])
})

test('外すときはダイアログが出ない', async ({ page }) => {
  const patches = recordPatches(page, 'remove-target')
  const group = await openEdit(page, 'remove-target')
  const merge = group.getByRole('checkbox', { name: /merge_pull_request/ })
  const comment = group.getByRole('checkbox', { name: /post_comment/ })
  await expect(merge).toBeChecked()
  await expect(comment).toBeChecked()

  await merge.click()
  await expect(merge).not.toBeChecked()
  await expect(page.getByRole('alertdialog')).toHaveCount(0)
  await comment.click()
  await expect(comment).not.toBeChecked()
  await expect(page.getByRole('alertdialog')).toHaveCount(0)

  await save(page, 'remove-target')
  expect(patches).toHaveLength(1)
  expect(patches[0].postDataJSON()).toMatchObject({ approvalWaivers: [] })
  expect((await getProject(page, 'remove-target')).approvalWaivers).toEqual([])
})
