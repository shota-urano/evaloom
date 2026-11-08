import { spawnSync } from 'node:child_process'
import { mkdirSync, mkdtempSync, realpathSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { expect, test as base, type Page } from '@playwright/test'

import { run, startEvaloomd, type Evaloomd } from './evaloomd.ts'
import { createRepo } from '../../scripts/fixtures.mts'

// evaloom-iao.10: Project 登録・編集の Linear Team ID と実行先ラベル、Settings のトリガーのラベル名と接頭辞、
// Dashboard の Linear の取り込みの状況（15-linear-intake 3.8・5章・6章）。
//
// Linear の実 API には接続しない。evaloomd は HOME を空の一時ディレクトリにして起動するので、
// API キーのファイル（~/.config/evaloom/linear-api-key）が無く、Linear Poller は照会を始めずに
// 対象の Team の last_error を auth にする（15 6章）。照会が続けて失敗している状況は linear_poll_teams に直接用意する。

const SCREENSHOTS = fileURLToPath(new URL('../../docs/design/screenshots/', import.meta.url))
const TIME_ZONE = 'Asia/Tokyo'

type Server = { db: string; temp: string; origin: string; cookie: string }

async function login(loginURL: string): Promise<string> {
  const response = await fetch(loginURL, { headers: { Accept: 'application/json' } })
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

/** Git リポジトリを用意して Project を API で登録する。 */
async function registerProject(server: Server, id: string, name: string, linear: { linearTeamId?: string; linearRoutingLabel?: string } = {}) {
  const repo = createRepo(server.temp, id)
  await owner(server, 'POST', '/api/projects', {
    id, name, localRepoPath: repo.path, gitRemoteUrl: repo.url, runnerId: 'local', baseBranch: 'main', defaultRole: 'developer', ...linear,
  })
  return repo
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

const test = base.extend<{ server: Server }, { worker: Server }>({
  worker: [
    // oxlint-disable-next-line no-empty-pattern
    async ({}, provide) => {
      const binary = process.env.EVALOOMD_BINARY
      if (!binary) throw new Error('EVALOOMD_BINARY が無い（e2e/global-setup.ts が設定する）')
      const temp = realpathSync(mkdtempSync(join(tmpdir(), 'evaloom-e2e-iao10-')))
      const db = join(temp, 'evaloom.db')
      const home = join(temp, 'home')
      mkdirSync(home)
      const registered = spawnSync(binary, ['-db', db, 'runner', 'register', '-worktree-root', join(temp, 'worktrees'), '-name', 'ローカル Runner', 'local'], { encoding: 'utf8' })
      if (registered.status !== 0) throw new Error(`runner register が失敗した: ${registered.stderr}`)
      const evaloomd = await startWithoutLinearKey(binary, db, home)
      try {
        await provide({ db, temp, origin: evaloomd.origin, cookie: await login(evaloomd.loginURL) })
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

test.use({ timezoneId: TIME_ZONE })

const field = (page: Page, label: string) => page.locator('[data-field]').filter({ has: page.getByLabel(label, { exact: true }) })
const detailRow = (page: Page, label: string) => page.locator('dl > div').filter({ has: page.getByText(label, { exact: true }) }).locator('dd')

async function fillRepository(page: Page, id: string, name: string, repo: { path: string; url: string }) {
  await page.getByLabel('Project ID').fill(id)
  await page.getByLabel('名称').fill(name)
  await page.getByLabel('ローカルリポジトリのパス').fill(repo.path)
  await page.getByLabel('Git リポジトリ URL').fill(repo.url)
  await page.getByLabel('Runner').selectOption('local')
}

test('Project の登録で Linear Team ID と実行先ラベルを保存し、詳細と編集画面に表示する', async ({ page, server }) => {
  const repo = createRepo(server.temp, 'linear-alpha')
  await page.goto('/projects')
  await page.getByRole('link', { name: 'プロジェクトを登録' }).click()
  await fillRepository(page, 'linear-alpha', 'Linear Alpha', repo)
  await page.getByLabel('Linear Team ID').fill('TEAM-ENG')
  await page.getByLabel('実行先ラベル').fill('repo:linear-alpha')
  await page.getByRole('button', { name: '登録' }).click()

  await expect(page).toHaveURL('/projects/linear-alpha')
  await expect(detailRow(page, 'Linear Team ID')).toHaveText('TEAM-ENG')
  await expect(detailRow(page, '実行先ラベル')).toHaveText('repo:linear-alpha')
  // API にも保存されている。
  const saved = (await owner(server, 'GET', '/api/projects/linear-alpha')) as { linearTeamId: string; linearRoutingLabel: string }
  expect(saved).toMatchObject({ linearTeamId: 'TEAM-ENG', linearRoutingLabel: 'repo:linear-alpha' })
  mkdirSync(SCREENSHOTS, { recursive: true })
  await page.screenshot({ path: join(SCREENSHOTS, 'evaloom-iao.10-project-detail.png'), fullPage: true })

  // 編集画面では登録済みの値が入っている。実行先ラベルを空欄にして保存すると解除される。
  await page.getByRole('link', { name: '編集' }).click()
  await expect(page.getByLabel('Linear Team ID')).toHaveValue('TEAM-ENG')
  await expect(page.getByLabel('実行先ラベル')).toHaveValue('repo:linear-alpha')
  await page.getByLabel('実行先ラベル').fill('')
  await page.getByRole('button', { name: '保存' }).click()
  await expect(page).toHaveURL('/projects/linear-alpha')
  await expect(detailRow(page, 'Linear Team ID')).toHaveText('TEAM-ENG')
  await expect(detailRow(page, '実行先ラベル')).toHaveText('未設定')
  expect(await owner(server, 'GET', '/api/projects/linear-alpha')).toMatchObject({ linearTeamId: 'TEAM-ENG', linearRoutingLabel: null })
})

test('同じ Linear Team ID と実行先ラベルの組を別の Project に登録すると、409 の内容がフォームに出る', async ({ page, server }) => {
  await registerProject(server, 'linear-owner', 'Linear Owner', { linearTeamId: 'TEAM-DUP', linearRoutingLabel: 'repo:shared' })
  const repo = createRepo(server.temp, 'linear-second')

  await page.goto('/projects/_new')
  await fillRepository(page, 'linear-second', 'Linear Second', repo)
  await page.getByLabel('Linear Team ID').fill('TEAM-DUP')
  await page.getByLabel('実行先ラベル').fill('repo:shared')
  await page.getByRole('button', { name: '登録' }).click()

  const message = 'ほかのプロジェクトに同じ Linear Team ID と実行先ラベルの組が登録されています'
  const alert = page.getByTestId('api-error')
  await expect(alert).toContainText('Linear の実行先が重複しています')
  await expect(alert).toContainText(`実行先ラベル: ${message}`)
  await expect(field(page, '実行先ラベル')).toContainText(message)
  await expect(page.getByLabel('実行先ラベル')).toHaveAttribute('aria-invalid', 'true')
  await expect(page.getByRole('button', { name: '最新を読み直す' })).toHaveCount(0)
  await expect(page).toHaveURL('/projects/_new')
  mkdirSync(SCREENSHOTS, { recursive: true })
  await page.screenshot({ path: join(SCREENSHOTS, 'evaloom-iao.10-project-form.png'), fullPage: true })

  // ラベルを変えれば登録できる。
  await page.getByLabel('実行先ラベル').fill('repo:second')
  await page.getByRole('button', { name: '登録' }).click()
  await expect(page).toHaveURL('/projects/linear-second')
  await expect(detailRow(page, '実行先ラベル')).toHaveText('repo:second')

  // 編集で既存の組に変えようとしても同じく 409 の内容が出て、保存されない。
  await page.getByRole('link', { name: '編集' }).click()
  await page.getByLabel('実行先ラベル').fill('repo:shared')
  await page.getByRole('button', { name: '保存' }).click()
  await expect(field(page, '実行先ラベル')).toContainText(message)
  expect(await owner(server, 'GET', '/api/projects/linear-second')).toMatchObject({ linearRoutingLabel: 'repo:second' })
})

test('Settings でトリガーのラベル名と実行先ラベルの接頭辞を変えて保存できる', async ({ page, server }) => {
  await page.goto('/')
  await page.getByRole('navigation', { name: '画面' }).getByRole('link', { name: 'Settings' }).click()
  await expect(page).toHaveURL('/settings')
  const card = page.getByTestId('settings-linear')
  await expect(card.getByLabel('トリガーのラベル名')).toHaveValue('ai-ready')
  await expect(card.getByLabel('実行先ラベルの接頭辞')).toHaveValue('repo:')

  // 空欄では送らない。
  await card.getByLabel('トリガーのラベル名').fill('')
  await card.getByRole('button', { name: '保存' }).click()
  await expect(field(page, 'トリガーのラベル名')).toContainText('入力してください')

  await card.getByLabel('トリガーのラベル名').fill('evaloom-ready')
  await card.getByLabel('実行先ラベルの接頭辞').fill('project:')
  await card.getByRole('button', { name: '保存' }).click()
  await expect(card.getByRole('status')).toContainText('保存しました')
  expect(await owner(server, 'GET', '/api/settings')).toEqual({ linear: { triggerLabel: 'evaloom-ready', routingLabelPrefix: 'project:' } })

  // 読み直しても保存した値が出る。
  await page.reload()
  await expect(card.getByLabel('トリガーのラベル名')).toHaveValue('evaloom-ready')
  await expect(card.getByLabel('実行先ラベルの接頭辞')).toHaveValue('project:')
  mkdirSync(SCREENSHOTS, { recursive: true })
  await page.screenshot({ path: join(SCREENSHOTS, 'evaloom-iao.10-settings.png'), fullPage: true })

  // ほかのテストに影響しないよう既定値に戻す。
  await owner(server, 'PATCH', '/api/settings', { linear: { triggerLabel: 'ai-ready', routingLabelPrefix: 'repo:' } })
})

test('照会が続けて失敗している Team は、Dashboard のエラー状況に失敗の分類と最後に成功した時刻が出る', async ({ page, server }) => {
  await registerProject(server, 'ops-app', 'Ops アプリ', { linearTeamId: 'TEAM-OPS', linearRoutingLabel: 'repo:ops-app' })
  await registerProject(server, 'web-app', 'Web アプリ', { linearTeamId: 'TEAM-WEB', linearRoutingLabel: 'repo:web-app' })

  // TEAM-OPS は 5 回続けて失敗（15 6章の上限）、TEAM-WEB は直近に成功。
  // API キーのファイルが無いので、Poller が照会の回を迎えても last_error は auth のまま、回数が増えるだけで、
  // 最後に成功した時刻は変わらない（store.RecordLinearPollFailure）。
  const lastSuccess = '2026-11-08T00:30:00Z'
  const now = new Date().toISOString()
  run('sqlite3', [
    server.db,
    `INSERT INTO linear_poll_teams (team_id, baseline_done_at, last_success_at, last_error, consecutive_failures)
       VALUES ('TEAM-OPS', '2026-11-07T00:00:00Z', '${lastSuccess}', 'auth', 5),
              ('TEAM-WEB', '2026-11-07T00:00:00Z', '${now}', NULL, 0)
       ON CONFLICT (team_id) DO UPDATE SET baseline_done_at = excluded.baseline_done_at, last_success_at = excluded.last_success_at,
         last_error = excluded.last_error, consecutive_failures = excluded.consecutive_failures;
     INSERT INTO linear_issue_states (linear_issue_id, issue_identifier, team_id, ai_ready, attach_seq, issue_updated_at,
       last_decision, last_decision_at, first_seen_at, last_seen_at)
       VALUES ('issue-web-1', 'WEB-12', 'TEAM-WEB', 1, 0, '${now}', 'skipped_initial', '${now}', '${now}', '${now}');`,
  ])

  await page.goto('/projects')
  await page.getByRole('navigation', { name: '画面' }).getByRole('link', { name: 'Dashboard' }).click()
  await expect(page).toHaveURL('/')

  const failures = page.getByRole('list', { name: 'Linear の照会の失敗' })
  const ops = failures.getByRole('listitem').filter({ hasText: 'TEAM-OPS' })
  await expect(ops).toHaveCount(1)
  await expect(ops).toContainText('認証エラー（auth）')
  await expect(ops).toContainText(/\d+ 回続けて失敗/)
  const expectedTime = new Date(lastSuccess).toLocaleString('ja-JP', { timeZone: TIME_ZONE })
  await expect(ops).toContainText(`最後に成功した時刻: ${expectedTime}`)
  await expect(ops.locator('time')).toHaveAttribute('datetime', /^2026-11-08T00:30:00/)
  await expect(ops).toContainText('linear-api-key')
  await expect(failures).not.toContainText('TEAM-WEB')

  // 取り込みの状況には、照会の対象の Team と、トリガーのラベルが付いている Issue の最後の判定が出る。
  const teams = page.getByRole('table', { name: 'Linear Team の照会の状況' })
  await expect(teams.locator('tr[data-team="TEAM-OPS"]')).toContainText('認証エラー（auth）')
  await expect(teams.locator('tr[data-team="TEAM-WEB"]')).toBeVisible()
  const issues = page.getByRole('table', { name: 'トリガーのラベルが付いている Issue' })
  await expect(issues).toContainText('WEB-12')
  await expect(issues).toContainText('最初の照会のため記録のみ（skipped_initial）')

  // 応答に API キーの値は含まれない（キーのファイル自体が無い）。分類と時刻だけが返る。
  const status = (await owner(server, 'GET', '/api/linear/status')) as { teams: { teamId: string; lastError: string; lastSuccessAt: string }[] }
  expect(status.teams.find((team) => team.teamId === 'TEAM-OPS')).toMatchObject({ lastError: 'auth' })

  mkdirSync(SCREENSHOTS, { recursive: true })
  await page.screenshot({ path: join(SCREENSHOTS, 'evaloom-iao.10-dashboard.png'), fullPage: true })
})
