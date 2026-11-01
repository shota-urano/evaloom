import { render, screen, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { ApiClient } from '@/api/client'
import type { Runner } from '@/api/registry'
import { CONCURRENCY_LIMITS, settingsSource } from '@/api/settings'
import { SessionProvider } from '@/auth/session'
import { fakeEvents } from '@/test/fake-events'
import { fakeFetch, json } from '@/test/fake-fetch'
import App from '@/App'
import { SettingsPage } from './settings-page'

const RUNNERS: Runner[] = [
  { id: 'mac-mini', name: 'Mac mini', status: 'online', lastHeartbeatAt: '2026-11-01T00:00:00Z', createdAt: '2026-10-01T00:00:00Z' },
]

function serve() {
  return fakeFetch((request) => {
    if (request.url === '/api/session') return json(200, { csrfToken: 't' })
    if (request.url.startsWith('/api/runners')) return json(200, { items: RUNNERS, nextCursor: '' })
    return json(500, {})
  })
}

describe('Settings（13 3.2・4節）', () => {
  it('同時実行の上限は仕様の確定値（全体2本・同一プロジェクト1本）', async () => {
    expect(CONCURRENCY_LIMITS).toEqual({ global: 2, perProject: 1 })
    expect(await settingsSource.load()).toEqual({ concurrency: { global: 2, perProject: 1 } })
  })

  it('同時実行の上限を表示し、変更する入力欄や保存ボタンを出さない', async () => {
    window.history.pushState(null, '', '/settings')
    const fake = serve()
    render(<App api={new ApiClient(fake.fetch, fakeEvents().factory)} />)
    expect(await screen.findByRole('heading', { name: 'Settings' })).toBeVisible()
    expect(await screen.findByTestId('concurrency-global')).toHaveTextContent('全体2 本')
    expect(screen.getByTestId('concurrency-per-project')).toHaveTextContent('同一プロジェクト1 本')
    expect(screen.getByText(/画面からは変更できません/)).toBeVisible()
    const main = screen.getByRole('main')
    expect(within(main).queryAllByRole('textbox')).toHaveLength(0)
    expect(within(main).queryAllByRole('spinbutton')).toHaveLength(0)
    expect(within(main).queryAllByRole('combobox')).toHaveLength(0)
    expect(within(main).queryAllByRole('button')).toHaveLength(0)
    // 全体設定を変える要求（PATCH /api/settings）は送らない。
    expect(fake.calls.filter((c) => c.method !== 'GET')).toHaveLength(0)
    expect(await screen.findByText('Mac mini')).toBeVisible()
    expect(screen.getByText('online')).toBeVisible()
  })

  it('値の取り出し方を差し替えると、その値と出どころを表示する', async () => {
    const fake = serve()
    render(
      <SessionProvider api={new ApiClient(fake.fetch, fakeEvents().factory)}>
        <SettingsPage source={{ origin: 'GET /api/settings', load: async () => ({ concurrency: { global: 3, perProject: 2 } }) }} />
      </SessionProvider>,
    )
    expect(await screen.findByTestId('concurrency-global')).toHaveTextContent('全体3 本')
    expect(screen.getByTestId('concurrency-per-project')).toHaveTextContent('同一プロジェクト2 本')
    expect(screen.getByText(/値の出どころ: GET \/api\/settings/)).toBeVisible()
  })
})
