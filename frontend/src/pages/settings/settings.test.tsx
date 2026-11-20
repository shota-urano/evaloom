import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'

import { ApiClient } from '@/api/client'
import type { Runner } from '@/api/registry'
import { CONCURRENCY_LIMITS, settingsPatchBody, settingsSource, type SettingsPatch } from '@/api/settings'
import { SessionProvider } from '@/auth/session'
import { fakeEvents } from '@/test/fake-events'
import { apiError, fakeFetch, json, type Recorded } from '@/test/fake-fetch'
import App from '@/App'
import { SettingsPage } from './settings-page'

const RUNNERS: Runner[] = [
  { id: 'mac-mini', name: 'Mac mini', status: 'online', lastHeartbeatAt: '2026-11-01T00:00:00Z', createdAt: '2026-10-01T00:00:00Z' },
]

const SETTINGS = {
  linear: { triggerLabel: 'ai-ready', routingLabelPrefix: 'route:' },
  routing: { proposer: { microHarness: 'claude_code', model: null }, defaultRunnerId: null },
  limits: { stageTimeoutMaxSec: { prepare: 1800, work: 14400, check: 3600, publish: 1800 } },
  approval: { ttlSec: 86400 },
}

function serve(handler: (request: Recorded) => Response | undefined = () => undefined) {
  return fakeFetch((request) => {
    if (request.url === '/api/session') return json(200, { csrfToken: 't' })
    const handled = handler(request)
    if (handled) return handled
    if (request.url.startsWith('/api/runners')) return json(200, { items: RUNNERS, nextCursor: '' })
    if (request.url === '/api/settings' && request.method === 'GET') return json(200, SETTINGS)
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
    expect(screen.getByTestId('concurrency-per-project')).toHaveTextContent('振り分け先ごと1 本')
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
    expect(screen.getByTestId('concurrency-per-project')).toHaveTextContent('振り分け先ごと2 本')
    expect(screen.getByText(/値の出どころ: GET \/api\/settings/)).toBeVisible()
  })

  it('02 3.8 の6キーを GET /api/settings から表示する', async () => {
    renderSettings()
    const form = await screen.findByRole('form', { name: '全体設定' })
    expect(await within(form).findByLabelText(/linear\.triggerLabel/)).toHaveValue('ai-ready')
    expect(within(form).getByLabelText(/linear\.routingLabelPrefix/)).toHaveValue('route:')
    expect(within(form).getByLabelText(/提案に使うミクロハーネス/)).toHaveValue('claude_code')
    expect(within(form).getByLabelText(/提案に使うモデル/)).toHaveValue('')
    expect(within(form).getByLabelText(/routing\.defaultRunnerId/)).toHaveValue('')
    expect(within(form).getByLabelText('prepare（秒）')).toHaveValue('1800')
    expect(within(form).getByLabelText('work（秒）')).toHaveValue('14400')
    expect(within(form).getByLabelText('check（秒）')).toHaveValue('3600')
    expect(within(form).getByLabelText('publish（秒）')).toHaveValue('1800')
    expect(within(form).getByLabelText(/approval\.ttlSec/)).toHaveValue('86400')
  })

  it('変えたキーだけを PATCH で送り、置き換えの値（proposer・stageTimeoutMaxSec）は全項目を送る', async () => {
    const fake = renderSettings((request) => (request.method === 'PATCH' ? json(200, { ...SETTINGS, approval: { ttlSec: 7200 } }) : undefined))
    const form = await screen.findByRole('form', { name: '全体設定' })
    await userEvent.type(await within(form).findByLabelText(/提案に使うモデル/), 'claude-sonnet')
    await userEvent.selectOptions(within(form).getByLabelText(/routing\.defaultRunnerId/), 'mac-mini')
    const work = within(form).getByLabelText('work（秒）')
    await userEvent.clear(work)
    await userEvent.type(work, '7200')
    const ttl = within(form).getByLabelText(/approval\.ttlSec/)
    await userEvent.clear(ttl)
    await userEvent.type(ttl, '7200')
    await userEvent.click(within(form).getByRole('button', { name: '保存' }))
    expect(await within(form).findByRole('status')).toHaveTextContent('保存しました')
    const patch = fake.calls.find((c) => c.method === 'PATCH')
    expect(patch?.url).toBe('/api/settings')
    expect(JSON.parse(patch?.body ?? '')).toEqual({
      routing: { proposer: { microHarness: 'claude_code', model: 'claude-sonnet' }, defaultRunnerId: 'mac-mini' },
      limits: { stageTimeoutMaxSec: { prepare: 1800, work: 7200, check: 3600, publish: 1800 } },
      approval: { ttlSec: 7200 },
    })
  })

  it('Runner を未設定に戻すと defaultRunnerId に null を送る', async () => {
    const fake = renderSettings((request) => {
      if (request.url === '/api/settings' && request.method === 'GET') return json(200, { ...SETTINGS, routing: { ...SETTINGS.routing, defaultRunnerId: 'mac-mini' } })
      return request.method === 'PATCH' ? json(200, SETTINGS) : undefined
    })
    const form = await screen.findByRole('form', { name: '全体設定' })
    const runner = await within(form).findByLabelText(/routing\.defaultRunnerId/)
    expect(runner).toHaveValue('mac-mini')
    await userEvent.selectOptions(runner, '')
    await userEvent.click(within(form).getByRole('button', { name: '保存' }))
    expect(await within(form).findByRole('status')).toHaveTextContent('保存しました')
    expect(JSON.parse(fake.calls.find((c) => c.method === 'PATCH')?.body ?? '')).toEqual({ routing: { defaultRunnerId: null } })
  })

  it('何も変えずに保存しても PATCH を送らない', async () => {
    const fake = renderSettings()
    const form = await screen.findByRole('form', { name: '全体設定' })
    await userEvent.click(await within(form).findByRole('button', { name: '保存' }))
    expect(await within(form).findByRole('status')).toHaveTextContent('変更はありません')
    expect(fake.calls.some((c) => c.method === 'PATCH')).toBe(false)
  })

  it('未知のキーは本文に写さない（02 3.8 のキーだけを送る）', () => {
    const tainted = {
      linear: { triggerLabel: 'go', teamId: 'uslab' },
      routing: { proposer: { microHarness: 'codex', model: null, extra: 1 }, other: 'x' },
      limits: { stageTimeoutMaxSec: { prepare: 1, work: 2, check: 3, publish: 4, plan: 5 }, foo: 1 },
      approval: { ttlSec: 60, bar: 2 },
      concurrency: { global: 3 },
    } as unknown as SettingsPatch
    expect(settingsPatchBody(tainted)).toEqual({
      linear: { triggerLabel: 'go' },
      routing: { proposer: { microHarness: 'codex', model: null } },
      limits: { stageTimeoutMaxSec: { prepare: 1, work: 2, check: 3, publish: 4 } },
      approval: { ttlSec: 60 },
    })
  })

  it('空欄と正の整数でない値は送らず、400 の details は同じ項目に出す', async () => {
    const fake = renderSettings((request) =>
      request.method === 'PATCH' ? apiError(400, 'validation_failed', 'invalid', [{ field: 'linear.routingLabelPrefix', reason: 'required' }]) : undefined,
    )
    const form = await screen.findByRole('form', { name: '全体設定' })
    const trigger = await within(form).findByLabelText(/linear\.triggerLabel/)
    await userEvent.clear(trigger)
    const ttl = within(form).getByLabelText(/approval\.ttlSec/)
    await userEvent.clear(ttl)
    await userEvent.type(ttl, '0')
    await userEvent.click(within(form).getByRole('button', { name: '保存' }))
    expect(trigger).toHaveAttribute('aria-invalid', 'true')
    expect(ttl).toHaveAttribute('aria-invalid', 'true')
    expect(within(form).getByText('1 以上の整数を入力してください')).toBeVisible()
    expect(fake.calls.some((c) => c.method === 'PATCH')).toBe(false)

    await userEvent.type(trigger, 'ai-ready-2')
    await userEvent.clear(ttl)
    await userEvent.type(ttl, '60')
    await userEvent.click(within(form).getByRole('button', { name: '保存' }))
    const prefix = within(form).getByLabelText(/linear\.routingLabelPrefix/)
    expect(await within(prefix.closest('[data-field]') as HTMLElement).findByText('入力してください')).toBeVisible()
    expect(within(form).getByRole('alert')).toHaveTextContent('入力内容を確認してください')
    expect(within(form).queryByRole('status')).toBeNull()
  })

  it('起動の合図のラベルが接頭辞で始まる 400（Q160）を、起動の合図のラベルの欄に出す', async () => {
    renderSettings((request) =>
      request.method === 'PATCH'
        ? apiError(400, 'validation_failed', 'invalid', [{ field: 'linear.triggerLabel', reason: 'starts_with_routing_label_prefix' }])
        : undefined,
    )
    const form = await screen.findByRole('form', { name: '全体設定' })
    const trigger = await within(form).findByLabelText(/linear\.triggerLabel/)
    await userEvent.clear(trigger)
    await userEvent.type(trigger, 'route:go')
    await userEvent.click(within(form).getByRole('button', { name: '保存' }))
    expect(await within(trigger.closest('[data-field]') as HTMLElement).findByText(/起動の合図のラベルが振り分けのラベルの接頭辞で始まっています/)).toBeVisible()
    expect(trigger).toHaveAttribute('aria-invalid', 'true')
    expect(within(form).getByRole('alert')).toHaveTextContent('入力内容を確認してください')
    // 保存されていないので、入力した値はそのまま残る。
    expect(trigger).toHaveValue('route:go')
    expect(within(form).queryByRole('status')).toBeNull()
  })

  it('振り分けの表のラベルが新しい接頭辞で始まらない 422（Q160）を、接頭辞の欄とフォーム上部に出す', async () => {
    const fake = renderSettings((request) =>
      request.method === 'PATCH'
        ? apiError(422, 'precondition_failed', 'routes labels must start with the routing label prefix', [
            { field: 'routes.route:web', reason: 'outside_routing_label_prefix' },
            { field: 'routes.route:ops', reason: 'outside_routing_label_prefix' },
          ])
        : undefined,
    )
    const form = await screen.findByRole('form', { name: '全体設定' })
    const prefix = await within(form).findByLabelText(/linear\.routingLabelPrefix/)
    await userEvent.clear(prefix)
    await userEvent.type(prefix, 'to:')
    await userEvent.click(within(form).getByRole('button', { name: '保存' }))
    expect(JSON.parse(fake.calls.find((c) => c.method === 'PATCH')?.body ?? '')).toEqual({ linear: { routingLabelPrefix: 'to:' } })
    const field = prefix.closest('[data-field]') as HTMLElement
    expect(await within(field).findByText('振り分けの表のラベル「route:web」、「route:ops」がこの接頭辞で始まりません')).toBeVisible()
    expect(prefix).toHaveAttribute('aria-invalid', 'true')
    const alert = within(form).getByRole('alert')
    expect(alert).toHaveTextContent('前提条件が満たされていません')
    expect(alert).toHaveTextContent('どの項目も変わっていません')
    expect(alert).toHaveTextContent('振り分けの表のラベル「route:web」: 振り分けのラベルの接頭辞で始まりません')
    expect(alert).toHaveTextContent('振り分けの表のラベル「route:ops」')
    expect(within(form).getByRole('link', { name: '振り分けの表を開く' })).toHaveAttribute('href', '/routes')
    expect(within(form).queryByRole('status')).toBeNull()
  })

  it('GET /api/settings が失敗したら、全体設定の欄にエラーを出す', async () => {
    renderSettings((request) => (request.url === '/api/settings' ? json(500, {}) : undefined))
    const card = await screen.findByTestId('settings-global')
    expect(await within(card).findByRole('alert')).toHaveTextContent('サーバーでエラーが発生しました')
    expect(within(card).queryAllByRole('textbox')).toHaveLength(0)
  })
})
