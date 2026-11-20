import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it } from 'vitest'
import { ApiClient } from '@/api/client'
import { registry, type Project } from '@/api/registry'
import { apiError, fakeFetch, json, type Recorded } from '@/test/fake-fetch'
import App from '@/App'

const T = '2026-10-27T00:00:00Z'
const RUNNERS = { items: [{ id: 'local', name: 'local', status: 'online', lastHeartbeatAt: null, createdAt: T }], nextCursor: '' }
const HARNESSES = { items: [{ id: 'engineer', name: 'Engineer', repoPath: '/r', repoUrl: '', defaultProfileId: null, approvalWaivers: [], createdAt: T, updatedAt: T }], nextCursor: '' }
const DEFINITION = {
  params: { schema: 'evaloom/params.schema.json' },
  approvals: [
    { operation: 'merge_pull_request', description: 'PR をマージする', waivable: true },
    { operation: 'deploy', description: '本番へ出す' },
    { operation: 'comment', description: 'コメントする', waivable: true },
  ],
}
const PROFILES = {
  items: [
    { id: 'profile-1', harnessId: 'engineer', version: '1.0.0', commitSha: 'a'.repeat(40), modelId: 'm', operationStatus: 'provisional', registeredAt: T, definition: DEFINITION },
    { id: 'profile-2', harnessId: 'engineer', version: '2.0.0', commitSha: 'b'.repeat(40), modelId: 'm', operationStatus: 'provisional', registeredAt: T, definition: { approvals: [{ operation: 'comment', waivable: true }] } },
  ],
  nextCursor: '',
}
const SETTINGS = { linear: { triggerLabel: 'ai-ready', routingLabelPrefix: 'repo:' } }
const PROJECT: Project = { id: 'alpha', name: 'Alpha', routingLabel: 'repo:alpha', runnerId: 'local', harnessId: 'engineer', profileId: 'profile-1', params: {}, approvalWaivers: [], createdAt: T, updatedAt: T }

/** 選択肢の一覧の API はここで返し、Project と routes の要求だけを handler に任せる。 */
function renderAt(path: string, handler: (request: Recorded) => Response = () => json(200, PROJECT)) {
  window.history.pushState(null, '', path)
  const fake = fakeFetch((r) => {
    if (r.url === '/api/session') return json(200, { csrfToken: 't' })
    if (r.url.startsWith('/api/runners')) return json(200, RUNNERS)
    if (r.url.startsWith('/api/harnesses')) return json(200, HARNESSES)
    if (r.url.startsWith('/api/harness-profiles')) return json(200, PROFILES)
    if (r.url === '/api/settings') return json(200, SETTINGS)
    return handler(r)
  })
  render(<App api={new ApiClient(fake.fetch)} />)
  return fake
}
afterEach(() => window.history.pushState(null, '', '/'))

async function fillNew(id: string, label: string) {
  await userEvent.type(await screen.findByLabelText('Project ID'), id)
  await userEvent.type(screen.getByLabelText('名前'), id.toUpperCase())
  await userEvent.type(screen.getByLabelText('振り分けのラベル'), label)
  await userEvent.selectOptions(screen.getByLabelText('動かすマシン'), 'local')
  await userEvent.selectOptions(screen.getByLabelText('頼むスペシャルハーネス'), 'engineer')
  await screen.findByRole('option', { name: /v1\.0\.0/ })
  await userEvent.selectOptions(screen.getByLabelText('版'), 'profile-1')
}
const body = (fake: { calls: Recorded[] }, method: string, url: string) =>
  JSON.parse(fake.calls.find((r) => r.method === method && r.url === url)?.body ?? 'null') as unknown

