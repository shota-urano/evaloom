import { spawnSync } from 'node:child_process'
import { mkdirSync, mkdtempSync, realpathSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { expect, test as base, type Page } from '@playwright/test'

import { loginOwner, run, setupOwner, startEvaloomd, type Evaloomd } from './evaloomd.ts'

// evaloom-dcj.12: Settings の起動の合図のラベル名・振り分けのラベルの接頭辞・振り分けを提案する AI と Runner、
// Dashboard の Linear の取り込みの状況（15-linear-intake 3.4・5章・6章、Q160・Q229）。
// - 設定を保存して表示できる。
// - 400（起動の合図のラベルが接頭辞で始まる）と 422（振り分けの表のラベルが新しい接頭辞で始まらない）の内容がフォームに出る。
// - 照会が続けて失敗しているとき、Dashboard のエラー状況に失敗の分類と最後に成功した時刻が出る。
//
// Linear の実 API には接続しない。evaloomd は HOME を空の一時ディレクトリにして起動するので、API キーのファイルが無く、
// Linear Poller は照会を始めない（15 6章）。照会が続けて失敗している状況は linear_poll_teams に直接入れる。
// 振り分けの表の行（routes）は起動前に DB へ入れる。Runner は runner register で1台登録し、生存報告を送らない（offline）。

const SCREENSHOTS = fileURLToPath(new URL('../../docs/design/screenshots/', import.meta.url))
const NOW = '2026-11-20T00:00:00Z'
const RUNNER = 'local'
const ROUTE_LABEL = 'route:web'
const TEAM = 'TEAM-USLAB'
const LAST_SUCCESS = '2026-11-19T23:40:00Z'

type Server = { origin: string; cookie: string; db: string }

const sqlText = (value: unknown) => `'${(typeof value === 'string' ? value : JSON.stringify(value)).replaceAll("'", "''")}'`

function seed(db: string) {
  run('sqlite3', [
    db,
    `INSERT INTO harnesses (id, name, repo_url, repo_path, default_profile_id, approval_waivers, created_at, updated_at)
      VALUES ('e2e-web', 'E2E Web ハーネス', 'https://example.com/web.git', '/tmp/e2e-web', NULL, '[]', '${NOW}', '${NOW}');
     INSERT INTO routes (label, project_id, harness_id, created_at, updated_at)
      VALUES (${sqlText(ROUTE_LABEL)}, NULL, 'e2e-web', '${NOW}', '${NOW}');`,
  ])
}

function register(binary: string, db: string, temp: string) {
  const registered = spawnSync(binary, ['-db', db, 'runner', 'register', '-worktree-root', join(temp, 'worktrees'), '-name', 'ローカル Runner', RUNNER], {
    encoding: 'utf8',
  })
  if (registered.status !== 0) throw new Error(`runner register が失敗した: ${registered.stderr}`)
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

const test = base.extend<{ server: Server }, { worker: Server }>({
  worker: [
    // oxlint-disable-next-line no-empty-pattern
    async ({}, provide) => {
      const binary = process.env.EVALOOMD_BINARY
      if (!binary) throw new Error('EVALOOMD_BINARY が無い（e2e/global-setup.ts が設定する）')
      const temp = realpathSync(mkdtempSync(join(tmpdir(), 'evaloom-e2e-dcj12-')))
      const db = join(temp, 'evaloom.db')
      const home = join(temp, 'home')
      mkdirSync(home)
      register(binary, db, temp)
      setupOwner(binary, db)
      seed(db)
      const evaloomd = await startWithoutLinearKey(binary, db, home)
      try {
        const response = await loginOwner(evaloomd.origin)
        const cookie = /evaloom_session=([^;]+)/.exec(response.headers.getSetCookie().join('\n'))?.[1]
        if (!response.ok || !cookie) throw new Error(`ログインに失敗した: ${response.status}`)
        await provide({ origin: evaloomd.origin, cookie, db })
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

async function screenshot(page: Page, screen: string) {
  mkdirSync(SCREENSHOTS, { recursive: true })
  await page.screenshot({ path: join(SCREENSHOTS, `evaloom-dcj.12-${screen}.png`), fullPage: true })
}

const patchSettings = (page: Page) => page.waitForRequest((r) => new URL(r.url()).pathname === '/api/settings' && r.method() === 'PATCH')

test('Settings: 400 と 422 の内容がフォームに出て、どの項目も保存されない', async ({ page, server }) => {
  await page.goto('/settings')
  const form = page.getByRole('form', { name: '全体設定' })
  const trigger = form.getByLabel(/linear\.triggerLabel/)
  const prefix = form.getByLabel(/linear\.routingLabelPrefix/)
  await expect(trigger).toHaveValue('ai-ready')
  await expect(prefix).toHaveValue('route:')

  // 400: 起動の合図のラベルが振り分けのラベルの接頭辞で始まる（Q160）。
  await trigger.fill('route:go')
  const [rejected] = await Promise.all([patchSettings(page), form.getByRole('button', { name: '保存' }).click()])
  expect((await rejected.response())?.status()).toBe(400)
  await expect(page.locator('[data-field="linear.triggerLabel"]')).toContainText('起動の合図のラベルが振り分けのラベルの接頭辞で始まっています')
  await expect(trigger).toHaveAttribute('aria-invalid', 'true')
  await expect(form.getByRole('alert')).toContainText('入力内容を確認してください')

  // 422: 振り分けの表に新しい接頭辞で始まらないラベル（route:web）が残る（Q160）。
  await trigger.fill('ai-ready')
  await prefix.fill('to:')
  const [precondition] = await Promise.all([patchSettings(page), form.getByRole('button', { name: '保存' }).click()])
  expect(precondition.postDataJSON()).toEqual({ linear: { routingLabelPrefix: 'to:' } })
  expect((await precondition.response())?.status()).toBe(422)
  await expect(page.locator('[data-field="linear.routingLabelPrefix"]')).toContainText(`振り分けの表のラベル「${ROUTE_LABEL}」がこの接頭辞で始まりません`)
  await expect(prefix).toHaveAttribute('aria-invalid', 'true')
  const alert = form.getByRole('alert')
  await expect(alert).toContainText('前提条件が満たされていません')
  await expect(alert).toContainText('どの項目も変わっていません')
  await expect(alert).toContainText(`振り分けの表のラベル「${ROUTE_LABEL}」: 振り分けのラベルの接頭辞で始まりません`)
  await expect(form.getByRole('link', { name: '振り分けの表を開く' })).toHaveAttribute('href', '/routes')
  await expect(form.getByRole('status')).toHaveCount(0)
  await screenshot(page, 'settings')

  // 拒否された PATCH は何も変えていない。
  const saved = (await owner(server, 'GET', '/api/settings')) as { linear: { triggerLabel: string; routingLabelPrefix: string } }
  expect(saved.linear).toEqual({ triggerLabel: 'ai-ready', routingLabelPrefix: 'route:' })
})

test('Settings: 起動の合図のラベル名・接頭辞・提案する AI・Runner を保存し、読み直しても表示される', async ({ page }) => {
  await page.goto('/settings')
  const form = page.getByRole('form', { name: '全体設定' })
  await expect(form.getByLabel(/linear\.triggerLabel/)).toHaveValue('ai-ready')

  // 接頭辞は振り分けの表のラベル（route:web）がまだ始まる値に変える。
  await form.getByLabel(/linear\.triggerLabel/).fill('evaloom-go')
  await form.getByLabel(/linear\.routingLabelPrefix/).fill('route')
  await form.getByLabel(/提案に使うミクロハーネス/).selectOption('codex')
  await form.getByLabel(/提案に使うモデル/).fill('gpt-e2e')
  await form.getByLabel(/routing\.defaultRunnerId/).selectOption(RUNNER)
  const [request] = await Promise.all([patchSettings(page), form.getByRole('button', { name: '保存' }).click()])
  expect(request.postDataJSON()).toEqual({
    linear: { triggerLabel: 'evaloom-go', routingLabelPrefix: 'route' },
    routing: { proposer: { microHarness: 'codex', model: 'gpt-e2e' }, defaultRunnerId: RUNNER },
  })
  expect((await request.response())?.status()).toBe(200)
  await expect(form.getByRole('status')).toHaveText('保存しました。')

  await page.reload()
  await expect(form.getByLabel(/linear\.triggerLabel/)).toHaveValue('evaloom-go')
  await expect(form.getByLabel(/linear\.routingLabelPrefix/)).toHaveValue('route')
  await expect(form.getByLabel(/提案に使うミクロハーネス/)).toHaveValue('codex')
  await expect(form.getByLabel(/提案に使うモデル/)).toHaveValue('gpt-e2e')
  await expect(form.getByLabel(/routing\.defaultRunnerId/)).toHaveValue(RUNNER)
  await expect(page.getByTestId('api-error')).toHaveCount(0)
})

test('Dashboard: 続けて失敗している照会の分類と最後に成功した時刻、提案を動かす Runner が無い理由が出る', async ({ page, server }) => {
  // 照会先 Team が5回続けて失敗した（15 6章の上限）。Poller の後と同じ形の行を入れる。
  run('sqlite3', [
    server.db,
    `INSERT INTO linear_poll_teams (team_id, baseline_done_at, last_success_at, last_error, consecutive_failures)
      VALUES ('${TEAM}', '2026-11-19T00:00:00Z', '${LAST_SUCCESS}', 'network', 5)
      ON CONFLICT (team_id) DO UPDATE SET last_success_at = excluded.last_success_at, last_error = excluded.last_error,
        consecutive_failures = excluded.consecutive_failures;`,
  ])

  // routing.defaultRunnerId が未設定 → reason=unset（Q229）。
  await owner(server, 'PATCH', '/api/settings', { routing: { defaultRunnerId: null } })
  await page.goto('/')
  const failures = page.getByRole('list', { name: 'Linear の照会の失敗' })
  await expect(failures.getByRole('listitem')).toHaveCount(1)
  const item = failures.getByRole('listitem').first()
  await expect(item).toContainText(TEAM)
  await expect(item).toContainText('通信エラー（network）')
  await expect(item).toContainText('5 回続けて失敗')
  await expect(item.locator('time')).toHaveAttribute('datetime', LAST_SUCCESS)
  await expect(item).toContainText(`最後に成功した時刻: ${new Date(LAST_SUCCESS).toLocaleString('ja-JP')}`)

  const section = page.getByTestId('linear-status')
  const runner = section.getByTestId('linear-routing-runner-problem')
  await expect(runner).toHaveAttribute('data-reason', 'unset')
  await expect(runner).toContainText('振り分けの提案を動かす Runner が未設定です')
  await expect(runner).toContainText('理由: 未設定・unset')
  await expect(section.getByRole('table', { name: 'Linear Team の照会の状況' }).getByRole('row', { name: new RegExp(TEAM) })).toContainText('5 回')

  // 生存報告を送っていない Runner を設定する → reason=offline。
  await owner(server, 'PATCH', '/api/settings', { routing: { defaultRunnerId: RUNNER } })
  await page.getByRole('button', { name: '再読み込み' }).click()
  await expect(runner).toHaveAttribute('data-reason', 'offline')
  await expect(runner).toContainText('振り分けの提案を動かす Runner が offline です')
  await expect(runner).toContainText('理由: offline）')
  await screenshot(page, 'dashboard')
})
