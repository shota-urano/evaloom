import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'

import { ApiClient } from '@/api/client'
import type { Runner } from '@/api/registry'
import { CONCURRENCY_LIMITS, settingsSource } from '@/api/settings'
import { SessionProvider } from '@/auth/session'
import { fakeEvents } from '@/test/fake-events'
import { apiError, fakeFetch, json, type Recorded } from '@/test/fake-fetch'
import App from '@/App'
import { SettingsPage } from './settings-page'

const RUNNERS: Runner[] = [
  { id: 'mac-mini', name: 'Mac mini', status: 'online', lastHeartbeatAt: '2026-11-01T00:00:00Z', createdAt: '2026-10-01T00:00:00Z' },
]

const LINEAR = { linear: { triggerLabel: 'ai-ready', routingLabelPrefix: 'repo:' } }

function serve(handler: (request: Recorded) => Response | undefined = () => undefined) {
  return fakeFetch((request) => {
    if (request.url === '/api/session') return json(200, { csrfToken: 't' })
    const handled = handler(request)
    if (handled) return handled
    if (request.url.startsWith('/api/runners')) return json(200, { items: RUNNERS, nextCursor: '' })
    if (request.url === '/api/settings' && request.method === 'GET') return json(200, LINEAR)
    return json(500, {})
  })
}

function renderSettings(handler?: (request: Recorded) => Response | undefined) {
  window.history.pushState(null, '', '/settings')
  const fake = serve(handler)
  render(<App api={new ApiClient(fake.fetch, fakeEvents().factory)} />)
  return fake
}

describe('Settings（13 3.2・4節）', () => {
  it('同時実行の上限は仕様の確定値（全体2本・同一プロジェクト1本）', async () => {
    expect(CONCURRENCY_LIMITS).toEqual({ global: 2, perProject: 1 })
    expect(await settingsSource.load()).toEqual({ concurrency: { global: 2, perProject: 1 } })
  })

  it('同時実行の上限を表示し、変更する入力欄や保存ボタンを出さない', async () => {
    const fake = renderSettings()
    expect(await screen.findByRole('heading', { name: 'Settings' })).toBeVisible()
    expect(await screen.findByTestId('concurrency-global')).toHaveTextContent('全体2 本')
    expect(screen.getByTestId('concurrency-per-project')).toHaveTextContent('同一プロジェクト1 本')
    expect(screen.getByText(/画面からは変更できません/)).toBeVisible()
    // Linear の取り込みのラベル（15 5章）は編集できるが、同時実行の上限の欄には入力欄もボタンも無い。
    const card = screen.getByTestId('settings-concurrency')
    expect(within(card).queryAllByRole('textbox')).toHaveLength(0)
    expect(within(card).queryAllByRole('spinbutton')).toHaveLength(0)
    expect(within(card).queryAllByRole('combobox')).toHaveLength(0)
    expect(within(card).queryAllByRole('button')).toHaveLength(0)
    // 画面を開いただけでは全体設定を変える要求（PATCH /api/settings）は送らない。
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

  it('Linear のトリガーのラベル名と接頭辞を GET /api/settings から出し、変えた値を PATCH で保存する（15 5章）', async () => {
    const fake = renderSettings((request) =>
      request.method === 'PATCH' ? json(200, { linear: { triggerLabel: 'evaloom', routingLabelPrefix: 'repo:' } }) : undefined,
    )
    const card = await screen.findByTestId('settings-linear')
    const trigger = await within(card).findByLabelText('トリガーのラベル名')
    expect(trigger).toHaveValue('ai-ready')
    expect(within(card).getByLabelText('実行先ラベルの接頭辞')).toHaveValue('repo:')
    await userEvent.clear(trigger)
    await userEvent.type(trigger, 'evaloom')
    await userEvent.click(within(card).getByRole('button', { name: '保存' }))
    expect(await within(card).findByRole('status')).toHaveTextContent('保存しました')
    const patch = fake.calls.find((c) => c.method === 'PATCH')
    expect(patch?.url).toBe('/api/settings')
    expect(JSON.parse(patch?.body ?? '')).toEqual({ linear: { triggerLabel: 'evaloom', routingLabelPrefix: 'repo:' } })
  })

  it('空欄では送らず、400 の details（linear.<key>）は同じ項目に出す', async () => {
    const fake = renderSettings((request) =>
      request.method === 'PATCH' ? apiError(400, 'validation_failed', 'invalid', [{ field: 'linear.routingLabelPrefix', reason: 'required' }]) : undefined,
    )
    const card = await screen.findByTestId('settings-linear')
    const trigger = await within(card).findByLabelText('トリガーのラベル名')
    await userEvent.clear(trigger)
    await userEvent.click(within(card).getByRole('button', { name: '保存' }))
    expect(trigger).toHaveAttribute('aria-invalid', 'true')
    expect(fake.calls.some((c) => c.method === 'PATCH')).toBe(false)

    await userEvent.type(trigger, 'ai-ready')
    await userEvent.click(within(card).getByRole('button', { name: '保存' }))
    const prefix = within(card).getByLabelText('実行先ラベルの接頭辞')
    expect(await within(prefix.closest('[data-field]') as HTMLElement).findByText('入力してください')).toBeVisible()
    expect(within(card).getByRole('alert')).toHaveTextContent('入力内容を確認してください')
    expect(within(card).queryByRole('status')).toBeNull()
  })

  it('GET /api/settings が失敗したら、Linear の欄にエラーを出す', async () => {
    renderSettings((request) => (request.url === '/api/settings' ? json(500, {}) : undefined))
    const card = await screen.findByTestId('settings-linear')
    expect(await within(card).findByRole('alert')).toHaveTextContent('サーバーでエラーが発生しました')
    expect(within(card).queryAllByRole('textbox')).toHaveLength(0)
  })
})