describe('Project の登録・編集（13 3.5）', () => {
  it('入力欄は6項目（登録時は Project ID も）だけで、必須項目と接頭辞を検証する', async () => {
    const fake = renderAt('/projects/_new')
    const form = await screen.findByRole('form', { name: 'プロジェクトの登録' })
    const names = [...form.querySelectorAll('input, select, textarea')].map((e) => e.getAttribute('name'))
    expect(new Set(names)).toEqual(new Set(['id', 'name', 'routingLabel', 'harnessId', 'profileId', 'params', 'runnerId']))
    await userEvent.type(screen.getByLabelText('振り分けのラベル'), 'route:x')
    await userEvent.click(screen.getByRole('button', { name: '登録' }))
    for (const label of ['Project ID', '名前', '動かすマシン', '頼むスペシャルハーネス', '版']) {
      expect(within(screen.getByLabelText(label).closest('[data-field]') as HTMLElement).getByText('入力してください')).toBeVisible()
    }
    expect(screen.getByText('「repo:」で始めてください')).toBeVisible()
    expect(fake.calls.some((call) => call.method === 'POST')).toBe(false)
  })

  it('Project を登録した後に振り分けのラベルを登録する', async () => {
    const fake = renderAt('/projects/_new', (r) => json(r.method === 'POST' ? 201 : 200, r.url === '/api/routes' ? {} : PROJECT))
    await fillNew('alpha', 'repo:alpha')
    await userEvent.click(screen.getByRole('button', { name: '登録' }))
    expect(await screen.findByRole('heading', { name: 'Alpha' })).toBeVisible()
    expect(body(fake, 'POST', '/api/projects')).toEqual({ id: 'alpha', name: 'ALPHA', harnessId: 'engineer', profileId: 'profile-1', params: {}, approvalWaivers: [], runnerId: 'local' })
    expect(body(fake, 'POST', '/api/routes')).toEqual({ label: 'repo:alpha', projectId: 'alpha' })
    expect(fake.calls.filter((r) => r.method === 'POST').map((r) => r.url)).toEqual(['/api/projects', '/api/routes'])
  })

  it('承認を省く設定に、選んだ版で waivable の操作だけが並び、確認しないと足されない', async () => {
    const fake = renderAt('/projects/_new', (r) => json(r.method === 'POST' ? 201 : 200, r.url === '/api/routes' ? {} : PROJECT))
    await fillNew('alpha', 'repo:alpha')
    const group = screen.getByRole('group', { name: '承認を省く設定' })
    expect(within(group).getAllByRole('checkbox').map((c) => (c as HTMLInputElement).value)).toEqual(['merge_pull_request', 'comment'])
    expect(screen.getByTestId('params-schema')).toHaveTextContent('evaloom/params.schema.json')

    await userEvent.click(within(group).getByRole('checkbox', { name: /merge_pull_request/ }))
    const dialog = screen.getByRole('alertdialog', { name: '承認を省く操作を足しますか' })
    expect(dialog).toHaveTextContent('merge_pull_request')
    expect(dialog).toHaveTextContent('PR をマージする')
    expect(dialog).toHaveTextContent('この Project では、この操作を本人の承認なしにスペシャルハーネスが行います。')
    await userEvent.click(within(dialog).getByRole('button', { name: 'やめる' }))
    expect(within(group).getByRole('checkbox', { name: /merge_pull_request/ })).not.toBeChecked()

    await userEvent.click(within(group).getByRole('checkbox', { name: /comment/ }))
    await userEvent.click(screen.getByRole('button', { name: '承認を省く' }))
    expect(within(group).getByRole('checkbox', { name: /comment/ })).toBeChecked()
    await userEvent.click(screen.getByRole('button', { name: '登録' }))
    await screen.findByRole('heading', { name: 'Alpha' })
    expect(body(fake, 'POST', '/api/projects')).toMatchObject({ approvalWaivers: ['comment'] })
  })

  it('版を変えて waivable でない操作が残れば示し、外せる', async () => {
    renderAt('/projects/alpha/edit', () => json(200, { ...PROJECT, approvalWaivers: ['merge_pull_request'] }))
    await screen.findByRole('option', { name: /v2\.0\.0/ })
    await userEvent.selectOptions(screen.getByLabelText('版'), 'profile-2')
    const leftover = screen.getByTestId('leftover-waivers')
    expect(leftover).toHaveTextContent('merge_pull_request')
    await userEvent.click(within(leftover).getByRole('button', { name: '外す' }))
    expect(screen.queryByTestId('leftover-waivers')).toBeNull()
  })

  it('渡す値がスキーマに合わない 422 の内容を入力欄に出す', async () => {
    renderAt('/projects/_new', (r) =>
      r.method === 'POST' ? apiError(422, 'precondition_failed', 'repository validation failed: params (invalid)', [{ field: 'params', reason: 'invalid' }]) : json(200, PROJECT),
    )
    await fillNew('alpha', 'repo:alpha')
    await userEvent.click(screen.getByRole('button', { name: '登録' }))
    const field = (await screen.findByLabelText('渡す値（JSON）')).closest('[data-field]') as HTMLElement
    expect(await within(field).findByText('値が正しくありません')).toBeVisible()
  })

  it('ラベルの登録だけが失敗したら、ラベルが無いことと理由を出して入力を残し、再送はラベルだけ登録する', async () => {
    let routeAttempts = 0
    const fake = renderAt('/projects/_new', (r) => {
      if (r.url === '/api/routes') {
        routeAttempts += 1
        return routeAttempts === 1 ? apiError(409, 'conflict', 'route label or destination already registered') : json(201, {})
      }
      return json(r.method === 'POST' ? 201 : 200, { ...PROJECT, routingLabel: null })
    })
    await fillNew('alpha', 'repo:taken')
    await userEvent.click(screen.getByRole('button', { name: '登録' }))
    const alert = await screen.findByTestId('label-missing')
    expect(alert).toHaveTextContent('振り分けのラベルがありません')
    expect(alert).toHaveTextContent('すでに登録されています')
    expect(screen.getByLabelText('振り分けのラベル')).toHaveValue('repo:taken')
    expect(screen.getByLabelText('Project ID')).toBeDisabled()

    await userEvent.clear(screen.getByLabelText('振り分けのラベル'))
    await userEvent.type(screen.getByLabelText('振り分けのラベル'), 'repo:alpha')
    await userEvent.click(screen.getByRole('button', { name: '保存' }))
    await screen.findByRole('heading', { name: 'Alpha' })
    expect(fake.calls.filter((r) => r.method !== 'GET').map((r) => `${r.method} ${r.url}`)).toEqual([
      'POST /api/projects',
      'POST /api/routes',
      'PATCH /api/projects/alpha',
      'POST /api/routes',
    ])
  })

  it('更新では登録済みのラベルを変えず、ほかの項目を送る', async () => {
    const project = { ...PROJECT, params: { target: 'opaque' }, approvalWaivers: ['comment'] }
    const fake = renderAt('/projects/alpha/edit', () => json(200, project))
    expect(await screen.findByLabelText('振り分けのラベル')).toBeDisabled()
    await userEvent.clear(screen.getByLabelText('名前'))
    await userEvent.type(screen.getByLabelText('名前'), 'Renamed')
    await userEvent.click(screen.getByRole('button', { name: '保存' }))
    await screen.findByRole('heading', { name: 'Alpha' })
    expect(body(fake, 'PATCH', '/api/projects/alpha')).toEqual({ name: 'Renamed', runnerId: 'local', harnessId: 'engineer', profileId: 'profile-1', params: { target: 'opaque' }, approvalWaivers: ['comment'] })
    expect(fake.calls.some((r) => r.url === '/api/routes')).toBe(false)
  })

  it('一覧にラベルを出し、ラベルが無ければその旨を出す', async () => {
    renderAt('/projects', () => json(200, { items: [PROJECT, { ...PROJECT, id: 'beta', name: 'Beta', routingLabel: null }], nextCursor: '' }))
    const cells = await screen.findAllByTestId('routing-label')
    expect(cells.map((c) => c.textContent)).toEqual(['repo:alpha', 'ラベルなし'])
  })

  it('詳細にラベルとタスクの一覧へのリンクを出す', async () => {
    renderAt('/projects/alpha')
    expect(await screen.findByTestId('routing-label')).toHaveTextContent('repo:alpha')
    expect(screen.getByRole('link', { name: 'この Project のタスク' })).toHaveAttribute('href', '/tasks?projectId=alpha')
  })

  it('一覧 API のページを最後まで読む', async () => {
    const fake = fakeFetch((r) => json(200, r.url.includes('cursor=') ? { items: [{ ...PROJECT, id: 'b' }], nextCursor: '' } : { items: [{ ...PROJECT, id: 'a' }], nextCursor: 'next+1' }))
    expect((await registry.listProjects(new ApiClient(fake.fetch))).map((p) => p.id)).toEqual(['a', 'b'])
  })
})
