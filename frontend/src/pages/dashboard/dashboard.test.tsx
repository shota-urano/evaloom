import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import { ApiClient } from '@/api/client'
import type { Project, Runner } from '@/api/registry'
import type { TaskSummary } from '@/api/tasks'
import type { NotificationFailure } from '@/api/notification-failures'
import type { LinearStatus } from '@/api/linear'
import { fakeEvents } from '@/test/fake-events'
import { apiError, fakeFetch, json, type Recorded } from '@/test/fake-fetch'
import App from '@/App'
import { summarize } from './summary'

const page = <T,>(items: T[]) => json(200, { items, nextCursor: '' })

const RUNNERS: Runner[] = [
  { id: 'mac-mini', name: 'Mac mini', status: 'online', lastHeartbeatAt: '2026-11-01T00:00:00Z', createdAt: '2026-10-01T00:00:00Z' },
  { id: 'spare', name: '予備機', status: 'offline', lastHeartbeatAt: null, createdAt: '2026-10-01T00:00:00Z' },
]

const PROJECTS = [{ id: 'alpha', name: 'Alpha', harnesses: [] }] as unknown as Project[]

const task = (id: string, status: TaskSummary['status'], executionStatus: string, errorClass: string | null = null): TaskSummary => ({
  id,
  projectId: 'alpha',
  title: `件名 ${id}`,
  body: '本文',
  acceptanceCriteria: '条件',
  shortName: 'task',
  taskType: 'development',
  collaborationMode: 'solo',
  status,
  createdAt: '2026-10-27T00:00:00Z',
  updatedAt: '2026-10-27T00:00:00Z',
  latestExecution: { id: `e-${id}`, seq: 1, mode: 'continue', status: executionStatus, errorClass },
})

const TASKS = [
  task('task-001', 'running', 'running'),
  task('task-002', 'verifying', 'verifying'),
  task('task-003', 'queued', 'pending'),
  task('task-004', 'blocked', 'blocked'),
  task('task-005', 'failed', 'failed', 'test_failed'),
  task('task-006', 'completed', 'succeeded'),
  task('task-007', 'cancelled', 'cancelled'),
]

const failure = (eventId: number, taskId: string, reason = 'permission_denied', kind = 'succeeded'): NotificationFailure => ({
  eventId,
  executionId: `e-${taskId}`,
  taskId,
  taskTitle: `件名 ${taskId}`,
  kind,
  channel: 'macos',
  reason,
  createdAt: '2026-11-01T00:00:00Z',
})

/** linear_outbox の行から返す項目（12 5、16 3.4.2、Q82）。eventId と executionId が null で outboxId が入る。 */
const outboxFailure = (outboxId: string, taskId: string, reason = 'auth', kind = 'intake_blocked', createdAt = '2026-11-01T00:00:00Z'): NotificationFailure => ({
  eventId: null,
  executionId: null,
  outboxId,
  taskId,
  taskTitle: `件名 ${taskId}`,
  kind,
  channel: 'linear',
  reason,
  createdAt,
})

/** GET /api/notification-failures の応答（12 5。最後のページの nextCursor は null）。 */
const failurePage = (items: NotificationFailure[], nextCursor: string | null = null) => json(200, { items, nextCursor })

function renderDashboard(handler: (request: Recorded) => Response | undefined = () => undefined) {
  window.history.pushState(null, '', '/')
  const fake = fakeFetch((request) => {
    if (request.url === '/api/session') return json(200, { csrfToken: 't' })
    const handled = handler(request)
    if (handled) return handled
    if (request.url.startsWith('/api/runners')) return page(RUNNERS)
    if (request.url.startsWith('/api/projects')) return page(PROJECTS)
    if (request.url.startsWith('/api/tasks')) return page(TASKS)
    if (request.url.startsWith('/api/notification-failures')) return failurePage([])
    if (request.url === '/api/linear/status') return json(200, { teams: [], issues: [] })
    if (request.url.startsWith('/api/approvals')) return page([])
    return json(500, {})
  })
  render(<App api={new ApiClient(fake.fetch, fakeEvents().factory)} />)
  return fake
}

