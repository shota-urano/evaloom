import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it } from 'vitest'
import { ApiClient } from '@/api/client'
import { apiError, fakeFetch, json, type Recorded } from '@/test/fake-fetch'
import App from '@/App'
import { validateRoute } from './labels'

const T = '2026-11-19T00:00:00Z'
const PROJECTS = { items: [{ id: 'alpha', name: 'Alpha', runnerId: 'local', harnessId: 'engineer', profileId: 'p', params: {}, approvalWaivers: [], createdAt: T, updatedAt: T }], nextCursor: '' }
const HARNESSES = {
  items: [
    { id: 'engineer', name: 'Engineer', repoPath: '/r', repoUrl: '', defaultProfileId: 'p', approvalWaivers: [], createdAt: T, updatedAt: T },
    { id: 'writer', name: 'Writer', repoPath: '/w', repoUrl: '', defaultProfileId: null, approvalWaivers: [], createdAt: T, updatedAt: T },
  ],
  nextCursor: '',
}
const ROUTES = {
  items: [
    { label: 'repo:alpha', projectId: 'alpha', harnessId: null, createdAt: T, updatedAt: T },
    { label: 'repo:write', projectId: null, harnessId: 'writer', createdAt: T, updatedAt: T },
  ],
  nextCursor: '',
}
const SETTINGS = { linear: { triggerLabel: 'ai-ready', routingLabelPrefix: 'repo:' } }

function renderAt(handler: (request: Recorded) => Response | undefined = () => undefined) {
  window.history.pushState(null, '', '/routes')
  const fake = fakeFetch((r) => {
    const answer = handler(r)
    if (answer) return answer
    if (r.url === '/api/session') return json(200, { csrfToken: 't' })
    if (r.url.startsWith('/api/projects')) return json(200, PROJECTS)
    if (r.url.startsWith('/api/harnesses')) return json(200, HARNESSES)
    if (r.url === '/api/settings') return json(200, SETTINGS)
    if (r.url.startsWith('/api/routes') && r.method === 'GET') return json(200, ROUTES)
    return json(500, {})
  })
  render(<App api={new ApiClient(fake.fetch)} />)
  return fake
}
afterEach(() => window.history.pushState(null, '', '/'))

const writes = (fake: { calls: Recorded[] }) => fake.calls.filter((r) => r.method !== 'GET')

describe('validateRoute', () => {
  it('振り分け先はどちらか一方だけ', () => {
    expect(validateRoute({ label: 'repo:x', projectId: 'a', harnessId: 'b' }, true).destination).toBeUndefined()
    expect(validateRoute({ label: 'repo:x', projectId: '', harnessId: '' }, true).destination).toBeUndefined()
    expect(validateRoute({ label: ' repo:x ', projectId: 'a', harnessId: '' }, true)).toEqual({ errors: {}, label: 'repo:x', destination: { projectId: 'a' } })
    expect(validateRoute({ label: '', projectId: '', harnessId: 'b' }, false).destination).toEqual({ harnessId: 'b' })
    expect(validateRoute({ label: '', projectId: '', harnessId: 'b' }, true).errors.label).toEqual(['入力してください'])
  })
})

