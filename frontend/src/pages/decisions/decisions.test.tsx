import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'

import { ApiClient } from '@/api/client'
import { canContinue, decisionListPath, operationDescription, targetView, withdrawnReasonLabel, type Decision, type DecisionDetail } from '@/api/decisions'
import { fakeEvents } from '@/test/fake-events'
import { apiError, fakeFetch, json, type Recorded } from '@/test/fake-fetch'
import App from '@/App'

const page = <T,>(items: T[]) => json(200, { items, nextCursor: '' })

const ID = '01JE2EDECISION00000000001A'
const EXECUTION = '01JE2EEXECUTION0000000001A'
const PROFILE = '01JE2EPR0F1LE000000000001A'

const decision = (overrides: Partial<DecisionDetail> = {}): DecisionDetail => ({
  id: ID,
  kind: 'blocked',
  taskId: 'task-1',
  executionId: EXECUTION,
  subjectId: null,
  cause: 'check_blocked',
  basis: 'content-abc',
  detail: { message: 'check blocked', summary: '確認が止まった', findings: [{ rule: 'R1', severity: 'error', message: '危ない変更', location: { path: 'a.go', line: 3 } }] },
  status: 'open',
  withdrawnReason: null,
  answer: null,
  answeredAt: null,
  createdAt: '2026-11-19T00:00:00Z',
  ...overrides,
})

const task = { id: 'task-1', projectId: '', harnessId: '', title: 'ログイン画面を直す', body: '本文です', acceptanceCriteria: '', status: 'blocked', source: 'linear', linear: { identifier: 'EV-1', url: 'https://linear.app/x/EV-1' } }

function renderAt(path: string, handler: (request: Recorded) => Response | undefined) {
  window.history.pushState(null, '', path)
  const fake = fakeFetch((request) => {
    if (request.url === '/api/session') return json(200, { csrfToken: 't' })
    if (request.url === '/api/tasks/task-1') return json(200, task)
    const handled = handler(request)
    if (handled) return handled
    if (request.url.startsWith('/api/')) return page([])
    return json(500, {})
  })
  render(<App api={new ApiClient(fake.fetch, fakeEvents().factory)} />)
  return fake
}

const answers = (fake: ReturnType<typeof fakeFetch>) => fake.calls.filter((c) => c.method === 'POST' && c.url.endsWith('/answer')).map((c) => JSON.parse(c.body ?? 'null'))

describe('判断待ちの補助関数', () => {
  it('一覧の URL に状態と種類を付ける', () => {
    expect(decisionListPath('open')).toBe('/api/decisions?status=open')
    expect(decisionListPath('open', 'question')).toBe('/api/decisions?status=open&kind=question')
  })
  it('context_conflict と workspace_missing は続行できない', () => {
    expect(canContinue('check_blocked')).toBe(true)
    expect(canContinue('publish_unknown')).toBe(true)
    expect(canContinue('context_conflict')).toBe(false)
    expect(canContinue('workspace_missing')).toBe(false)
  })
  it('取り下げの理由を文字にする', () => {
    expect(withdrawnReasonLabel({ kind: 'blocked', withdrawnReason: 'cancelled' })).toBe('タスクの中止')
    expect(withdrawnReasonLabel({ kind: 'route', withdrawnReason: 'superseded' })).toBe('Linear で本人が振り分けのラベルを付けた')
    expect(withdrawnReasonLabel({ kind: 'approval', withdrawnReason: 'superseded' })).toBe('新しい判断待ちに置き換えた')
    expect(withdrawnReasonLabel({ kind: 'approval', withdrawnReason: 'expired' })).toBe('期限切れ')
  })
  it('target の label と url、Profile の操作の説明を取り出す', () => {
    expect(targetView({ label: 'PR #1', url: 'https://example.com/1' })).toEqual({ label: 'PR #1', url: 'https://example.com/1' })
    expect(targetView('opaque')).toEqual({ label: null, url: null })
    expect(operationDescription({ approvals: [{ operation: 'merge', description: 'PR をマージする' }] }, 'merge')).toBe('PR をマージする')
    expect(operationDescription(null, 'merge')).toBeNull()
  })
})

describe('判断待ちの一覧', () => {
  it('種類で絞り込むと ?kind= を付けて読み直す', async () => {
    const items: Decision[] = [decision(), decision({ id: '01JE2EDECISION00000000002A', kind: 'question', cause: 'question' })]
    const fake = renderAt('/decisions', (request) => {
      if (request.url.startsWith('/api/decisions?status=open&kind=question')) return page(items.filter((d) => d.kind === 'question'))
      if (request.url.startsWith('/api/decisions?status=open')) return page(items)
      return undefined
    })
    const list = await screen.findByRole('list', { name: '判断待ち' })
    expect(within(list).getAllByRole('listitem')).toHaveLength(2)
    await userEvent.selectOptions(screen.getByLabelText('種類'), 'question')
    await screen.findByRole('link', { name: '作業中の AI の質問' })
    expect(within(screen.getByRole('list', { name: '判断待ち' })).getAllByRole('listitem')).toHaveLength(1)
    expect(fake.calls.some((c) => c.url.startsWith('/api/decisions?status=open&kind=question'))).toBe(true)
  })

  it('取り下げの一覧に理由を文字で出す', async () => {
    renderAt('/decisions', (request) => {
      if (request.url.startsWith('/api/decisions?status=withdrawn')) return page([decision({ status: 'withdrawn', withdrawnReason: 'expired' })])
      return undefined
    })
    await userEvent.click(await screen.findByRole('button', { name: '取り下げ' }))
    expect(await screen.findByText('取り下げの理由: 期限切れ')).toBeVisible()
  })
})

