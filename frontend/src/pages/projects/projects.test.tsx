import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it } from 'vitest'
import { ApiClient } from '@/api/client'
import { registry, type Project } from '@/api/registry'
import { fakeFetch, json, type Recorded } from '@/test/fake-fetch'
import App from '@/App'

const RUNNERS = { items: [{ id: 'local', name: 'local', status: 'online', lastHeartbeatAt: null, createdAt: '2026-10-27T00:00:00Z' }], nextCursor: '' }
const PROJECT: Project = { id: 'alpha', name: 'Alpha', runnerId: 'local', harnessId: 'engineer', profileId: 'profile-1', params: {}, approvalWaivers: [], createdAt: '2026-10-27T00:00:00Z', updatedAt: '2026-10-27T00:00:00Z' }
function renderAt(path: string, handler: (request: Recorded) => Response) {
  window.history.pushState(null, '', path)
  const fake = fakeFetch((request) => request.url === '/api/session' ? json(200, { csrfToken: 't' }) : handler(request))
  render(<App api={new ApiClient(fake.fetch)} />)
  return fake
}
afterEach(() => window.history.pushState(null, '', '/'))

describe('Project の直接登録', () => {
  it('必須項目と JSON を検証し、旧項目を表示しない', async () => {
    const fake = renderAt('/projects/_new', () => json(200, RUNNERS))
    await userEvent.click(await screen.findByRole('button', { name: '登録' }))
    for (const label of ['Project ID', '名称', 'Runner', 'Harness ID', 'Profile ID']) {
      expect(within(screen.getByLabelText(label).closest('[data-field]') as HTMLElement).getByText('入力してください')).toBeVisible()
    }
    for (const label of ['既定の役割', 'Linear Team ID', '実行先ラベル']) expect(screen.queryByLabelText(label)).toBeNull()
    expect(fake.calls.some((call) => call.method === 'POST')).toBe(false)
  })
  it('新しい登録項目だけを送り、版と渡す値を保存する', async () => {
    const fake = renderAt('/projects/_new', (r) => r.url.startsWith('/api/runners') ? json(200, RUNNERS) : json(r.method === 'POST' ? 201 : 200, PROJECT))
    await userEvent.type(await screen.findByLabelText('Project ID'), 'alpha')
    await userEvent.type(screen.getByLabelText('名称'), 'Alpha')
    await userEvent.selectOptions(screen.getByLabelText('Runner'), 'local')
    await userEvent.type(screen.getByLabelText('Harness ID'), 'engineer')
    await userEvent.type(screen.getByLabelText('Profile ID'), 'profile-1')
    await userEvent.click(screen.getByRole('button', { name: '登録' }))
    expect(await screen.findByRole('heading', { name: 'Alpha' })).toBeVisible()
    expect(JSON.parse(fake.calls.find((r) => r.method === 'POST')?.body ?? '')).toEqual({ id: 'alpha', name: 'Alpha', runnerId: 'local', harnessId: 'engineer', profileId: 'profile-1', params: {}, approvalWaivers: [] })
  })
  it('更新時にも固定した Profile と承認を省く設定を保持する', async () => {
    const project = { ...PROJECT, params: { target: 'opaque' }, approvalWaivers: ['send'] }
    const fake = renderAt('/projects/alpha/edit', (r) => r.url.startsWith('/api/runners') ? json(200, RUNNERS) : json(200, project))
    await userEvent.clear(await screen.findByLabelText('名称'))
    await userEvent.type(screen.getByLabelText('名称'), 'Renamed')
    await userEvent.click(screen.getByRole('button', { name: '保存' }))
    expect(JSON.parse(fake.calls.find((r) => r.method === 'PATCH')?.body ?? '')).toEqual({ name: 'Renamed', runnerId: 'local', harnessId: 'engineer', profileId: 'profile-1', params: { target: 'opaque' }, approvalWaivers: ['send'] })
  })
  it('一覧 API のページを最後まで読む', async () => {
    const fake = fakeFetch((r) => json(200, r.url.includes('cursor=') ? { items: [{ ...PROJECT, id: 'b' }], nextCursor: '' } : { items: [{ ...PROJECT, id: 'a' }], nextCursor: 'next+1' }))
    expect((await registry.listProjects(new ApiClient(fake.fetch))).map((p) => p.id)).toEqual(['a', 'b'])
  })
})
