import { mkdirSync, mkdtempSync, realpathSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { expect, test as base, type Page } from '@playwright/test'

import { run, startEvaloomd } from './evaloomd.ts'
import { createRepo, tagProfile, type Repo } from '../../scripts/fixtures.mts'

// evaloom-52y.3: Harnesses 一覧・登録・編集・詳細（Profile の版の登録と一覧）。
//
// ログイン URL は一回限りなので、evaloom-52y.2 と同じく、global-setup がビルドしたバイナリを空きポートで別に起動する。
// - Profile の版は画面から実 API（POST /api/harness-profiles）で登録する。そのため Harness リポジトリを本物の
//   Git リポジトリとして作り、evaloom/profile.yaml を commit して profile/v<版> のタグを付ける（03 3.5、
//   backend/internal/api/harness_profiles.md）。版の登録は Adapter の登録表を使わない（Harness の adapter 列と
//   定義の executionConfiguration.adapter の一致だけを見る）。
// - Harness 自体の登録・更新は Adapter の登録表で検証され、evaloomd は具体的な Adapter をまだ登録していない
//   （backend/internal/api/harnesses.md）。そのため Harness の行だけは SQLite に直接入れる。
//   画面からの Harness 登録は、Adapter が未登録であることが項目のエラーとして出ることを確かめる。

const SCREENSHOTS = fileURLToPath(new URL('../../docs/design/screenshots/', import.meta.url))
const HARNESS = 'claude-developer'
const HARNESS_NAME = 'Claude Developer'

type Server = { origin: string; cookie: string; harness: Repo; project: Repo }

function seedHarnesses(db: string, harness: Repo, other: string) {
  const now = new Date().toISOString()
  const row = (id: string, name: string, repo: Repo) =>
    `INSERT INTO harnesses (id, name, specialty, supported_tasks, adapter, repo_path, repo_url, supported_models,
      available_tools, tool_permissions, contract_version, created_at, updated_at)
      VALUES ('${id}', '${name}', 'developer', '["development"]', 'claude_code', '${repo.path}', '${repo.url}',
      '["claude-e2e-model"]', '["read","write"]', '{}', '1', '${now}', '${now}');`
  // Harness ID「new」は slug として正当。登録画面（/harnesses/_new）と衝突しないことの確認に使う。
  run('sqlite3', [db, [row(HARNESS, HARNESS_NAME, harness), row('new', 'New という名前の Harness', { path: other, url: other })].join('\n')])
}

const test = base.extend<{ server: Server }, { worker: Server }>({
  worker: [
    // Playwright は fixture の第1引数を分割代入で受け取る必要がある。
    // oxlint-disable-next-line no-empty-pattern
    async ({}, provide) => {
      const binary = process.env.EVALOOMD_BINARY
      if (!binary) throw new Error('EVALOOMD_BINARY が無い（e2e/global-setup.ts が設定する）')
      // macOS の tmpdir はシンボリックリンク経由なので、evaloomd が保存する実パスに揃える。
      const temp = realpathSync(mkdtempSync(join(tmpdir(), 'evaloom-e2e-52y3-')))
      const db = join(temp, 'evaloom.db')
      const harness = createRepo(temp, 'harness')
      const project = createRepo(temp, 'app')
      const other = createRepo(temp, 'other-harness')
      run(binary, ['-db', db, 'runner', 'register', '-worktree-root', join(temp, 'worktrees'), '-name', 'ローカル Runner', 'local'])
      seedHarnesses(db, harness, other.path)
      const evaloomd = await startEvaloomd(binary, db, '0')
      try {
        // 一回限りのログイン URL を使い、セッション Cookie を各テストのブラウザへ渡す。
        const response = await fetch(evaloomd.loginURL, { headers: { Accept: 'application/json' } })
        const cookie = /evaloom_session=([^;]+)/.exec(response.headers.getSetCookie().join('\n'))?.[1]
        if (!response.ok || !cookie) throw new Error(`ログインに失敗した: ${response.status}`)
        await provide({ origin: evaloomd.origin, cookie, harness, project })
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
  await page.screenshot({ path: join(SCREENSHOTS, `evaloom-52y.3-${screen}.png`), fullPage: true })
}

function field(page: Page, label: string) {
  return page.locator('[data-field]').filter({ has: page.getByLabel(label, { exact: true }) })
}

function profileRow(page: Page, version: string) {
  return page.getByRole('table', { name: '登録済みの Profile の版' }).locator('tbody tr').filter({ hasText: version })
}

async function openHarness(page: Page) {
  await page.goto('/')
  await page.getByRole('navigation', { name: '画面' }).getByRole('link', { name: 'Harnesses' }).click()
  await expect(page.getByRole('heading', { name: 'Harnesses' })).toBeVisible()
  await page.getByRole('link', { name: HARNESS_NAME }).click()
  await expect(page).toHaveURL(`/harnesses/${HARNESS}`)
  await expect(page.getByRole('heading', { name: HARNESS_NAME })).toBeVisible()
}

async function registerVersion(page: Page, version: string) {
  await page.getByLabel('版', { exact: true }).fill(version)
  await page.getByRole('button', { name: '版を登録' }).click()
}

/** 画面のセッションで本人用 API を呼ぶ（テストの前提づくり用）。evaloomd は Origin も照合するので同一オリジンを付ける。 */
async function ownerPost(page: Page, origin: string, path: string, body: unknown) {
  const session = await page.request.get('/api/session')
  const { csrfToken } = (await session.json()) as { csrfToken: string }
  const response = await page.request.post(path, { data: body, headers: { 'X-CSRF-Token': csrfToken, Origin: origin } })
  expect(response.ok(), `${path}: ${response.status()} ${await response.text()}`).toBe(true)
}

test('Profile の版を登録すると commit SHA と「暫定運用」が表示される', async ({ page, server }) => {
  const sha = tagProfile(server.harness.path, '1.0.0', 'first')
  await openHarness(page)
  await expect(page.getByText('登録済みの版はありません。')).toBeVisible()

  await registerVersion(page, '1.0.0')
  await expect(page.getByRole('status')).toHaveText(`版 1.0.0 を登録しました。commit SHA: ${sha}、状態: 暫定運用。`)
  const row = profileRow(page, '1.0.0')
  await expect(row).toHaveCount(1)
  await expect(row).toContainText(sha)
  await expect(row).toContainText('claude-e2e-model')
  await expect(row).toContainText('暫定運用')
  await expect(page.getByLabel('版', { exact: true })).toHaveValue('')

  // 同じタグ（同じ SHA）の再登録は行を増やさず、変更なしと表示する。
  await registerVersion(page, '1.0.0')
  await expect(page.getByRole('status')).toContainText(`版 1.0.0 は同じ commit SHA（${sha}）で登録済みです。変更はありません`)
  await expect(page.getByRole('table', { name: '登録済みの Profile の版' }).locator('tbody tr')).toHaveCount(1)

  // 登録した版をプロジェクトに割り当てると、使用中プロジェクトに出る（割り当て自体は evaloom-52y.2 の画面の範囲）。
  await ownerPost(page, server.origin, '/api/projects', {
    id: 'alpha-app', name: 'Alpha アプリ', localRepoPath: server.project.path, gitRemoteUrl: server.project.url, runnerId: 'local', baseBranch: 'main', defaultRole: 'developer',
  })
  await ownerPost(page, server.origin, '/api/projects/alpha-app/harnesses', { role: 'developer', harnessId: HARNESS, version: '1.0.0' })

  // 再読み込みしても、版と SHA はサーバーから読み直される。
  await page.reload()
  await expect(profileRow(page, '1.0.0')).toContainText(sha)
  await expect(profileRow(page, '1.0.0')).toContainText('暫定運用')
  const usages = page.getByRole('table', { name: 'この Harness を使っているプロジェクト' }).locator('tbody tr')
  await expect(usages).toHaveCount(1)
  await expect(usages.nth(0)).toContainText('Alpha アプリ')
  await expect(usages.nth(0)).toContainText('developer')
  await expect(usages.nth(0)).toContainText(sha.slice(0, 7))
  await screenshot(page, 'harness-detail')

  await page.getByRole('link', { name: '一覧へ戻る' }).click()
  const listRow = page.getByRole('table', { name: '登録済み Harness' }).locator('tbody tr').filter({ hasText: HARNESS_NAME })
  await expect(listRow).toContainText('claude_code')
  await expect(listRow).toContainText(server.harness.path)
  await expect(listRow).toContainText('Alpha アプリ')
  await screenshot(page, 'harnesses')
})

test('同じ版を別の commit SHA で登録しようとすると 409 の内容が表示され、登録済みの版は変わらない', async ({ page, server }) => {
  const registered = tagProfile(server.harness.path, '2.0.0', 'second')
  await openHarness(page)
  await registerVersion(page, '2.0.0')
  await expect(page.getByRole('status')).toContainText(registered)

  // 公開済みのタグを別の commit へ付け替える（03 3.5 で禁止されている操作）。
  const moved = tagProfile(server.harness.path, '2.0.0', 'second-moved')
  expect(moved).not.toBe(registered)

  await registerVersion(page, '2.0.0')
  const alert = page.getByTestId('api-error')
  await expect(alert).toContainText('状態が変わりました')
  await expect(alert).toContainText('版 2.0.0 は別の commit SHA ですでに登録されています')
  await expect(alert).toContainText('公開済みのタグ（profile/v2.0.0）は付け替えられない')
  await expect(alert).toContainText(`登録済みの commit SHA: ${registered}`)
  await expect(alert).toContainText('Profile version already registered with another commit SHA')
  await expect(page.getByRole('status')).toHaveCount(0)
  const row = profileRow(page, '2.0.0')
  await expect(row).toHaveCount(1)
  await expect(row).toContainText(registered)
  await expect(row).not.toContainText(moved)
  await screenshot(page, 'profile-conflict')

  await page.reload()
  await expect(profileRow(page, '2.0.0')).toContainText(registered)
})

test('SemVer でない版は送らず、タグが無い版は前提条件の失敗理由が表示される', async ({ page }) => {
  await openHarness(page)
  await registerVersion(page, 'v3')
  await expect(field(page, '版').locator('[data-slot=field-error]')).toHaveText('SemVer（例 1.2.0）で入力してください')

  await registerVersion(page, '9.9.9')
  const alert = page.getByTestId('api-error')
  await expect(alert).toContainText('前提条件が満たされていません')
  await expect(alert).toContainText('タグ: 見つかりません')
  await expect(page.getByRole('table', { name: '登録済みの Profile の版' })).not.toContainText('9.9.9')
})

test('登録画面 /harnesses/_new は Harness ID「new」の詳細と衝突せず、登録・編集のエラーは項目に出る', async ({ page, server }) => {
  await page.goto('/harnesses')
  await page.getByRole('link', { name: 'Harness を登録' }).click()
  await expect(page).toHaveURL('/harnesses/_new')
  await expect(page.getByRole('heading', { name: 'Harness の登録' })).toBeVisible()

  // 必須項目が空なら送信しない。
  await page.getByRole('button', { name: '登録' }).click()
  await expect(field(page, 'Harness ID').locator('[data-slot=field-error]')).toHaveText('入力してください')
  await expect(field(page, 'Adapter').locator('[data-slot=field-error]')).toHaveText('選択してください')

  // evaloomd は Adapter を登録していないので、サーバーは adapter を 400 で拒否する。その details が Adapter の項目に出る。
  await page.getByLabel('Harness ID').fill('codex-developer')
  await page.getByLabel('名称').fill('Codex Developer')
  await page.getByLabel('Adapter').selectOption('codex')
  await page.getByLabel('ハーネスリポジトリのパス').fill(server.project.path)
  await page.getByLabel('ハーネスリポジトリ URL').fill(server.project.url)
  await page.getByLabel('対応モデル').fill('gpt-e2e')
  await page.getByRole('button', { name: '登録' }).click()
  await expect(page.getByTestId('api-error')).toContainText('入力内容を確認してください')
  await expect(field(page, 'Adapter').locator('[data-slot=field-error]')).toHaveText('値が正しくありません')
  await expect(page.getByLabel('Adapter')).toHaveAttribute('aria-invalid', 'true')
  await screenshot(page, 'harness-form')

  // Harness ID「new」の詳細は登録画面ではなく詳細を開く。
  await page.goto('/harnesses')
  await page.getByRole('link', { name: 'New という名前の Harness' }).click()
  await expect(page).toHaveURL('/harnesses/new')
  await expect(page.getByRole('heading', { name: 'New という名前の Harness' })).toBeVisible()
  await expect(page.getByText('Profile の版', { exact: true })).toBeVisible()

  // 編集画面は登録済みの値を出し、ID は変えられない。
  await page.goto(`/harnesses/${HARNESS}`)
  await page.getByRole('link', { name: '編集' }).click()
  await expect(page.getByRole('heading', { name: 'Harness の編集' })).toBeVisible()
  await expect(page.getByLabel('Harness ID')).toBeDisabled()
  await expect(page.getByLabel('Harness ID')).toHaveValue(HARNESS)
  await expect(page.getByLabel('Adapter')).toHaveValue('claude_code')
  await expect(page.getByLabel('ハーネスリポジトリのパス')).toHaveValue(server.harness.path)
  await expect(page.getByLabel('対応モデル')).toHaveValue('claude-e2e-model')
  await page.getByRole('link', { name: 'キャンセル' }).click()
  await expect(page).toHaveURL(`/harnesses/${HARNESS}`)
})