describe('判断待ちの詳細', () => {
  it('check_blocked は確かめた結果を出し、続行の送信に表示中の contentId を含める', async () => {
    const fake = renderAt(`/decisions/${ID}`, (request) => {
      if (request.url === `/api/decisions/${ID}` && request.method === 'GET') return json(200, decision())
      if (request.url === `/api/decisions/${ID}/answer`) return json(200, decision({ status: 'answered' }))
      return undefined
    })
    expect(await screen.findByTestId('blocked-content-id')).toHaveTextContent('content-abc')
    expect(screen.getByText('危ない変更')).toBeVisible()
    await userEvent.click(screen.getByRole('button', { name: '続行する' }))
    expect(answers(fake)).toEqual([{ action: 'continue', contentId: 'content-abc' }])
  })

  it.each(['context_conflict', 'workspace_missing'] as const)('%s では続行ボタンを出さず中止だけ出す', async (cause) => {
    const fake = renderAt(`/decisions/${ID}`, (request) => {
      if (request.url === `/api/decisions/${ID}` && request.method === 'GET') return json(200, decision({ cause, basis: null, detail: { message: '原因の説明' } }))
      if (request.url === `/api/decisions/${ID}/answer`) return json(200, decision({ cause, status: 'answered' }))
      return undefined
    })
    expect(await screen.findByTestId('blocked-message')).toHaveTextContent('原因の説明')
    expect(screen.queryByRole('button', { name: '続行する' })).toBeNull()
    await userEvent.click(screen.getByRole('button', { name: '中止する' }))
    expect(answers(fake)).toEqual([{ action: 'abort' }])
  })

  it('approval は操作・対象・content_id を出し、送信に表示中の contentId と有効時間を含める', async () => {
    const approvalDecision = decision({
      kind: 'approval',
      cause: null,
      basis: 'sha-123',
      subjectId: '01JE2EAPPROVAL00000000001A',
      detail: { operation: 'merge', target: { label: 'PR #7', url: 'https://example.com/pr/7' }, summary: 'マージする', contentId: 'sha-123', requestedStage: 'publish' },
      approval: {
        id: '01JE2EAPPROVAL00000000001A', decisionId: ID, taskId: 'task-1', executionId: EXECUTION, requestedStage: 'publish', operation: 'merge',
        target: { label: 'PR #7', url: 'https://example.com/pr/7' }, summary: 'マージする', contentId: 'sha-123', status: 'pending',
        invalidatedReason: null, decidedAt: null, expiresAt: null, createdAt: '2026-11-19T00:00:00Z',
      },
    })
    const fake = renderAt(`/decisions/${ID}`, (request) => {
      if (request.url === `/api/decisions/${ID}` && request.method === 'GET') return json(200, approvalDecision)
      if (request.url.startsWith('/api/decisions?status=open&kind=approval')) return page([approvalDecision])
      if (request.url === `/api/executions/${EXECUTION}`) return json(200, { id: EXECUTION, profileId: PROFILE })
      if (request.url === `/api/harness-profiles/${PROFILE}`) return json(200, { id: PROFILE, version: '1.0.0', definition: { approvals: [{ operation: 'merge', description: 'PR をマージする' }] } })
      if (request.url === `/api/decisions/${ID}/answer`) return json(200, { ...approvalDecision, status: 'answered' })
      return undefined
    })
    expect(await screen.findByTestId('approval-content-id')).toHaveTextContent('sha-123')
    expect(screen.getByTestId('approval-target')).toHaveTextContent('PR #7')
    expect(await screen.findByText('PR をマージする')).toBeVisible()
    await userEvent.type(screen.getByLabelText(/承認の有効時間/), '3600')
    await userEvent.click(screen.getByRole('button', { name: '承認する' }))
    expect(answers(fake)).toEqual([{ action: 'approve', contentId: 'sha-123', ttlSec: 3600 }])
  })

  it('route は提案と理由、候補を出し、別のラベルを選んで承認できる', async () => {
    const routeDecision = decision({
      kind: 'route', executionId: null, cause: null, basis: '01JE2EPROPOSAL00000000001A', subjectId: '01JE2EPROPOSAL00000000001A', detail: {},
      routeProposal: {
        id: '01JE2EPROPOSAL00000000001A', taskId: 'task-1', trigger: 'multiple_labels', seenLabels: ['repo:a', 'repo:b'],
        candidates: [{ label: 'repo:a', kind: 'project', id: 'a', description: 'A の説明' }, { label: 'repo:b', kind: 'project', id: 'b', description: 'B の説明' }],
        status: 'proposed', runnerId: null, proposedLabel: 'repo:a', reason: 'A が近い', errorClass: null, decisionId: ID, createdAt: '2026-11-19T00:00:00Z', finishedAt: null,
      },
    })
    const fake = renderAt(`/decisions/${ID}`, (request) => {
      if (request.url === `/api/decisions/${ID}` && request.method === 'GET') return json(200, routeDecision)
      if (request.url.startsWith('/api/routes')) return page([{ label: 'repo:a', projectId: 'a', harnessId: null }, { label: 'repo:b', projectId: 'b', harnessId: null }])
      if (request.url === `/api/decisions/${ID}/answer`) return json(200, { ...routeDecision, status: 'answered' })
      return undefined
    })
    expect(await screen.findByTestId('proposed-label')).toHaveTextContent('repo:a')
    expect(screen.getByTestId('proposal-reason')).toHaveTextContent('A が近い')
    expect(await screen.findByText('B の説明')).toBeVisible()
    expect(screen.getByRole('radio', { name: /repo:a/ })).toBeChecked()
    await userEvent.click(screen.getByRole('radio', { name: /repo:b/ }))
    await userEvent.click(screen.getByRole('button', { name: '選んだラベルで承認する' }))
    expect(answers(fake)).toEqual([{ action: 'approve', label: 'repo:b', basis: '01JE2EPROPOSAL00000000001A' }])
  })

  it('question は質問と選択肢を出し、回答を送れる', async () => {
    const fake = renderAt(`/decisions/${ID}`, (request) => {
      if (request.url === `/api/decisions/${ID}` && request.method === 'GET') {
        return json(200, decision({ kind: 'question', cause: 'question', basis: null, detail: { question: '残しますか？', options: ['残す', '消す'] } }))
      }
      if (request.url === `/api/decisions/${ID}/answer`) return json(200, decision({ status: 'answered' }))
      return undefined
    })
    expect(await screen.findByTestId('question-text')).toHaveTextContent('残しますか？')
    await userEvent.click(screen.getByRole('radio', { name: '消す' }))
    await userEvent.type(screen.getByLabelText('回答'), '理由はテストです')
    await userEvent.click(screen.getByRole('button', { name: '回答を送る' }))
    expect(answers(fake)).toEqual([{ action: 'answer', text: '理由はテストです', option: '消す' }])
  })

  it('422 で内容が変わったことを出して読み直させる', async () => {
    let basis = 'content-abc'
    renderAt(`/decisions/${ID}`, (request) => {
      if (request.url === `/api/decisions/${ID}` && request.method === 'GET') return json(200, decision({ basis }))
      if (request.url === `/api/decisions/${ID}/answer`) {
        basis = 'content-new'
        return apiError(422, 'precondition_failed', 'Answer basis does not match the decision')
      }
      return undefined
    })
    await userEvent.click(await screen.findByRole('button', { name: '続行する' }))
    expect(await screen.findByTestId('decision-stale')).toBeVisible()
    expect(screen.getByRole('button', { name: '続行する' })).toBeDisabled()
    await userEvent.click(screen.getByRole('button', { name: '画面を読み直す' }))
    expect(await screen.findByText('content-new')).toBeVisible()
    expect(screen.queryByTestId('decision-stale')).toBeNull()
  })

  it('409 で最新の状態を読み直して出す', async () => {
    let status: Decision['status'] = 'open'
    renderAt(`/decisions/${ID}`, (request) => {
      if (request.url === `/api/decisions/${ID}` && request.method === 'GET') return json(200, decision({ status }))
      if (request.url === `/api/decisions/${ID}/answer`) {
        status = 'answered'
        return apiError(409, 'conflict', 'Decision is already answered or withdrawn')
      }
      return undefined
    })
    await userEvent.click(await screen.findByRole('button', { name: '続行する' }))
    expect(await screen.findByTestId('decision-conflict')).toBeVisible()
    expect(await screen.findByText('判断済み')).toBeVisible()
    expect(screen.queryByRole('button', { name: '続行する' })).toBeNull()
  })

  it('withdrawn の理由を文字で出す', async () => {
    renderAt(`/decisions/${ID}`, (request) => {
      if (request.url === `/api/decisions/${ID}`) return json(200, decision({ status: 'withdrawn', withdrawnReason: 'cancelled' }))
      return undefined
    })
    expect(await screen.findByTestId('withdrawn-reason')).toHaveTextContent('タスクの中止')
    expect(screen.queryByRole('button', { name: '続行する' })).toBeNull()
  })
})