describe('summarize', () => {
  it('Runner を online / offline で数え、タスクを実行中・待機中・Blocked・失敗に分ける', () => {
    const summary = summarize(RUNNERS, TASKS)
    expect(summary.runners).toEqual({ online: 1, offline: 1 })
    expect(summary.tasks.running.map((t) => t.id)).toEqual(['task-001', 'task-002'])
    expect(summary.tasks.waiting.map((t) => t.id)).toEqual(['task-003'])
    expect(summary.tasks.blocked.map((t) => t.id)).toEqual(['task-004'])
    expect(summary.tasks.error.map((t) => t.id)).toEqual(['task-005'])
  })

  it('timed_out は失敗に数え、received・completed・cancelled はどこにも数えない', () => {
    const summary = summarize([], [task('a', 'timed_out', 'timed_out'), task('b', 'received', 'pending'), task('c', 'completed', 'succeeded')])
    expect(summary.tasks.error.map((t) => t.id)).toEqual(['a'])
    expect(summary.tasks.running.length + summary.tasks.waiting.length + summary.tasks.blocked.length).toBe(0)
  })
})

describe('Dashboard（13 3.2）', () => {
  it('Runner の online / offline と、実行中・待機中・Blocked の件数を表示する', async () => {
    const fake = renderDashboard()
    expect(await screen.findByRole('heading', { name: 'Dashboard' })).toBeVisible()
    expect(await screen.findByTestId('runner-count-online')).toHaveTextContent('online1件')
    expect(screen.getByTestId('runner-count-offline')).toHaveTextContent('offline1件')
    expect(screen.getByTestId('task-count-running')).toHaveTextContent('実行中2件')
    expect(screen.getByTestId('task-count-waiting')).toHaveTextContent('待機中1件')
    expect(screen.getByTestId('task-count-blocked')).toHaveTextContent('Blocked1件')
    expect(screen.getByTestId('task-count-error')).toHaveTextContent('失敗1件')

    const runnerRows = screen.getAllByRole('row').filter((row) => row.hasAttribute('data-runner'))
    expect(runnerRows.map((row) => within(row).getByText(/^(online|offline)$/).textContent)).toEqual(['online', 'offline'])
    expect(within(screen.getByRole('list', { name: 'Blocked のタスク' })).getByRole('link', { name: '件名 task-004' })).toHaveAttribute('href', '/tasks/task-004')
    expect(within(screen.getByRole('list', { name: '失敗したタスク' })).getByText(/test_failed/)).toBeVisible()
    expect(fake.calls.some((c) => c.url.startsWith('/api/runners'))).toBe(true)
    expect(fake.calls.some((c) => c.url.startsWith('/api/tasks'))).toBe(true)
  })

  it('タスクも Runner も無ければ 0 件と空の案内を出す', async () => {
    renderDashboard((request) => (request.url.startsWith('/api/runners') || request.url.startsWith('/api/tasks') ? page([]) : undefined))
    expect(await screen.findByTestId('task-count-running')).toHaveTextContent('実行中0件')
    expect(screen.getByText('Runner は登録されていません。', { exact: false })).toBeVisible()
    expect(screen.getByText('Blocked のタスクはありません。')).toBeVisible()
  })

  it('API のエラーを表示し、再読み込みで取り直す', async () => {
    let fail = true
    const fake = renderDashboard((request) => (fail && request.url.startsWith('/api/runners') ? apiError(403, 'forbidden', 'x') : undefined))
    expect(await screen.findByTestId('api-error')).toBeVisible()
    fail = false
    const before = fake.calls.length
    await userEvent.click(screen.getByRole('button', { name: '再読み込み' }))
    expect(await screen.findByTestId('runner-count-online')).toHaveTextContent('online1件')
    expect(fake.calls.length).toBeGreaterThan(before)
  })

  it('完了したタスクの通知失敗を、通知失敗の一覧 API から読んでエラー状況に表示する', async () => {
    const fake = renderDashboard((request) =>
      request.url === '/api/notification-failures' ? failurePage([failure(7, 'task-006', 'permission_denied', 'succeeded')]) : undefined,
    )
    const failures = await screen.findByRole('list', { name: '通知失敗' })
    const item = within(failures).getByRole('listitem')
    expect(within(item).getByRole('link', { name: '件名 task-006' })).toHaveAttribute('href', '/tasks/task-006')
    expect(item).toHaveTextContent('通知が許可されていない')
    expect(item).toHaveTextContent('reason: permission_denied')
    expect(item).toHaveTextContent('kind: succeeded')
    expect(item).toHaveTextContent('channel: macos')
    expect(item).toHaveTextContent('e-task-006')
    expect(screen.getByTestId('notification-failure-count')).toHaveTextContent('通知失敗1件')
    expect(screen.queryByRole('button', { name: 'さらに表示' })).toBeNull()
    // Execution ごとの SSE は読まない。limit は送らずサーバーの既定に任せる。
    expect(fake.calls.some((c) => c.url.endsWith('/events'))).toBe(false)
    expect(fake.calls.filter((c) => c.url.startsWith('/api/notification-failures')).map((c) => c.url)).toEqual(['/api/notification-failures'])
  })

  it('nextCursor があれば「さらに表示」で続きを読み、51 件目も欠けず重複せずに表示する', async () => {
    const all = Array.from({ length: 51 }, (_, i) => failure(1000 - i, `task-${String(i + 1).padStart(3, '0')}`, i === 50 ? 'timeout' : 'permission_denied'))
    const fake = renderDashboard((request) => {
      if (request.url === '/api/notification-failures') return failurePage(all.slice(0, 50), '951')
      if (request.url === '/api/notification-failures?cursor=951') return failurePage(all.slice(50), null)
      return undefined
    })
    const failures = await screen.findByRole('list', { name: '通知失敗' })
    expect(within(failures).getAllByRole('listitem')).toHaveLength(50)
    expect(within(failures).queryByRole('link', { name: '件名 task-051' })).toBeNull()
    expect(screen.getByTestId('notification-failure-count')).toHaveTextContent('通知失敗50件以上')

    await userEvent.click(screen.getByRole('button', { name: 'さらに表示' }))
    const last = await within(failures).findByRole('link', { name: '件名 task-051' })
    expect(last.closest('li')).toHaveTextContent('通知ヘルパーの結果待ちが時間切れ')
    const titles = within(failures)
      .getAllByRole('link')
      .map((link) => link.textContent)
    expect(titles).toEqual(all.map((f) => f.taskTitle))
    expect(screen.getByTestId('notification-failure-count')).toHaveTextContent('通知失敗51件')
    expect(screen.queryByRole('button', { name: 'さらに表示' })).toBeNull()
    expect(fake.calls.filter((c) => c.url.startsWith('/api/notification-failures')).map((c) => c.url)).toEqual([
      '/api/notification-failures',
      '/api/notification-failures?cursor=951',
    ])
  })

  it('再読み込みで通知失敗を1ページ目から読み直し、足した続きは捨てる', async () => {
    let first = [failure(3, 'task-006'), failure(2, 'task-005')]
    renderDashboard((request) => {
      if (request.url === '/api/notification-failures') return failurePage(first, '2')
      if (request.url === '/api/notification-failures?cursor=2') return failurePage([failure(1, 'task-004')], null)
      return undefined
    })
    const failures = await screen.findByRole('list', { name: '通知失敗' })
    await userEvent.click(screen.getByRole('button', { name: 'さらに表示' }))
    expect(await within(failures).findByRole('link', { name: '件名 task-004' })).toBeVisible()

    first = [failure(4, 'task-001'), failure(3, 'task-006')]
    await userEvent.click(screen.getByRole('button', { name: '再読み込み' }))
    expect(await within(screen.getByTestId('notification-failures')).findByRole('link', { name: '件名 task-001' })).toBeVisible()
    expect(within(screen.getByRole('list', { name: '通知失敗' })).getAllByRole('link').map((l) => l.textContent)).toEqual(['件名 task-001', '件名 task-006'])
  })

  it('続きの読み込みに失敗したらエラーを出し、読めた分は残す', async () => {
    renderDashboard((request) => {
      if (request.url === '/api/notification-failures') return failurePage([failure(2, 'task-006')], '2')
      if (request.url.startsWith('/api/notification-failures?cursor=')) return apiError(400, 'validation_failed', 'bad cursor', [{ field: 'cursor', reason: 'invalid' }])
      return undefined
    })
    const failures = await screen.findByRole('list', { name: '通知失敗' })
    await userEvent.click(screen.getByRole('button', { name: 'さらに表示' }))
    expect(await within(screen.getByTestId('notification-failures')).findByTestId('api-error')).toBeVisible()
    expect(within(failures).getByRole('link', { name: '件名 task-006' })).toBeVisible()
  })

  it('通知失敗が無ければその旨を出す', async () => {
    renderDashboard()
    expect(await screen.findByText('通知失敗はありません。')).toBeVisible()
    expect(screen.getByTestId('notification-failure-count')).toHaveTextContent('通知失敗0件')
  })

  it('照会の失敗が5回続いた Linear Team を、失敗の分類と最後に成功した時刻つきでエラー状況に出す（15 6章）', async () => {
    const status: LinearStatus = {
      teams: [
        { teamId: 'team-down', lastSuccessAt: '2026-11-08T00:00:00Z', lastError: 'network', consecutiveFailures: 5 },
        { teamId: 'team-new', lastSuccessAt: null, lastError: 'auth', consecutiveFailures: 7 },
        { teamId: 'team-flaky', lastSuccessAt: '2026-11-08T00:05:00Z', lastError: 'rate_limited', consecutiveFailures: 4 },
        { teamId: 'team-ok', lastSuccessAt: '2026-11-08T00:06:00Z', lastError: null, consecutiveFailures: 0 },
      ],
      issues: [{ linearIssueId: 'i1', issueIdentifier: 'ENG-123', teamId: 'team-ok', lastDecision: 'skipped_initial', lastDecisionAt: '2026-11-08T00:06:00Z' }],
    }
    renderDashboard((request) => (request.url === '/api/linear/status' ? json(200, status) : undefined))
    const failures = await screen.findByRole('list', { name: 'Linear の照会の失敗' })
    const items = within(failures).getAllByRole('listitem')
    expect(items).toHaveLength(2)
    expect(items[0]).toHaveTextContent('team-down')
    expect(items[0]).toHaveTextContent('通信エラー（network）')
    expect(items[0]).toHaveTextContent('5 回続けて失敗')
    expect(items[0]).toHaveTextContent(`最後に成功した時刻: ${new Date('2026-11-08T00:00:00Z').toLocaleString('ja-JP')}`)
    expect(within(items[0]).getByText(new Date('2026-11-08T00:00:00Z').toLocaleString('ja-JP'))).toHaveAttribute('datetime', '2026-11-08T00:00:00Z')
    expect(items[1]).toHaveTextContent('認証エラー（auth）')
    expect(items[1]).toHaveTextContent('最後に成功した時刻: まだ成功していません')
    expect(items[1]).toHaveTextContent('linear-api-key')
    expect(failures).not.toHaveTextContent('team-flaky')

    // 取り込みの状況には、失敗の少ない Team も含めてすべて出す。
    const teams = screen.getByRole('table', { name: 'Linear Team の照会の状況' })
    expect(within(teams).getAllByRole('row').filter((row) => row.hasAttribute('data-team')).map((row) => row.getAttribute('data-team'))).toEqual([
      'team-down',
      'team-new',
      'team-flaky',
      'team-ok',
    ])
    expect(within(teams).getByRole('row', { name: /team-flaky/ })).toHaveTextContent('レート制限（rate_limited）')
    const issues = screen.getByRole('table', { name: 'トリガーのラベルが付いている Issue' })
    expect(issues).toHaveTextContent('ENG-123')
    expect(issues).toHaveTextContent('最初の照会のため記録のみ（skipped_initial）')
  })

  it('照会の対象の Team が無ければその旨を出し、エラー状況には失敗を出さない', async () => {
    renderDashboard()
    expect(await screen.findByText(/照会の対象の Linear Team はありません。/)).toBeVisible()
    expect(screen.getByText('続けて失敗している照会はありません。')).toBeVisible()
  })

  it('GET /api/linear/status が失敗しても、ほかの欄は表示し、Linear の欄にエラーを出す', async () => {
    renderDashboard((request) => (request.url === '/api/linear/status' ? json(500, {}) : undefined))
    expect(await within(await screen.findByTestId('linear-status')).findByTestId('api-error')).toHaveTextContent('サーバーでエラーが発生しました')
    expect(within(screen.getByTestId('linear-poll-failures')).getByTestId('api-error')).toBeVisible()
    expect(screen.getByTestId('task-count-running')).toHaveTextContent('実行中2件')
  })
})

