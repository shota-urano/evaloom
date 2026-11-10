import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, expect, it } from 'vitest'
import { ApiClient } from '@/api/client'
import type { Harness } from '@/api/registry'
import { fakeFetch, json, type Recorded } from '@/test/fake-fetch'
import App from '@/App'

const HARNESS: Harness = { id: 'engineer', name: 'Engineer', repoPath: '/repos/engineer', repoUrl: 'https://example.invalid/h.git', defaultProfileId: null, approvalWaivers: [], createdAt: '2026-10-27T00:00:00Z', updatedAt: '2026-10-27T00:00:00Z' }
function renderAt(path: string, handler: (r: Recorded) => Response) {
  window.history.pushState(null, '', path)
  const fake = fakeFetch((r) => r.url === '/api/session' ? json(200, { csrfToken: 't' }) : handler(r))
  render(<App api={new ApiClient(fake.fetch)} />)
  return fake
}
afterEach(() => window.history.pushState(null, '', '/'))
it('Harness はリポジトリの情報だけを登録する', async () => {
  const fake = renderAt('/harnesses/_new', (r) => r.method === 'POST' ? json(201, HARNESS) : json(200, { items: [], nextCursor: '' }))
  await userEvent.type(await screen.findByLabelText('Harness ID'), 'engineer')
  await userEvent.type(screen.getByLabelText('名称'), 'Engineer')
  await userEvent.type(screen.getByLabelText('ハーネスリポジトリのパス'), '/repos/engineer')
  await userEvent.type(screen.getByLabelText('ハーネスリポジトリ URL'), 'https://example.invalid/h.git')
  for (const label of ['Adapter', '専門分野', 'Tool Permissions', '対応モデル']) expect(screen.queryByLabelText(label)).toBeNull()
  await userEvent.click(screen.getByRole('button', { name: '登録' }))
  expect(JSON.parse(fake.calls.find((r) => r.method === 'POST')?.body ?? '')).toEqual({ id: 'engineer', name: 'Engineer', repoPath: '/repos/engineer', repoUrl: 'https://example.invalid/h.git' })
})
it('編集では直接振り分けの設定を要求から除き、変更しない', async () => {
  const fake = renderAt('/harnesses/engineer/edit', () => json(200, { items: [HARNESS], nextCursor: '' }))
  await userEvent.clear(await screen.findByLabelText('名称'))
  await userEvent.type(screen.getByLabelText('名称'), 'Renamed')
  await userEvent.click(screen.getByRole('button', { name: '保存' }))
  expect(JSON.parse(fake.calls.find((r) => r.method === 'PATCH')?.body ?? '')).toEqual({ name: 'Renamed', repoPath: '/repos/engineer', repoUrl: 'https://example.invalid/h.git' })
})
