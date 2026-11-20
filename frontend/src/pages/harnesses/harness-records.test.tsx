import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, expect, it } from 'vitest'
import { ApiClient } from '@/api/client'
import type { Harness, HarnessRecord } from '@/api/registry'
import { fakeFetch, json, type Recorded } from '@/test/fake-fetch'
import App from '@/App'

const T = '2026-11-01T00:00:00Z'
const HARNESS: Harness = { id: 'engineer', name: 'Engineer', repoPath: '/r', repoUrl: 'https://example.invalid/h.git', defaultProfileId: null, approvalWaivers: [], createdAt: T, updatedAt: T }
const record = (id: string, createdAt: string, fileName: string, sourceIdentifier: string | null): HarnessRecord => ({
  id,
  harnessId: 'engineer',
  artifactId: `art-${id}`,
  executionId: `exe-${id}`,
  taskId: `task-${id}`,
  sourceIdentifier,
  fileName,
  createdAt,
  deletedAt: null,
})

function renderRecords(records: HarnessRecord[]) {
  window.history.pushState(null, '', '/harnesses/engineer/records')
  const fake = fakeFetch((r: Recorded) => {
    if (r.url === '/api/session') return json(200, { csrfToken: 't' })
    if (r.method === 'DELETE') {
      const id = decodeURIComponent(r.url.split('/').pop() ?? '')
      records = records.map((x) => (x.id === id ? { ...x, deletedAt: '2026-11-20T00:00:00Z' } : x))
      return new Response(null, { status: 204 })
    }
    if (r.url.startsWith('/api/harnesses/engineer/records')) return json(200, { items: records, nextCursor: '' })
    if (r.url.startsWith('/api/harnesses/engineer')) return json(200, HARNESS)
    return json(404, { error: { code: 'not_found', message: 'x', details: [] } })
  })
  render(<App api={new ApiClient(fake.fetch)} />)
  return fake
}
afterEach(() => window.history.pushState(null, '', '/'))

it('記録をサーバーの応答の順（新しい順）のまま出し、タスクと成果物へのリンクを付ける', async () => {
  renderRecords([record('r2', '2026-11-03T00:00:00Z', 'new.md', null), record('r3', '2026-11-02T00:00:00.5Z', 'mid.md', 'ENG-3'), record('r1', '2026-11-02T00:00:00Z', 'old.md', 'ENG-1')])
  const table = await screen.findByRole('table', { name: '記録の一覧' })
  const rows = within(table).getAllByRole('row').slice(1)
  expect(rows.map((row) => row.getAttribute('data-record'))).toEqual(['r2', 'r3', 'r1'])
  expect(within(rows[1]).getByRole('link', { name: 'ENG-3' }).getAttribute('href')).toBe('/tasks/task-r3')
  expect(within(rows[0]).getByRole('link', { name: 'task-r2' })).toBeTruthy()
  expect(within(rows[0]).getByRole('link', { name: '内容を開く' }).getAttribute('href')).toBe('/api/executions/exe-r2/artifacts/art-r2')
})

it('確認ダイアログでやめると消さず、消すと削除済みになる', async () => {
  const fake = renderRecords([record('r1', T, 'notes.md', 'ENG-1')])
  await userEvent.click(await screen.findByRole('button', { name: '消す' }))
  let dialog = screen.getByRole('alertdialog')
  expect(dialog.textContent).toContain('次の仕事に渡りません')
  await userEvent.click(within(dialog).getByRole('button', { name: 'やめる' }))
  expect(fake.calls.some((c) => c.method === 'DELETE')).toBe(false)

  await userEvent.click(screen.getByRole('button', { name: '消す' }))
  dialog = screen.getByRole('alertdialog')
  await userEvent.click(within(dialog).getByRole('button', { name: '消す' }))
  const del = fake.calls.find((c) => c.method === 'DELETE')
  expect(del?.url).toBe('/api/harness-records/r1')
  expect(del?.headers['X-CSRF-Token']).toBe('t')
  const row = await screen.findByText('削除済み')
  expect(row).toBeTruthy()
  expect(screen.queryByRole('button', { name: '消す' })).toBeNull()
})
