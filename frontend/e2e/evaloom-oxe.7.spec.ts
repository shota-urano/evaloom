import { mkdirSync, mkdtempSync, realpathSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { expect, test as base, type Page } from '@playwright/test'

import { loginOwner, run, setupOwner, startEvaloomd } from './evaloomd.ts'
import { createRepo, type Repo } from '../../scripts/fixtures.mts'

// evaloom-oxe.7: 記録の一覧と削除（13 3.8、契約 9章）。
// - スペシャルハーネスごとに記録が新しい順に出る。
// - 確認ダイアログを経て消すと「削除済み」になる。
//
// スペシャルハーネスは本物の API で登録する。記録は Runner の出す段の報告の後に Control Plane が作る行なので、
// その後と同じ形の行（tasks・executions・artifacts・harness_records）を DB へ直接入れ、本物の
// GET /api/harnesses/{id}/records と DELETE /api/harness-records/{id} を通して確かめる。
// 追跡ファイルはスクリーンショットだけを書く。

const SCREENSHOTS = fileURLToPath(new URL('../../docs/design/screenshots/', import.meta.url))
const HARNESS = 'engineer'
const HARNESS_NAME = 'エンジニア'

type Server = { origin: string; cookie: string; harness: Repo; db: string }

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
      const temp = realpathSync(mkdtempSync(join(tmpdir(), 'evaloom-e2e-oxe7-')))
      const db = join(temp, 'evaloom.db')
      const harness = createRepo(temp, 'harness')
      setupOwner(binary, db)
      const evaloomd = await startEvaloomd(binary, db, '0')
      try {
        await provide({ origin: evaloomd.origin, cookie: await login(evaloomd.origin), harness, db })
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

async function ownerPost(page: Page, origin: string, path: string, body: unknown) {
  const session = await page.request.get('/api/session')
  const { csrfToken } = (await session.json()) as { csrfToken: string }
  const response = await page.request.post(path, { data: body, headers: { 'X-CSRF-Token': csrfToken, Origin: origin } })
  expect(response.ok(), `${path}: ${response.status()} ${await response.text()}`).toBe(true)
}

const NOW = '2026-11-20T00:00:00Z'
const sqlText = (value: string | null) => (value === null ? 'NULL' : `'${value.replaceAll("'", "''")}'`)
/** ULID の形（01 3.1）の ID。 */
const ulid = (kind: string, n: number) => `01JE2E${kind}${String(n).padStart(19, '0')}`

type Seeded = { id: string; taskId: string; executionId: string; artifactId: string; sourceIdentifier: string | null; createdAt: string }

/** 出す段まで成功した Execution の記録（02 3.2 harness_records、19 3.8）と同じ形の行を入れる。 */
function seedRecords(db: string, rows: { n: number; createdAt: string; sourceIdentifier: string | null }[]): Seeded[] {
  const statements: string[] = []
  const seeded = rows.map(({ n, createdAt, sourceIdentifier }) => {
    const r: Seeded = { id: ulid('R', n), taskId: `task-${n}`, executionId: ulid('X', n), artifactId: ulid('A', n), sourceIdentifier, createdAt }
    statements.push(
      `INSERT INTO tasks (id, source, project_id, harness_id, title, body, acceptance_criteria, collaboration_mode, status, idempotency_key, created_at, updated_at)
        VALUES ('${r.taskId}', ${sourceIdentifier ? "'linear'" : "'manual'"}, NULL, '${HARNESS}', 'タスク ${n}', '本文', '条件', 'solo', 'completed', 'idem-${n}', '${NOW}', '${NOW}');`,
      `INSERT INTO executions (id, task_id, seq, mode, status, route_kind, created_at)
        VALUES ('${r.executionId}', '${r.taskId}', 1, 'continue', 'succeeded', 'harness', '${NOW}');`,
      `INSERT INTO artifacts (id, execution_id, stage, kind, name, storage_path, sha256, size_bytes, redacted, created_at)
        VALUES ('${r.artifactId}', '${r.executionId}', 'check', 'out_file', 'check/report.md', 'e2e/${r.artifactId}', '${'0'.repeat(64)}', 0, 1, '${createdAt}');`,
      `INSERT INTO harness_records (id, harness_id, artifact_id, execution_id, task_id, source_identifier, file_name, created_at, deleted_at)
        VALUES ('${r.id}', '${HARNESS}', '${r.artifactId}', '${r.executionId}', '${r.taskId}', ${sqlText(sourceIdentifier)}, 'check/report.md', '${createdAt}', NULL);`,
    )
    return r
  })
  run('sqlite3', [db, statements.join('\n')])
  return seeded
}

async function screenshot(page: Page, screen: string) {
  mkdirSync(SCREENSHOTS, { recursive: true })
  await page.screenshot({ path: join(SCREENSHOTS, `evaloom-oxe.7-${screen}.png`), fullPage: true })
}

test('スペシャルハーネスの記録が新しい順に出て、確認ダイアログを経て消すと「削除済み」になる', async ({ page, server }) => {
  await ownerPost(page, server.origin, '/api/harnesses', {
    id: HARNESS,
    name: HARNESS_NAME,
    repoPath: server.harness.path,
    repoUrl: 'https://example.com/engineer.git',
  })
  // 入れる順と日付の順をばらばらにし、秒の端数の有無も混ぜる（サーバーの並び＝新しい順をそのまま出す）。
  const [r101, r103, r102] = seedRecords(server.db, [
    { n: 101, createdAt: '2026-11-18T09:00:00Z', sourceIdentifier: 'ENG-101' },
    { n: 103, createdAt: '2026-11-19T09:00:00.250Z', sourceIdentifier: null },
    { n: 102, createdAt: '2026-11-19T09:00:00Z', sourceIdentifier: 'ENG-102' },
  ])

  // 詳細画面から記録の一覧へ移る。
  await page.goto(`/harnesses/${HARNESS}`)
  await page.getByRole('link', { name: '記録の一覧' }).click()
  await expect(page).toHaveURL(`/harnesses/${HARNESS}/records`)
  await expect(page.getByRole('heading', { name: `${HARNESS_NAME} の記録` })).toBeVisible()

  // 新しい順（記録した日付の降順）。
  const rows = page.getByRole('table', { name: '記録の一覧' }).locator('tbody tr')
  await expect(rows).toHaveCount(3)
  expect(await rows.evaluateAll((trs) => trs.map((tr) => tr.getAttribute('data-record')))).toEqual([r103.id, r102.id, r101.id])
  // 出所の識別子とタスク詳細へのリンク、成果物の内容へのリンク。
  await expect(rows.nth(1).getByRole('link', { name: 'ENG-102' })).toHaveAttribute('href', `/tasks/${r102.taskId}`)
  await expect(rows.nth(0).getByRole('link', { name: r103.taskId })).toHaveAttribute('href', `/tasks/${r103.taskId}`)
  await expect(rows.nth(1).getByRole('link', { name: '内容を開く' })).toHaveAttribute(
    'href',
    `/api/executions/${r102.executionId}/artifacts/${r102.artifactId}`,
  )
  await screenshot(page, 'records')

  // 確認ダイアログでやめると消さない。
  const target = rows.filter({ hasText: 'ENG-102' })
  await target.getByRole('button', { name: '消す' }).click()
  const dialog = page.getByRole('alertdialog')
  await expect(dialog).toContainText('次の仕事に渡りません')
  await expect(dialog).toContainText('ENG-102')
  await dialog.getByRole('button', { name: 'やめる' }).click()
  await expect(dialog).toHaveCount(0)
  await expect(page.getByText('削除済み', { exact: true })).toHaveCount(0)

  // 確認ダイアログを経て消すと「削除済み」になる。
  await target.getByRole('button', { name: '消す' }).click()
  await screenshot(page, 'confirm')
  const [deleted] = await Promise.all([
    page.waitForResponse((r) => r.request().method() === 'DELETE' && new URL(r.url()).pathname === `/api/harness-records/${r102.id}`),
    page.getByRole('alertdialog').getByRole('button', { name: '消す' }).click(),
  ])
  expect(deleted.status()).toBe(204)
  await expect(target).toContainText('削除済み')
  await expect(target.getByRole('button', { name: '消す' })).toHaveCount(0)
  await expect(page.getByRole('status')).toContainText('次の仕事に渡しません')
  // DB でも deleted_at が入り、ほかの記録は消えていない（次の仕事に渡さない、19 3.8）。
  const deletedAt = run('sqlite3', [server.db, `SELECT id FROM harness_records WHERE deleted_at IS NOT NULL;`]).trim()
  expect(deletedAt).toBe(r102.id)
  // 読み直しても「削除済み」のまま。
  await page.reload()
  await expect(rows.filter({ hasText: 'ENG-102' })).toContainText('削除済み')
  // ほかの記録は消えていない。
  await expect(rows).toHaveCount(3)
  await expect(page.getByText('削除済み', { exact: true })).toHaveCount(1)
  await screenshot(page, 'deleted')
})
