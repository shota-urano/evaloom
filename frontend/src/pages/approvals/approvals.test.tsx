import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'

import type { Approval, ApprovalDetail } from '@/api/approvals'
import { isExpired } from '@/api/approvals'
import { ApiClient } from '@/api/client'
import { fakeEvents } from '@/test/fake-events'
import { apiError, fakeFetch, json, type Recorded } from '@/test/fake-fetch'
import App from '@/App'

const page = <T,>(items: T[]) => json(200, { items, nextCursor: '' })

const HEAD = '1234567890abcdef1234567890abcdef12345678'
const ID = '01JE2EAPPROVAL000000000001'
const EXECUTION = '01JE2EEXECUTION00000000001'

const approval = (overrides: Partial<Approval> = {}): Approval => ({
  id: ID,
  taskId: 'task-001',
  executionId: EXECUTION,
  operation: 'merge_pull_request',
  target: { repository: 'example/repo', prNumber: 12, baseBranch: 'main', headBranch: 'evaloom/task-001-login' },
  contentVersion: HEAD,
  paramsHash: 'f'.repeat(64),
  status: 'pending',
  invalidatedReason: null,
  decidedBy: null,
  decidedAt: null,
  expiresAt: null,
  maxUses: 1,
  useCount: 0,
  consumedAt: null,
  result: null,
  createdAt: '2026-11-08T00:00:00Z',
  ...overrides,
})

const detail = (overrides: Partial<ApprovalDetail> = {}): ApprovalDetail => ({
  ...approval(),
  ...overrides,
})

function renderAt(path: string, handler: (request: Recorded) => Response | undefined) {
  window.history.pushState(null, '', path)
  const fake = fakeFetch((request) => {
    if (request.url === '/api/session') return json(200, { csrfToken: 't' })
    const handled = handler(request)
    if (handled) return handled
    if (request.url.startsWith('/api/')) return page([])
    return json(500, {})
  })
  render(<App api={new ApiClient(fake.fetch, fakeEvents().factory)} />)
  return fake
}

describe('Approvals 一覧', () => {
  it('承認待ちと履歴を分けて表示し、詳細へのリンクを出す', async () => {
    const fake = renderAt('/approvals', (request) => {
      if (request.url.startsWith('/api/approvals?status=pending')) return page([approval()])
      if (request.url.startsWith('/api/approvals?')) {
        return page([approval(), approval({ id: '01JE2EAPPROVAL000000000000', status: 'rejected', decidedAt: '2026-11-07T00:00:00Z', decidedBy: 'owner' })])
      }
      return undefined
    })
    const pending = await screen.findByRole('table', { name: '承認待ち' })
    expect(within(pending).getByRole('link', { name: 'example/repo#12' })).toHaveAttribute('href', `/approvals/${ID}`)
    expect(within(pending).getByText(HEAD.slice(0, 12))).toBeVisible()
    const history = await screen.findByRole('table', { name: '履歴' })
    // 履歴には pending を出さない。
    expect(within(history).getAllByRole('row')).toHaveLength(2)
    expect(within(history).getByText('rejected')).toBeVisible()
    expect(fake.calls.some((c) => c.url.startsWith('/api/approvals?status=pending'))).toBe(true)
  })

  it('履歴を状態で絞り込むと ?status= を付けて読み直す', async () => {
    const fake = renderAt('/approvals', () => undefined)
    await screen.findByText('履歴はありません。')
    await userEvent.selectOptions(screen.getByLabelText('状態'), 'expired')
    await screen.findByText('条件に合う承認はありません。')
    expect(fake.calls.some((c) => c.url.startsWith('/api/approvals?status=expired'))).toBe(true)
    expect(window.location.search).toBe('?status=expired')
  })
})

