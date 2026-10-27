import { mkdirSync, mkdtempSync, realpathSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { expect, test as base, type Page } from '@playwright/test'

import { run, startEvaloomd } from './evaloomd.ts'

// evaloom-52y.2: Projects 一覧・登録・編集・詳細（Profile 割り当てを含む）。
//
// ログイン URL は一回限りで、E2E_PORT の evaloomd のものは evaloom-52y.1 のテストが使う。
// そのためこのファイルは、同じバイナリ（global-setup がビルド）を空きポートで別に起動し、専用のデータを用意する。
// - Project のパス検証（03 3.2）を本物で通すため、bare リポジトリを origin にした Git リポジトリを作る。
// - Runner は evaloomd の runner register で登録する。
// - Harness と Profile の版は API での登録に Adapter と Harness リポジトリのタグが要り（evaloom-52y.3 の範囲）、
//   evaloomd は Adapter を登録していないので、SQLite に直接入れる。

const SCREENSHOTS = fileURLToPath(new URL('../../docs/design/screenshots/', import.meta.url))
const HARNESS = 'claude-developer'
const PROFILES = [
  { version: '1.0.0', sha: '1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d' },
  { version: '1.1.0', sha: '9f8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c' },
]

type Repo = { path: string; url: string }
type Server = { origin: string; cookie: string; repos: Record<string, Repo>; notGit: string }

function git(cwd: string, ...args: string[]) {
  run('git', ['-c', 'user.name=e2e', '-c', 'user.email=e2e@example.invalid', '-c', 'commit.gpgsign=false', ...args], { cwd })
}

/** origin（bare リポジトリ）に main を push 済みの Git リポジトリを作る。 */
function createRepo(root: string, name: string): Repo {
  const url = join(root, 'remotes', `${name}.git`)
  const path = join(root, 'repos', name)
  mkdirSync(url, { recursive: true })
  mkdirSync(path, { recursive: true })
  git(url, 'init', '--quiet', '--bare', '-b', 'main')
  git(path, 'init', '--quiet', '-b', 'main')
  git(path, 'commit', '--quiet', '--allow-empty', '-m', 'init')
  git(path, 'remote', 'add', 'origin', url)
  git(path, 'push', '--quiet', 'origin', 'main')
  return { path, url }
}

function seedHarness(db: string) {
  const now = new Date().toISOString()
  const rows = [
    `INSERT INTO harnesses (id, name, specialty, supported_tasks, adapter, repo_path, repo_url, supported_models,
      available_tools, tool_permissions, contract_version, created_at, updated_at)
      VALUES ('${HARNESS}', 'Claude Developer', 'general', '[]', 'claude_code', '/nonexistent/harness',
      'git@example.invalid:harness.git', '[]', '[]', '{}', '1', '${now}', '${now}');`,
    ...PROFILES.map(
      (p, i) => `INSERT INTO harness_profiles (id, harness_id, version, commit_sha, tag_name, definition, definition_hash,
        model_id, operation_status, registered_at) VALUES ('01JE2E0000000000000000000${i}', '${HARNESS}', '${p.version}',
        '${p.sha}', 'profile/v${p.version}', '{}', 'sha256:e2e', 'claude-e2e', 'provisional', '${now}');`,
    ),
  ]
  run('sqlite3', [db, rows.join('\n')])
}

const test = base.extend<{ server: Server }, { worker: Server }>({
  worker: [
    // Playwright は fixture の第1引数を分割代入で受け取る必要がある。
    // oxlint-disable-next-line no-empty-pattern
    async ({}, provide) => {
      const binary = process.env.EVALOOMD_BINARY
      if (!binary) throw new Error('EVALOOMD_BINARY が無い（e2e/global-setup.ts が設定する）')
      // macOS の tmpdir はシンボリックリンク経由なので、evaloomd が保存する実パスに揃える。
      const temp = realpathSync(mkdtempSync(join(tmpdir(), 'evaloom-e2e-52y2-')))
      const db = join(temp, 'evaloom.db')
      const repos = Object.fromEntries(['alpha', 'beta', 'gamma', 'delta'].map((name) => [name, createRepo(temp, name)]))
      const notGit = join(temp, 'repos', 'not-a-repository')
      mkdirSync(notGit)
      run(binary, ['-db', db, 'runner', 'register', '-worktree-root', join(temp, 'worktrees'), '-name', 'ローカル Runner', 'local'])
      seedHarness(db)
      const evaloomd = await startEvaloomd(binary, db, '0')
      try {
        // 一回限りのログイン URL を使い、セッション Cookie を各テストのブラウザへ渡す。
        const response = await fetch(evaloomd.loginURL, { headers: { Accept: 'application/json' } })
        const cookie = /evaloom_session=([^;]+)/.exec(response.headers.getSetCookie().join('\n'))?.[1]
        if (!response.ok || !cookie) throw new Error(`ログインに失敗した: ${response.status}`)
        await provide({ origin: evaloomd.origin, cookie, repos, notGit })
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
  await page.screenshot({ path: join(SCREENSHOTS, `evaloom-52y.2-${screen}.png`), fullPage: true })
}

function field(page: Page, label: string) {
  return page.locator('[data-field]').filter({ has: page.getByLabel(label, { exact: true }) })
}

function fieldError(page: Page, label: string) {
  return field(page, label).locator('[data-slot=field-error]')
}

type ProjectValues = { id: string; name: string; repo: Repo }

async function fillProject(page: Page, { id, name, repo }: ProjectValues) {
  await page.getByLabel('Project ID').fill(id)
  await page.getByLabel('名称').fill(name)
  await page.getByLabel('ローカルリポジトリのパス').fill(repo.path)
  await page.getByLabel('Git リポジトリ URL').fill(repo.url)
  await page.getByLabel('Runner').selectOption('local')
}

async function registerProject(page: Page, values: ProjectValues) {
  await page.goto('/projects')
  await page.getByRole('link', { name: 'プロジェクトを登録' }).click()
  await expect(page.getByRole('heading', { name: 'プロジェクトの登録' })).toBeVisible()
  await fillProject(page, values)
  await page.getByRole('button', { name: '登録' }).click()
  await expect(page.getByRole('heading', { name: values.name })).toBeVisible()
  await expect(page).toHaveURL(`/projects/${values.id}`)
}

test('2つのプロジェクトを画面から登録でき、一覧に表示される', async ({ page, server }) => {
  await page.goto('/')
  await page.getByRole('navigation', { name: '画面' }).getByRole('link', { name: 'Projects' }).click()
  await expect(page.getByRole('heading', { name: 'Projects' })).toBeVisible()
  await expect(page.getByText('登録済みのプロジェクトはありません。')).toBeVisible()

  await page.getByRole('link', { name: 'プロジェクトを登録' }).click()
  await fillProject(page, { id: 'alpha-app', name: 'Alpha アプリ', repo: server.repos.alpha })
  await page.getByLabel('テストコマンド').fill('make verify')
  await page.getByLabel('コピー許可リスト').fill('config/test.json')
  await screenshot(page, 'project-form')
  await page.getByRole('button', { name: '登録' }).click()
  await expect(page.getByRole('heading', { name: 'Alpha アプリ' })).toBeVisible()
  const settings = page.locator('[data-slot=card]').filter({ has: page.getByText('設定', { exact: true }) })
  await expect(settings.getByText('make verify')).toBeVisible()
  await expect(settings.getByText('config/test.json')).toBeVisible()

  await registerProject(page, { id: 'beta-api', name: 'Beta API', repo: server.repos.beta })

  await page.getByRole('link', { name: '一覧へ戻る' }).click()
  const rows = page.getByRole('table', { name: '登録済みプロジェクト' }).locator('tbody tr')
  await expect(rows).toHaveCount(2)
  await expect(rows.nth(0)).toContainText('Alpha アプリ')
  await expect(rows.nth(0)).toContainText(server.repos.alpha.path)
  await expect(rows.nth(0)).toContainText('未割り当て')
  await expect(rows.nth(1)).toContainText('Beta API')
  await expect(rows.nth(1)).toContainText(server.repos.beta.path)

  // 再読み込みしても登録内容はサーバーから読み直される。
  await page.reload()
  await expect(rows).toHaveCount(2)
})

test('パス検証の失敗理由が表示され、登録されない', async ({ page, server }) => {
  await page.goto('/projects/_new')
  await fillProject(page, { id: 'broken', name: '検証に失敗する', repo: { path: server.notGit, url: server.repos.gamma.url } })
  await page.getByRole('button', { name: '登録' }).click()
  const alert = page.getByTestId('api-error')
  await expect(alert).toContainText('前提条件が満たされていません')
  await expect(alert).toContainText('ローカルリポジトリのパス: Git リポジトリではありません')
  await expect(field(page, 'ローカルリポジトリのパス').getByText('Git リポジトリではありません')).toBeVisible()
  await expect(page.getByLabel('ローカルリポジトリのパス')).toHaveAttribute('aria-invalid', 'true')
  await screenshot(page, 'path-error')

  // origin と違う URL は、Git リポジトリ URL の項目に理由が出る。前の理由は消える。
  await page.getByLabel('ローカルリポジトリのパス').fill(server.repos.gamma.path)
  await page.getByLabel('Git リポジトリ URL').fill(server.repos.delta.url)
  await page.getByRole('button', { name: '登録' }).click()
  await expect(alert).toContainText('Git リポジトリ URL: Git URL がリポジトリの remote と一致しません')
  await expect(field(page, 'Git リポジトリ URL').getByText('Git URL がリポジトリの remote と一致しません')).toBeVisible()
  await expect(page.getByLabel('ローカルリポジトリのパス')).not.toHaveAttribute('aria-invalid')

  // remote に無い Base Branch。
  await page.getByLabel('Git リポジトリ URL').fill(server.repos.gamma.url)
  await page.getByLabel('Base Branch').fill('no-such-branch')
  await page.getByRole('button', { name: '登録' }).click()
  await expect(fieldError(page, 'Base Branch')).toHaveText('見つかりません')

  await page.goto('/projects')
  await expect(page.getByRole('table', { name: '登録済みプロジェクト' })).not.toContainText('検証に失敗する')
})

test('役割に Harness と版を割り当てられ、差し替えと別の役割の追加もできる', async ({ page, server }) => {
  // Project ID「new」は slug として正当なので、登録画面（/projects/_new）と衝突せず詳細を開けること。
  await registerProject(page, { id: 'new', name: 'Gamma Web', repo: server.repos.gamma })
  const card = page.locator('[data-slot=card]').filter({ hasText: 'Harness Profile の割り当て' })
  await expect(card.getByText('まだ割り当てがありません。')).toBeVisible()

  // 役割を空にし、Harness と版を選ばずに送ると、各項目にエラーが出て送信しない。
  await expect(card.getByLabel('役割', { exact: true })).toHaveValue('developer')
  await card.getByLabel('役割', { exact: true }).fill('')
  await card.getByRole('button', { name: '割り当てる' }).click()
  await expect(fieldError(page, '役割')).toHaveText('入力してください')
  await expect(fieldError(page, 'Harness')).toHaveText('選択してください')
  await expect(fieldError(page, '版')).toHaveText('選択してください')
  await expect(card.getByText('まだ割り当てがありません。')).toBeVisible()
  await card.getByLabel('役割', { exact: true }).fill('developer')

  await card.getByLabel('Harness', { exact: true }).selectOption(HARNESS)
  const version = card.getByLabel('版', { exact: true })
  await expect(version.locator('option')).toHaveText(['選択してください', '1.0.0（1a2b3c4・暫定運用）', '1.1.0（9f8e7d6・暫定運用）'])
  await version.selectOption('1.0.0')
  await card.getByRole('button', { name: '割り当てる' }).click()
  await expect(card.getByRole('status')).toHaveText(`developer に ${HARNESS} の版 1.0.0 を割り当てました。`)
  const rows = card.getByRole('table', { name: '割り当て中の Harness Profile' }).locator('tbody tr')
  await expect(rows).toHaveCount(1)
  await expect(rows.nth(0)).toContainText('developer')
  await expect(rows.nth(0)).toContainText(HARNESS)
  await expect(rows.nth(0)).toContainText('1.0.0')
  await expect(rows.nth(0)).toContainText('1a2b3c4')

  // 同じ役割へ別の版を割り当てると差し替わる。
  await card.getByLabel('Harness', { exact: true }).selectOption(HARNESS)
  await card.getByLabel('版', { exact: true }).selectOption('1.1.0')
  await card.getByRole('button', { name: '割り当てる' }).click()
  await expect(rows).toHaveCount(1)
  await expect(rows.nth(0)).toContainText('1.1.0')
  await expect(rows.nth(0)).toContainText('9f8e7d6')

  // 別の役割には別の版を固定できる。
  await card.getByLabel('役割', { exact: true }).fill('reviewer')
  await card.getByLabel('Harness', { exact: true }).selectOption(HARNESS)
  await card.getByLabel('版', { exact: true }).selectOption('1.0.0')
  await card.getByRole('button', { name: '割り当てる' }).click()
  await expect(rows).toHaveCount(2)
  await expect(rows.nth(1)).toContainText('reviewer')
  await expect(rows.nth(1)).toContainText('1.0.0')

  // 再読み込みしても割り当てはサーバーから読み直される。
  await page.reload()
  await expect(rows).toHaveCount(2)
  await expect(rows.nth(0)).toContainText('1.1.0')
  await screenshot(page, 'project-detail')

  // 一覧にも割り当て中の Harness と版が出る。
  await page.getByRole('link', { name: '一覧へ戻る' }).click()
  const row = page.getByRole('table', { name: '登録済みプロジェクト' }).locator('tbody tr').filter({ hasText: 'Gamma Web' })
  await expect(row).toContainText(`developer: ${HARNESS} v1.1.0`)
  await expect(row).toContainText(`reviewer: ${HARNESS} v1.0.0`)
  await row.getByRole('link', { name: 'Gamma Web' }).click()
  await expect(page).toHaveURL('/projects/new')
  await expect(page.getByRole('heading', { name: 'Gamma Web' })).toBeVisible()
  await page.getByRole('link', { name: '一覧へ戻る' }).click()
  await screenshot(page, 'projects')
})

test('登録済みのプロジェクトを編集でき、ID は変えられない', async ({ page, server }) => {
  await registerProject(page, { id: 'delta-cli', name: 'Delta CLI', repo: server.repos.delta })
  await page.getByRole('link', { name: '編集' }).click()
  await expect(page.getByRole('heading', { name: 'プロジェクトの編集' })).toBeVisible()
  await expect(page.getByLabel('Project ID')).toBeDisabled()
  await expect(page.getByLabel('Project ID')).toHaveValue('delta-cli')
  await expect(page.getByLabel('ローカルリポジトリのパス')).toHaveValue(server.repos.delta.path)
  await page.getByLabel('名称').fill('Delta CLI（改名）')
  await page.getByLabel('テストコマンド').fill('go test ./...')
  await page.getByLabel('テストのタイムアウト（秒）').fill('600')
  await page.getByRole('button', { name: '保存' }).click()
  await expect(page.getByRole('heading', { name: 'Delta CLI（改名）' })).toBeVisible()
  const settings = page.locator('[data-slot=card]').filter({ has: page.getByText('設定', { exact: true }) })
  await expect(settings.getByText('go test ./...')).toBeVisible()
  await expect(settings.locator('div').filter({ hasText: /^テストのタイムアウト（秒）600$/ })).toBeVisible()
})
