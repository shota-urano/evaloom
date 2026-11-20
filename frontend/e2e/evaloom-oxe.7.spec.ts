import { mkdirSync, mkdtempSync, realpathSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { expect, test as base, type Page, type Route } from '@playwright/test'

import { loginOwner, setupOwner, startEvaloomd } from './evaloomd.ts'
import { createRepo, type Repo } from '../../scripts/fixtures.mts'

// evaloom-oxe.7: 記録の一覧と削除（13 3.8、契約 9章）。
// - スペシャルハーネスごとに記録が新しい順に出る。
// - 確認ダイアログを経て消すと「削除済み」になる。
//
// スペシャルハーネスは本物の evaloomd に登録する。記録の API（GET /api/harnesses/{id}/records、
// DELETE /api/harness-records/{id}、01 5.1）は evaloomd にまだ無い（仕様の穴として報告済み）ため、
// この2本だけを page.route で 02 3.2 harness_records の形（camelCase、01 5.3）の応答に差し替える。
// 追跡ファイルはスクリーンショットだけを書く。

const SCREENSHOTS = fileURLToPath(new URL('../../docs/design/screenshots/', import.meta.url))
const HARNESS = 'engineer'
const HARNESS_NAME = 'エンジニア'

type Server = { origin: string; cookie: string; harness: Repo }

type HarnessRecord = {
  id: string
  harnessId: string
  artifactId: string
  executionId: string
  taskId: string
  sourceIdentifier: string | null
  fileName: string
  createdAt: string
  deletedAt: string | null
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
      const temp = realpathSync(mkdtempSync(join(tmpdir(), 'evaloom-e2e-oxe7-')))
      const db = join(temp, 'evaloom.db')
      const harness = createRepo(temp, 'harness')
      setupOwner(binary, db)
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

async function ownerPost(page: Page, origin: string, path: string, body: unknown) {
  const session = await page.request.get('/api/session')
  const { csrfToken } = (await session.json()) as { csrfToken: string }
  const response = await page.request.post(path, { data: body, headers: { 'X-CSRF-Token': csrfToken, Origin: origin } })
  expect(response.ok(), `${path}: ${response.status()} ${await response.text()}`).toBe(true)
}

const record = (n: number, createdAt: string, fileName: string, sourceIdentifier: string | null): HarnessRecord => ({
  id: `01JE2E0R00000000000000000${n}`,
  harnessId: HARNESS,
  artifactId: `01JE2E0A00000000000000000${n}`,
  executionId: `01JE2E0X00000000000000000${n}`,
  taskId: `01JE2E0T00000000000000000${n}`,
  sourceIdentifier,
  fileName,
  createdAt,
  deletedAt: null,
})

/** 記録の API 2本だけを差し替える。DELETE を受けたら deletedAt を入れ、以後の一覧に「消した記録」として返す。 */
async function stubRecordsApi(page: Page, records: HarnessRecord[]) {
  const deletes: { id: string; csrf: string | undefined }[] = []
  // サーバーの並びに頼らないことも確かめるため、応答は古い順のまま返す。
  await page.route(`**/api/harnesses/${HARNESS}/records*`, (route: Route) =>
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ items: records, nextCursor: '' }) }),
  )
  await page.route('**/api/harness-records/*', (route: Route) => {
    const request = route.request()
    if (request.method() !== 'DELETE') return route.fallback()
    const id = decodeURIComponent(new URL(request.url()).pathname.split('/').pop() ?? '')
    deletes.push({ id, csrf: request.headers()['x-csrf-token'] })
    const target = records.find((r) => r.id === id)
    if (!target) return route.fulfill({ status: 404, contentType: 'application/json', body: JSON.stringify({ error: { code: 'not_found', message: 'record not found', details: [] } }) })
    target.deletedAt = '2026-11-20T03:00:00Z'
    return route.fulfill({ status: 204 })
  })
  return deletes
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
  const records = [
    record(1, '2026-11-18T09:00:00Z', 'check/report.md', 'ENG-101'),
    record(2, '2026-11-19T09:00:00Z', 'check/report.md', 'ENG-102'),
    record(3, '2026-11-20T09:00:00Z', 'check/report.md', null),
  ]
  const deletes = await stubRecordsApi(page, records)

  // 詳細画面から記録の一覧へ移る。
  await page.goto(`/harnesses/${HARNESS}`)
  await page.getByRole('link', { name: '記録の一覧' }).click()
  await expect(page).toHaveURL(`/harnesses/${HARNESS}/records`)
  await expect(page.getByRole('heading', { name: `${HARNESS_NAME} の記録` })).toBeVisible()

  // 新しい順（記録した日付の降順）。
  const rows = page.getByRole('table', { name: '記録の一覧' }).locator('tbody tr')
  await expect(rows).toHaveCount(3)
  expect(await rows.evaluateAll((trs) => trs.map((tr) => tr.getAttribute('data-record')))).toEqual([records[2].id, records[1].id, records[0].id])
  // 出所の識別子とタスク詳細へのリンク、成果物の内容へのリンク。
  await expect(rows.nth(1).getByRole('link', { name: 'ENG-102' })).toHaveAttribute('href', `/tasks/${records[1].taskId}`)
  await expect(rows.nth(0).getByRole('link', { name: records[2].taskId })).toHaveAttribute('href', `/tasks/${records[2].taskId}`)
  await expect(rows.nth(1).getByRole('link', { name: '内容を開く' })).toHaveAttribute(
    'href',
    `/api/executions/${records[1].executionId}/artifacts/${records[1].artifactId}`,
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
  expect(deletes).toHaveLength(0)
  await expect(page.getByText('削除済み', { exact: true })).toHaveCount(0)

  // 確認ダイアログを経て消すと「削除済み」になる。
  await target.getByRole('button', { name: '消す' }).click()
  await screenshot(page, 'confirm')
  await page.getByRole('alertdialog').getByRole('button', { name: '消す' }).click()
  await expect(target).toContainText('削除済み')
  await expect(target.getByRole('button', { name: '消す' })).toHaveCount(0)
  await expect(page.getByRole('status')).toContainText('次の仕事に渡しません')
  expect(deletes).toHaveLength(1)
  expect(deletes[0].id).toBe(records[1].id)
  expect(deletes[0].csrf).toBeTruthy()
  // ほかの記録は消えていない。
  await expect(rows).toHaveCount(3)
  await expect(page.getByText('削除済み', { exact: true })).toHaveCount(1)
  await screenshot(page, 'deleted')
})