describe('Dashboard の通知失敗: linear_outbox の行（13 3.2、16 3.4.2、Q82）', () => {
  it('eventId と executionId が null の項目を、件名のリンク・kind・channel=linear・reason・記録した時刻とともに出し、Execution の ID を出さない', async () => {
    renderDashboard((request) =>
      request.url === '/api/notification-failures'
        ? failurePage([outboxFailure('ob-1', 'task-003', 'issue_removed', 'missing_acceptance_criteria', '2026-11-08T01:02:03Z')])
        : undefined,
    )
    const failures = await screen.findByRole('list', { name: '通知失敗' })
    const item = within(failures).getByRole('listitem')
    expect(item).toHaveAttribute('data-source', 'linear_outbox')
    expect(within(item).getByRole('link', { name: '件名 task-003' })).toHaveAttribute('href', '/tasks/task-003')
    expect(item).toHaveTextContent('Issue が削除・アーカイブされていた')
    expect(item).toHaveTextContent('reason: issue_removed')
    expect(item).toHaveTextContent('kind: missing_acceptance_criteria')
    expect(item).toHaveTextContent('channel: linear')
    const time = within(item).getByText(new Date('2026-11-08T01:02:03Z').toLocaleString('ja-JP'))
    expect(time).toHaveAttribute('datetime', '2026-11-08T01:02:03Z')
    expect(item.textContent).not.toMatch(/null|undefined/)
    expect(within(item).queryByTestId('notification-failure-execution')).toBeNull()
    expect(item).not.toHaveTextContent('ob-1')
    expect(screen.getByTestId('notification-failure-count')).toHaveTextContent('通知失敗1件')
  })

  it('channel=linear の reason の4つの値に、16 3.4.2 の表に沿った説明を出す', async () => {
    renderDashboard((request) =>
      request.url === '/api/notification-failures'
        ? failurePage([
            outboxFailure('ob-4', 'task-001', 'auth'),
            outboxFailure('ob-3', 'task-002', 'issue_removed'),
            outboxFailure('ob-2', 'task-003', 'retries_exhausted'),
            outboxFailure('ob-1', 'task-004', 'other'),
          ])
        : undefined,
    )
    const items = within(await screen.findByRole('list', { name: '通知失敗' })).getAllByRole('listitem')
    expect(items.map((item) => item.querySelector('.text-sm')?.firstChild?.textContent)).toEqual([
      'Linear API の認証エラー',
      'Issue が削除・アーカイブされていた',
      '一時的な失敗が続き、5回目も失敗した',
      'その他の失敗',
    ])
  })

  it('eventId が null の項目が複数あり execution_events の項目と混ざっても、応答の順のまま欠けず重複せずに出し、「さらに表示」の続きも同じ', async () => {
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {})
    const first = [
      outboxFailure('ob-3', 'task-001', 'auth', 'intake_blocked', '2026-11-08T00:05:00Z'),
      failure(12, 'task-002', 'timeout', 'failed'),
      outboxFailure('ob-2', 'task-003', 'retries_exhausted', 'cancelled', '2026-11-08T00:03:00Z'),
      outboxFailure('ob-1', 'task-004', 'other', 'missing_acceptance_criteria', '2026-11-08T00:02:00Z'),
    ]
    const second = [
      failure(11, 'task-005', 'permission_denied', 'succeeded'),
      outboxFailure('ob-0', 'task-006', 'issue_removed', 'intake_blocked', '2026-11-08T00:00:30Z'),
      // outboxId と eventId の値が同じ文字でも別の項目として出す。
      { ...failure(10, 'task-007', 'other', 'blocked'), outboxId: null },
      outboxFailure('10', 'task-008', 'auth', 'intake_blocked', '2026-11-08T00:00:10Z'),
    ]
    try {
      renderDashboard((request) => {
        if (request.url === '/api/notification-failures') return failurePage(first, 'c1')
        if (request.url === '/api/notification-failures?cursor=c1') return failurePage(second, null)
        return undefined
      })
      const failures = await screen.findByRole('list', { name: '通知失敗' })
      const titles = () => within(failures).getAllByRole('link').map((link) => link.textContent)
      expect(titles()).toEqual(first.map((f) => f.taskTitle))
      expect(within(failures).getAllByRole('listitem').map((li) => li.getAttribute('data-source'))).toEqual([
        'linear_outbox',
        'execution_event',
        'linear_outbox',
        'linear_outbox',
      ])

      await userEvent.click(screen.getByRole('button', { name: 'さらに表示' }))
      expect(await within(failures).findByRole('link', { name: '件名 task-008' })).toBeVisible()
      expect(titles()).toEqual([...first, ...second].map((f) => f.taskTitle))
      expect(screen.getByTestId('notification-failure-count')).toHaveTextContent('通知失敗8件')
      expect(screen.queryByRole('button', { name: 'さらに表示' })).toBeNull()
      // Execution の ID は execution_events の項目にだけ出し、null や undefined の文字は出ない。
      expect(within(failures).getAllByTestId('notification-failure-execution').map((el) => el.textContent)).toEqual([
        ' · e-task-002',
        ' · e-task-005',
        ' · e-task-007',
      ])
      expect(failures.textContent).not.toMatch(/null|undefined/)
      // 同じ key の警告（項目の欠け・重複の原因）が出ていない。
      expect(consoleError.mock.calls.some((args) => String(args[0]).includes('same key'))).toBe(false)
    } finally {
      consoleError.mockRestore()
    }
  })

  it('コメントの本文とエラーの文字列を表示しない', async () => {
    // API は返さない（12 5）が、余分な欄が来ても画面に出さない。
    const leaked = {
      ...outboxFailure('ob-1', 'task-003', 'auth'),
      body: 'COMMENT-BODY-SECRET',
      error: 'ERROR-TEXT-SECRET 401 Unauthorized',
    } as NotificationFailure
    renderDashboard((request) => (request.url === '/api/notification-failures' ? failurePage([leaked]) : undefined))
    const failures = await screen.findByRole('list', { name: '通知失敗' })
    expect(within(failures).getByRole('link', { name: '件名 task-003' })).toBeVisible()
    expect(failures).not.toHaveTextContent('COMMENT-BODY-SECRET')
    expect(failures).not.toHaveTextContent('ERROR-TEXT-SECRET')
  })
})