describe('振り分けの表（13 3.6）', () => {
  it('一覧にラベル・種類・名前を出し、既定の版が無いスペシャルハーネスを示す', async () => {
    renderAt()
    const rows = await screen.findAllByTestId('route-row')
    expect(rows.map((row) => [...row.querySelectorAll('td')].slice(0, 2).map((td) => td.textContent))).toEqual([
      ['repo:alpha', 'Project'],
      ['repo:write', 'スペシャルハーネス'],
    ])
    expect(within(rows[0]).getByText('Alpha')).toBeVisible()
    expect(within(rows[1]).getByText('既定の版が無いため実行されません')).toBeVisible()
    expect(screen.getByTestId('recheck-note')).toHaveTextContent('次の照会で判定し直します')
  })

  it('両方または両方なしでは送信しない', async () => {
    const fake = renderAt()
    await userEvent.type(await screen.findByLabelText('ラベル'), 'repo:new')
    await userEvent.click(screen.getByRole('button', { name: '登録' }))
    expect(screen.getByText('Project かスペシャルハーネスのどちらか一方を選んでください')).toBeVisible()
    await userEvent.selectOptions(screen.getByLabelText('Project'), 'alpha')
    await userEvent.selectOptions(screen.getByLabelText('スペシャルハーネス'), 'writer')
    expect(screen.getByTestId('no-default-profile')).toBeVisible()
    await userEvent.click(screen.getByRole('button', { name: '登録' }))
    expect(screen.getByText('Project かスペシャルハーネスのどちらか一方だけを選んでください')).toBeVisible()
    expect(writes(fake)).toEqual([])
  })

  it('登録・変更・削除の要求を送る', async () => {
    const fake = renderAt((r) => {
      if (r.method === 'POST') return json(201, { label: 'repo:new', projectId: null, harnessId: 'engineer', createdAt: T, updatedAt: T })
      if (r.method === 'PATCH') return json(200, { label: 'repo:alpha', projectId: null, harnessId: 'engineer', createdAt: T, updatedAt: T })
      if (r.method === 'DELETE') return new Response(null, { status: 204 })
      return undefined
    })
    await userEvent.type(await screen.findByLabelText('ラベル'), 'repo:new')
    await userEvent.selectOptions(screen.getByLabelText('スペシャルハーネス'), 'engineer')
    await userEvent.click(screen.getByRole('button', { name: '登録' }))
    expect(await screen.findByText(/ラベル「repo:new」を登録しました/)).toBeVisible()

    await userEvent.click(screen.getByRole('button', { name: '「repo:alpha」を変更' }))
    expect(screen.getByLabelText('ラベル')).toBeDisabled()
    await userEvent.selectOptions(screen.getByLabelText('Project'), '')
    await userEvent.selectOptions(screen.getByLabelText('スペシャルハーネス'), 'engineer')
    await userEvent.click(screen.getByRole('button', { name: '保存' }))
    expect(await screen.findByText(/振り分け先を変更しました/)).toBeVisible()

    await userEvent.click(screen.getByRole('button', { name: '「repo:write」を削除' }))
    await userEvent.click(within(screen.getByRole('alertdialog')).getByRole('button', { name: '削除' }))
    expect(await screen.findByText(/ラベル「repo:write」を削除しました/)).toBeVisible()

    expect(writes(fake).map((r) => [r.method, r.url, r.body])).toEqual([
      ['POST', '/api/routes', JSON.stringify({ label: 'repo:new', harnessId: 'engineer' })],
      ['PATCH', '/api/routes/repo%3Aalpha', JSON.stringify({ harnessId: 'engineer' })],
      ['DELETE', '/api/routes/repo%3Awrite', undefined],
    ])
  })

  it('409 と 400 の内容を出す', async () => {
    let status = 409
    renderAt((r) =>
      r.method === 'POST'
        ? status === 409
          ? apiError(409, 'conflict', 'route label or destination already registered')
          : apiError(400, 'validation_failed', 'invalid', [{ field: 'label', reason: 'invalid' }])
        : undefined,
    )
    await userEvent.type(await screen.findByLabelText('ラベル'), 'repo:alpha')
    await userEvent.selectOptions(screen.getByLabelText('Project'), 'alpha')
    await userEvent.click(screen.getByRole('button', { name: '登録' }))
    expect(await screen.findByText('ラベルを登録できませんでした')).toBeVisible()
    expect(screen.getByText(/同じラベルか、同じ振り分け先へのラベルがすでに登録されています/)).toBeVisible()

    status = 400
    await userEvent.click(screen.getByRole('button', { name: '登録' }))
    expect(await screen.findByText(/「repo:」で始まるラベルにしてください/)).toBeVisible()
  })
})
