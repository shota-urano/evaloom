import { mkdirSync, mkdtempSync, realpathSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { expect, test as base, type Page } from '@playwright/test'

import { loginOwner, run, setupOwner, startEvaloomd } from './evaloomd.ts'
import { createRepo, git, type Repo } from '../../scripts/fixtures.mts'

// evaloom-oxe.6: スペシャルハーネスの一覧・登録・編集・詳細（13 3.7）。
// - スペシャルハーネスを画面から登録し、版を画面から登録する（本物の API。タグ profile/v<版> を commit SHA に解決する）。
// - 同じ版を別の commit に付け直して登録すると 409 の内容が出る。
// - 版の詳細に段ごとの上限時間・許可宛先・鍵の名前、承認が要る操作と waivable が出て、鍵の値の欄は無い。
// - 既定の版を詳細画面から設定でき、編集画面でも選べる。
//
// global-setup がビルドしたバイナリを一時 DB と空きポートで別に起動する。ハーネスリポジトリは一時ディレクトリに
// 本物の Git リポジトリとして作る。追跡ファイルはスクリーンショットだけを書く。

const SCREENSHOTS = fileURLToPath(new URL('../../docs/design/screenshots/', import.meta.url))
const NOW = '2026-11-20T00:00:00Z'
const HARNESS = 'engineer'
const HARNESS_NAME = 'エンジニア'
const PREFIX = 'repo:'

type Server = { origin: string; cookie: string; harness: Repo }

/** 契約 v1 の Profile 定義（契約 5章）と、定義が参照するファイルを commit し、profile/v<版> を付ける。 */
function tagProfile(repo: string, version: string, note: string): string {
  const files: Record<string, string> = {
    'evaloom/work/AGENTS.md': `E2E instructions (${note})\n`,
    'evaloom/check/run': '#!/bin/sh\n',
    'evaloom/publish/run': '#!/bin/sh\n',
    'evaloom/profile.yaml': `contractVersion: "1"
profileVersion: "${version}"
name: "e2e-engineer"
description: "PR を作るハーネス (${note})"
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
    timeoutSec: 120
    network:
      allow: ["api.github.com:443"]
    secrets:
      - slot: "github_token"
        access: "write"
approvals:
  - operation: "merge_pull_request"
    description: "PR をマージする"
    waivable: true
  - operation: "delete_branch"
    description: "ブランチを消す"
records:
  keep: ["check/report.md"]
  pass:
    recent: 3
`,
  }
  for (const [name, content] of Object.entries(files)) {
    mkdirSync(join(repo, dirname(name)), { recursive: true })
    writeFileSync(join(repo, name), content)
  }
  git(repo, 'add', 'evaloom')
  git(repo, 'commit', '--quiet', '-m', `profile ${version} (${note})`)
  git(repo, 'tag', '-f', `profile/v${version}`)
  return git(repo, 'rev-parse', `refs/tags/profile/v${version}^{commit}`)
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
      // macOS の tmpdir はシンボリックリンク経由なので、evaloomd が保存する実パスに揃える。
      const temp = realpathSync(mkdtempSync(join(tmpdir(), 'evaloom-e2e-oxe6-')))
      const db = join(temp, 'evaloom.db')
      const harness = createRepo(temp, 'harness')
      setupOwner(binary, db)
      run('sqlite3', [db, `INSERT INTO settings (key, value, updated_at) VALUES ('linear.routingLabelPrefix', '"${PREFIX}"', '${NOW}');`])
      const evaloomd = await startEvaloomd(binary, db, '0')
      try {
        await provide({ origin: evaloomd.origin, cookie: await login(evaloomd.origin), harness })
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
  await page.screenshot({ path: join(SCREENSHOTS, `evaloom-oxe.6-${screen}.png`), fullPage: true })
}

/** 画面のセッションで本人用 API を呼ぶ（テストの前提づくり用）。 */
async function ownerPost(page: Page, origin: string, path: string, body: unknown) {
  const session = await page.request.get('/api/session')
  const { csrfToken } = (await session.json()) as { csrfToken: string }
  const response = await page.request.post(path, { data: body, headers: { 'X-CSRF-Token': csrfToken, Origin: origin } })
  expect(response.ok(), `${path}: ${response.status()} ${await response.text()}`).toBe(true)
}

const versionRow = (page: Page, version: string) =>
  page.getByRole('table', { name: '登録済みの版' }).locator('tbody tr').filter({ hasText: version })

async function registerVersion(page: Page, version: string) {
  await page.getByRole('form', { name: 'Profile の版の登録' }).getByLabel('版', { exact: true }).fill(version)
  await page.getByRole('button', { name: '版を登録' }).click()
}

test('スペシャルハーネスを登録し、版の登録・409・宣言の表示・既定の版の設定ができる', async ({ page, server }) => {
  // 登録
  await page.goto('/harnesses')
  await expect(page.getByText('登録済みの Harness はありません。')).toBeVisible()
  await page.getByRole('link', { name: 'Harness を登録' }).click()
  await page.getByLabel('Harness ID').fill(HARNESS)
  await page.getByLabel('名称').fill(HARNESS_NAME)
  await page.getByLabel('ハーネスリポジトリのパス').fill(server.harness.path)
  await page.getByLabel('ハーネスリポジトリ URL').fill('https://example.com/engineer.git')
  await page.getByRole('button', { name: '登録' }).click()
  await expect(page).toHaveURL(`/harnesses/${HARNESS}`)
  await expect(page.getByRole('heading', { name: HARNESS_NAME })).toBeVisible()
  await expect(page.getByText('登録済みの版はありません。')).toBeVisible()

  // 版の登録: commit SHA と「暫定運用」が出る。
  const first = tagProfile(server.harness.path, '1.0.0', 'first')
  await registerVersion(page, '1.0.0')
  await expect(page.getByRole('status')).toHaveText(`版 1.0.0 を登録しました。commit SHA: ${first}、状態: 暫定運用。`)
  await expect(versionRow(page, '1.0.0')).toContainText(first)
  await expect(versionRow(page, '1.0.0')).toContainText('暫定運用')

  // 同じ版を別の commit に付け直して登録すると 409 の内容が出て、登録済みの版はそのまま。
  const moved = tagProfile(server.harness.path, '1.0.0', 'moved')
  expect(moved).not.toBe(first)
  const [conflict] = await Promise.all([
    page.waitForResponse((r) => new URL(r.url()).pathname === '/api/harness-profiles' && r.request().method() === 'POST'),
    registerVersion(page, '1.0.0'),
  ])
  expect(conflict.status()).toBe(409)
  const alert = page.getByTestId('api-error')
  await expect(alert).toContainText('版 1.0.0 は別の commit SHA ですでに登録されています')
  await expect(alert).toContainText(`登録済みの commit SHA: ${first}`)
  await expect(page.getByRole('table', { name: '登録済みの版' }).locator('tbody tr')).toHaveCount(1)
  await screenshot(page, 'conflict')

  // 版の詳細: 段ごとの上限時間・許可宛先・鍵の名前、承認が要る操作と waivable。
  const stages = page.getByRole('table', { name: '段ごとの宣言' })
  const publish = stages.locator('tr').filter({ hasText: '外に出す（publish）' })
  await expect(publish).toContainText('120 秒')
  await expect(publish).toContainText('api.github.com:443')
  await expect(publish).toContainText('github_token')
  await expect(publish).toContainText('書き込み（write）')
  await expect(stages.locator('tr').filter({ hasText: '作業（work）' })).toContainText('600 秒')
  await expect(stages.locator('tr').filter({ hasText: '作業（work）' })).toContainText('claude-e2e-model')
  await expect(stages.locator('tr').filter({ hasText: '確かめる（check）' })).toContainText('60 秒')
  const approvals = page.getByRole('table', { name: '承認が要る操作' })
  await expect(approvals.locator('tr').filter({ hasText: 'merge_pull_request' })).toContainText('省ける（waivable: true）')
  await expect(approvals.locator('tr').filter({ hasText: 'delete_branch' })).toContainText('省けない（waivable: false）')
  await expect(page.getByText('必須（required）')).toBeVisible()
  // 鍵の値を表示する欄が無い（名前と使い方だけ）。
  await expect(page.getByText('鍵の値')).toHaveCount(0)
  await expect(page.locator('input[type=password]')).toHaveCount(0)
  const head = await stages.locator('thead th').allTextContents()
  expect(head).toEqual(['段', '上限時間', '外への接続の許可宛先', '鍵の名前と使い方', '実行するもの'])
  // 定義の全文も開ける。
  await page.getByText('定義の全文').click()
  await expect(page.getByLabel('定義の全文')).toContainText('"slot": "github_token"')

  // 直接振り分けるラベル（振り分けの表）が出る。
  await ownerPost(page, server.origin, '/api/routes', { label: 'repo:engineer', harnessId: HARNESS })
  await page.reload()
  await expect(page.getByRole('list', { name: '直接振り分けるラベル' })).toContainText('repo:engineer')

  // 既定の版を設定する。
  const settings = page.locator('[data-slot=card]').filter({ has: page.getByText('設定', { exact: true }) })
  await expect(settings).toContainText('未設定（直接振り分けたタスクは実行されません）')
  await versionRow(page, '1.0.0').getByRole('button', { name: '既定の版にする' }).click()
  await expect(page.getByRole('status').filter({ hasText: '既定の版を 1.0.0 にしました' })).toBeVisible()
  await expect(versionRow(page, '1.0.0')).toContainText('既定の版')
  await expect(settings.locator('div').filter({ hasText: /^既定の版1\.0\.0$/ })).toBeVisible()
  await screenshot(page, 'detail')

  // 一覧に既定の版が出る。
  await page.getByRole('link', { name: '一覧へ戻る' }).click()
  const row = page.locator('tbody tr').filter({ hasText: HARNESS_NAME })
  await expect(row).toContainText('1.0.0')
  await screenshot(page, 'list')

  // 編集: 既定の版の waivable な操作だけが並び、確認して足すと保存される。
  await row.getByRole('link', { name: HARNESS_NAME }).click()
  await page.getByRole('link', { name: '編集' }).click()
  await expect(page.getByLabel('既定の版')).toHaveValue(/.+/)
  await expect(page.getByRole('checkbox', { name: /delete_branch/ })).toHaveCount(0)
  await page.getByRole('checkbox', { name: /merge_pull_request/ }).click()
  await expect(page.getByRole('alertdialog')).toContainText('承認を待たずに実行します')
  await screenshot(page, 'form-confirm')
  await page.getByRole('alertdialog').getByRole('button', { name: '承認を省く' }).click()
  await page.getByRole('button', { name: '保存' }).click()
  await expect(page).toHaveURL(`/harnesses/${HARNESS}`)
  await expect(settings).toContainText('merge_pull_request')
})