describe('承認の詳細', () => {
  it('対象と head SHA を表示し、Gate の結果と差分は出さない', async () => {
    renderAt(`/approvals/${ID}`, (request) => {
      if (request.url === `/api/approvals/${ID}`) return json(200, detail())
      return undefined
    })
    expect(await screen.findByTestId('approval-pr-url')).toHaveTextContent('example/repo#12')
    expect(screen.getByTestId('approval-content-version')).toHaveTextContent(HEAD)
    expect(screen.queryByRole('heading', { name: 'Gate の結果' })).toBeNull()
    expect(screen.queryByRole('heading', { name: '差分' })).toBeNull()
  })

  it('承認の送信に表示中の contentVersion を含める', async () => {
    let decided = false
    const fake = renderAt(`/approvals/${ID}`, (request) => {
      if (request.url === `/api/approvals/${ID}/decision`) {
        decided = true
        return json(200, detail({ status: 'approved' }))
      }
      if (request.url === `/api/approvals/${ID}`) return json(200, detail(decided ? { status: 'approved' } : {}))
      return undefined
    })
    await userEvent.click(await screen.findByRole('button', { name: '承認する' }))
    expect(await screen.findByRole('status')).toHaveTextContent('承認を受け付けました。')
    const call = fake.calls.find((c) => c.url === `/api/approvals/${ID}/decision`)
    expect(call?.method).toBe('POST')
    expect(JSON.parse(call?.body ?? '{}')).toEqual({ decision: 'approve', contentVersion: HEAD })
    expect(call?.headers['X-CSRF-Token']).toBe('t')
    await screen.findByText('approved')
    expect(screen.queryByRole('button', { name: '承認する' })).toBeNull()
  })

  it('却下の送信にも表示中の contentVersion を含める', async () => {
    const fake = renderAt(`/approvals/${ID}`, (request) => {
      if (request.url === `/api/approvals/${ID}/decision`) return json(200, detail({ status: 'rejected' }))
      if (request.url === `/api/approvals/${ID}`) return json(200, detail())
      return undefined
    })
    await userEvent.click(await screen.findByRole('button', { name: '却下する' }))
    expect(await screen.findByRole('status')).toHaveTextContent('却下を受け付けました。')
    const call = fake.calls.find((c) => c.url === `/api/approvals/${ID}/decision`)
    expect(JSON.parse(call?.body ?? '{}')).toEqual({ decision: 'reject', contentVersion: HEAD })
  })

  it('422 で内容が変わったことを表示し、読み直すと新しい head SHA を出す', async () => {
    const changed = 'fedcba0987654321fedcba0987654321fedcba09'
    let reads = 0
    renderAt(`/approvals/${ID}`, (request) => {
      if (request.url === `/api/approvals/${ID}/decision`) {
        return apiError(422, 'precondition_failed', "contentVersion must equal the approval's content version")
      }
      if (request.url === `/api/approvals/${ID}`) {
        reads++
        return json(200, detail(reads > 1 ? { contentVersion: changed } : {}))
      }
      return undefined
    })
    await userEvent.click(await screen.findByRole('button', { name: '承認する' }))
    const stale = await screen.findByTestId('approval-stale')
    expect(stale).toHaveTextContent('内容が変わりました')
    expect(screen.getByRole('button', { name: '承認する' })).toBeDisabled()
    await userEvent.click(within(stale).getByRole('button', { name: '画面を読み直す' }))
    expect(await screen.findByText(changed)).toBeVisible()
    expect(screen.queryByTestId('approval-stale')).toBeNull()
    expect(screen.getByRole('button', { name: '承認する' })).toBeEnabled()
  })

  it('409 は状態が変わったことを表示して読み直させる', async () => {
    renderAt(`/approvals/${ID}`, (request) => {
      if (request.url === `/api/approvals/${ID}/decision`) return apiError(409, 'conflict', 'Approval is already decided, invalidated or expired')
      if (request.url === `/api/approvals/${ID}`) return json(200, detail())
      return undefined
    })
    await userEvent.click(await screen.findByRole('button', { name: '承認する' }))
    const alert = await screen.findByTestId('api-error')
    expect(alert).toHaveTextContent('状態が変わりました')
    expect(within(alert).getByRole('button', { name: '最新を読み直す' })).toBeVisible()
  })

  it('期限切れの承認では再承認を求める操作を出し、新しい承認の詳細へ移る', async () => {
    const renewed = '01JE2EAPPROVAL000000000002'
    const fake = renderAt(`/approvals/${ID}`, (request) => {
      if (request.url === `/api/approvals/${ID}/renew`) return json(201, detail({ id: renewed }))
      if (request.url === `/api/approvals/${ID}`) return json(200, detail({ status: 'expired', decidedAt: '2026-11-07T00:00:00Z' }))
      if (request.url === `/api/approvals/${renewed}`) return json(200, detail({ id: renewed }))
      return undefined
    })
    expect(screen.queryByRole('button', { name: '承認する' })).toBeNull()
    await userEvent.click(await screen.findByRole('button', { name: '再承認を求める' }))
    await screen.findByText(renewed)
    expect(window.location.pathname).toBe(`/approvals/${renewed}`)
    expect(fake.calls.find((c) => c.url === `/api/approvals/${ID}/renew`)?.method).toBe('POST')
  })
})

describe('isExpired', () => {
  it('expired と、期限を過ぎた未使用の approved を期限切れとする', () => {
    const now = Date.parse('2026-11-08T12:00:00Z')
    expect(isExpired(approval({ status: 'expired' }), now)).toBe(true)
    expect(isExpired(approval({ status: 'approved', expiresAt: '2026-11-08T11:00:00Z' }), now)).toBe(true)
    expect(isExpired(approval({ status: 'approved', expiresAt: '2026-11-08T13:00:00Z' }), now)).toBe(false)
    expect(isExpired(approval({ status: 'approved', expiresAt: '2026-11-08T11:00:00Z', useCount: 1 }), now)).toBe(false)
    expect(isExpired(approval({ status: 'pending' }), now)).toBe(false)
  })
})

describe('Dashboard の承認待ち', () => {
  it('承認待ちの件数と詳細へのリンクを出す', async () => {
    const fake = renderAt('/', (request) => {
      if (request.url.startsWith('/api/approvals?status=pending')) {
        return page([approval(), approval({ id: '01JE2EAPPROVAL000000000003', target: { repository: 'example/other', prNumber: 3 } })])
      }
      if (request.url === '/api/linear/status') return json(200, { teams: [], issues: [] })
      if (request.url.startsWith('/api/notification-failures')) return json(200, { items: [], nextCursor: null })
      return undefined
    })
    expect(await screen.findByTestId('approval-count-pending')).toHaveTextContent('承認待ち2件')
    const list = screen.getByRole('list', { name: '承認待ち' })
    expect(within(list).getByRole('link', { name: 'example/repo#12' })).toHaveAttribute('href', `/approvals/${ID}`)
    expect(within(list).getByRole('link', { name: 'example/other#3' })).toBeVisible()
    expect(fake.calls.some((c) => c.url.startsWith('/api/approvals?status=pending'))).toBe(true)
  })
})
