import { act, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { ApiClient } from '@/api/client'
import type { Harness, HarnessProfile, Project } from '@/api/registry'
import type { TaskSummary } from '@/api/tasks'
import { fakeEvents } from '@/test/fake-events'
import { apiError, fakeFetch, json, type Recorded } from '@/test/fake-fetch'
import App from '@/App'
import { MAX_TASK_SUBSCRIPTIONS, TASK_LIST_POLL_MS } from './live'

const page = <T,>(items: T[]) => json(200, { items, nextCursor: '' })

// alpha の Profile は受け入れ条件を必須にしない。スペシャルハーネス engineer の既定の版は必須にする（契約 5.2）。
const PROJECTS = [
  { id: 'alpha', name: 'Alpha', profileId: 'p-optional' },
  { id: 'beta', name: 'Beta', profileId: 'p-required' },
] as unknown as Project[]
const HARNESSES = [
  { id: 'engineer', name: 'エンジニア', defaultProfileId: 'p-required' },
  { id: 'draft', name: '下書き', defaultProfileId: null },
] as unknown as Harness[]
const PROFILES = [
  { id: 'p-optional', harnessId: 'writer', version: '1.0.0', definition: { acceptanceCriteria: 'optional' } },
  { id: 'p-required', harnessId: 'engineer', version: '1.0.0', definition: { acceptanceCriteria: 'required' } },
] as unknown as HarnessProfile[]

const task = (id: string, projectId: string, status: TaskSummary['status'], extra: Partial<TaskSummary> = {}): TaskSummary => ({
  id,
  projectId,
  harnessId: '',
  title: `件名 ${id}`,
  body: '本文',
  acceptanceCriteria: '条件',
  shortName: 'task',
  taskType: 'development',
  collaborationMode: 'solo',
  status,
  createdAt: '2026-10-27T00:00:00Z',
  updatedAt: '2026-10-27T00:00:00Z',
  latestExecution: { id: `e-${id}`, seq: 1, mode: 'continue', status: 'pending', errorClass: null },
  ...extra,
})

let TASKS: TaskSummary[] = []
const INITIAL_TASKS = [
  task('task-003', 'alpha', 'cancelled', { latestExecution: { id: 'e3', seq: 1, mode: 'continue', status: 'cancelled', errorClass: 'cancelled' } }),
  task('task-002', 'beta', 'completed', {
    latestExecution: { id: 'e2', seq: 2, mode: 'continue', status: 'succeeded', errorClass: null },
  }),
  task('task-001', 'alpha', 'queued'),
]

/** GET /api/tasks の status・projectId・harnessId を本物の API と同じく完全一致で絞り込む。 */
beforeEach(() => {
  TASKS = INITIAL_TASKS.map((t) => ({ ...t }))
})

function serveTasks(request: Recorded) {
  const url = new URL(request.url, 'http://127.0.0.1')
  if (url.pathname !== '/api/tasks' || request.method !== 'GET') return undefined
  const status = url.searchParams.get('status')
  const projectId = url.searchParams.get('projectId')
  const harnessId = url.searchParams.get('harnessId')
  return page(
    TASKS.filter(
      (t) => (!status || t.status === status) && (!projectId || t.projectId === projectId) && (!harnessId || t.harnessId === harnessId),
    ),
  )
}

function renderAt(path: string, handler: (request: Recorded) => Response | Promise<Response> | undefined = () => undefined) {
  window.history.pushState(null, '', path)
  const fake = fakeFetch((request) => {
    if (request.url === '/api/session') return json(200, { csrfToken: 't' })
    const handled = handler(request)
    if (handled) return handled
    if (request.url.startsWith('/api/projects')) return page(PROJECTS)
    if (request.url.startsWith('/api/harnesses')) return page(HARNESSES)
    if (request.url.startsWith('/api/harness-profiles')) return page(PROFILES)
    return serveTasks(request) ?? json(500, {})
  })
  const events = fakeEvents()
  render(<App api={new ApiClient(fake.fetch, events.factory)} />)
  return { ...fake, events }
}

function fieldOf(label: string) {
  return screen.getByLabelText(label).closest('[data-field]') as HTMLElement
}

async function fillForm(acceptanceCriteria: string, destination = 'harness:engineer') {
  await userEvent.selectOptions(await screen.findByLabelText('振り分け先'), destination)
  await userEvent.type(screen.getByLabelText('件名'), 'ログイン画面')
  await userEvent.type(screen.getByLabelText('本文'), 'ログインフォームを作る')
  if (acceptanceCriteria) await userEvent.type(screen.getByLabelText('受け入れ条件'), acceptanceCriteria)
}

const posts = (fake: { calls: Recorded[] }) => fake.calls.filter((c) => c.method === 'POST' && c.url === '/api/tasks')

afterEach(() => window.history.pushState(null, '', '/'))

describe('タスク作成', () => {
  it('振り分け先の Profile が required のとき、受け入れ条件が空なら送信せず、項目にエラーを出す', async () => {
    const fake = renderAt('/tasks/_new')
    await fillForm('')
    expect(screen.getByLabelText('受け入れ条件')).toBeRequired()
    await userEvent.click(screen.getByRole('button', { name: '作成' }))
    expect(within(fieldOf('受け入れ条件')).getByText('入力してください')).toBeVisible()
    expect(screen.getByLabelText('受け入れ条件')).toHaveAttribute('aria-invalid', 'true')
    expect(posts(fake)).toHaveLength(0)

    // 空白だけも空と同じ扱い（04 3.1）。
    await userEvent.type(screen.getByLabelText('受け入れ条件'), '   ')
    await userEvent.click(screen.getByRole('button', { name: '作成' }))
    expect(within(fieldOf('受け入れ条件')).getByText('入力してください')).toBeVisible()
    expect(posts(fake)).toHaveLength(0)
  })

  it('振り分け先の Profile が required でなければ、受け入れ条件が空でも送れる', async () => {
    const fake = renderAt('/tasks/_new', (request) => (request.method === 'POST' ? json(201, task('task-004', 'alpha', 'queued')) : undefined))
    await fillForm('', 'project:alpha')
    expect(screen.getByLabelText('受け入れ条件')).not.toBeRequired()
    await userEvent.click(screen.getByRole('button', { name: '作成' }))
    expect(await screen.findByRole('status')).toHaveTextContent('タスク task-004')
    expect(JSON.parse(posts(fake)[0].body ?? '')).toEqual({ projectId: 'alpha', title: 'ログイン画面', body: 'ログインフォームを作る', acceptanceCriteria: '' })
  })

  it('既定の版の無いスペシャルハーネスは選べない', async () => {
    renderAt('/tasks/_new')
    const option = await screen.findByRole('option', { name: /下書き/ })
    expect(option).toBeDisabled()
  })

  it('サーバーが受け入れ条件を 400 で拒否したら details を項目に出す', async () => {
    renderAt('/tasks/_new', (request) =>
      request.method === 'POST' ? apiError(400, 'validation_failed', 'invalid request', [{ field: 'acceptanceCriteria', reason: 'required' }]) : undefined,
    )
    await fillForm('', 'project:alpha')
    await userEvent.click(screen.getByRole('button', { name: '作成' }))
    expect(await within(fieldOf('受け入れ条件')).findByText('入力してください')).toBeVisible()
  })

  it('二重クリックしても POST は1回だけで、Idempotency-Key を付けてスペシャルハーネスの振り分け先で送る', async () => {
    let resolve: (response: Response) => void = () => {}
    const fake = renderAt('/tasks/_new', (request) =>
      request.method === 'POST' ? new Promise<Response>((r) => (resolve = r)) : undefined,
    )
    await fillForm('ログインのテストが通る')
    await userEvent.dblClick(screen.getByRole('button', { name: '作成' }))
    expect(screen.getByRole('button', { name: '作成しています…' })).toBeDisabled()
    expect(posts(fake)).toHaveLength(1)
    const [post] = posts(fake)
    expect(post.headers['Idempotency-Key']).toMatch(/^[0-9a-f-]{36}$/)
    expect(post.headers['X-CSRF-Token']).toBe('t')
    expect(JSON.parse(post.body ?? '')).toEqual({
      harnessId: 'engineer',
      title: 'ログイン画面',
      body: 'ログインフォームを作る',
      acceptanceCriteria: 'ログインのテストが通る',
    })

    resolve(json(201, task('task-004', '', 'queued', { harnessId: 'engineer' })))
    expect(await screen.findByRole('status')).toHaveTextContent('タスク task-004（件名 task-004）を作成しました。')
    expect(window.location.pathname).toBe('/tasks')
    expect(posts(fake)).toHaveLength(1)
  })

  it('通信に失敗した同じ内容の再送は同じ Idempotency-Key を使い、内容を変えたら新しいキーにする', async () => {
    const fake = renderAt('/tasks/_new', (request) => (request.method === 'POST' ? Promise.reject(new TypeError('offline')) : undefined))
    await fillForm('条件')
    await userEvent.click(screen.getByRole('button', { name: '作成' }))
    expect(await screen.findByText('evaloomd に接続できません')).toBeVisible()
    await userEvent.click(screen.getByRole('button', { name: '作成' }))
    await screen.findByText('evaloomd に接続できません')
    await userEvent.type(screen.getByLabelText('件名'), '（改）')
    await userEvent.click(screen.getByRole('button', { name: '作成' }))
    await screen.findByText('evaloomd に接続できません')
    const keys = posts(fake).map((c) => c.headers['Idempotency-Key'])
    expect(keys).toHaveLength(3)
    expect(keys[1]).toBe(keys[0])
    expect(keys[2]).not.toBe(keys[0])
  })

  it('既定の版の無いスペシャルハーネスの 422 は振り分け先の項目に出す', async () => {
    renderAt('/tasks/_new', (request) =>
      request.method === 'POST'
        ? apiError(422, 'precondition_failed', 'Harness has no default Profile version', [{ field: 'harnessId', reason: 'default_profile_missing' }])
        : undefined,
    )
    await fillForm('条件')
    await userEvent.click(screen.getByRole('button', { name: '作成' }))
    expect(await within(fieldOf('振り分け先')).findByText(/既定の版がありません/)).toBeVisible()
  })
})

describe('Tasks 一覧', () => {
  const rows = () => within(screen.getByRole('table', { name: 'タスク' })).getAllByRole('row').slice(1)

  it('タスク、状態、振り分け先、最新 Execution を出す', async () => {
    renderAt('/tasks')
    await screen.findByRole('table', { name: 'タスク' })
    expect(rows()).toHaveLength(3)
    const done = rows()[1]
    expect(done).toHaveTextContent('件名 task-002')
    expect(done).toHaveTextContent('Completed')
    expect(done).toHaveTextContent('Beta')
    expect(done).toHaveTextContent('#2 succeeded')
    expect(rows()[2]).toHaveTextContent('Queued')
  })

  it('最新の Execution の outputs を label のリンクで出し、http(s) 以外の URL はリンクにしない', async () => {
    TASKS = [
      task('task-006', '', 'completed', {
        harnessId: 'engineer',
        latestExecution: {
          id: 'e6',
          seq: 1,
          mode: 'continue',
          status: 'succeeded',
          errorClass: null,
          outputs: [
            { kind: 'pull_request', label: 'PR #12', url: 'https://github.com/acme/app/pull/12' },
            { kind: 'note', label: '怪しい', url: 'javascript:alert(1)' },
          ],
        },
      }),
      task('task-001', 'alpha', 'queued', { latestExecution: { id: 'e1', seq: 1, mode: 'continue', status: 'pending', errorClass: null, outputs: [] } }),
    ]
    renderAt('/tasks')
    await screen.findByRole('table', { name: 'タスク' })
    const [done, queued] = rows()
    expect(within(done).getByRole('link', { name: 'PR #12' })).toHaveAttribute('href', 'https://github.com/acme/app/pull/12')
    expect(within(done).queryByRole('link', { name: '怪しい' })).not.toBeInTheDocument()
    expect(done).toHaveTextContent('javascript:alert(1)')
    // 振り分け先はスペシャルハーネスの名前で出す。
    expect(within(done).getByRole('link', { name: 'エンジニア' })).toHaveAttribute('href', '/harnesses/engineer')
    expect(done).toHaveTextContent('スペシャルハーネス')
    expect(queued.querySelectorAll('td')[4]).toHaveTextContent('なし')
  })

  it('Linear 起因のタスクに Issue の識別子とリンクを出し、保留中のタスクには理由を文字で出す', async () => {
    TASKS = [
      task('task-005', '', 'blocked', {
        source: 'linear',
        intakeBlockCause: 'missing_acceptance_criteria',
        linear: { identifier: 'ENG-9', url: 'https://linear.app/acme/issue/ENG-9' },
        latestExecution: null,
      }),
      task('task-004', 'alpha', 'queued', { source: 'linear', intakeBlockCause: null, linear: { identifier: 'ENG-8', url: 'https://linear.app/acme/issue/ENG-8' } }),
      ...TASKS.map((t) => ({ ...t, source: 'manual' as const, intakeBlockCause: null, linear: null })),
    ]
    renderAt('/tasks')
    await screen.findByRole('table', { name: 'タスク' })
    const [held, started, manual] = rows()
    expect(within(held).getByRole('link', { name: 'ENG-9' })).toHaveAttribute('href', 'https://linear.app/acme/issue/ENG-9')
    expect(held).toHaveTextContent('Blocked')
    expect(within(held).getByTestId('held-reason')).toHaveTextContent('保留の理由: 振り分け先の版が受け入れ条件を求めているのに、Issue に受け入れ条件が無い')
    // 振り分け先が決まっていない保留のタスクは、振り分け先を「なし」と出す。
    expect(held.querySelectorAll('td')[2]).toHaveTextContent('なし')
    expect(within(held).queryByRole('link', { name: /^\/projects/ })).not.toBeInTheDocument()
    expect(within(started).getByRole('link', { name: 'ENG-8' })).toHaveAttribute('href', 'https://linear.app/acme/issue/ENG-8')
    expect(within(started).queryByTestId('held-reason')).not.toBeInTheDocument()
    expect(within(manual).queryByTestId('linear-issue-link')).not.toBeInTheDocument()
  })

  it('振り分けを待つタスクには、そのことと開いている振り分けの判断待ちへのリンクを出す', async () => {
    const linear = (n: number) => ({ source: 'linear' as const, intakeBlockCause: null, linear: { identifier: `ENG-${n}`, url: `https://linear.app/acme/issue/ENG-${n}` } })
    TASKS = [
      task('task-011', '', 'routing', { ...linear(11), latestExecution: null }),
      task('task-010', '', 'routing', { ...linear(10), latestExecution: null }),
      task('task-004', 'alpha', 'queued', linear(4)),
    ]
    const decisionCalls: string[] = []
    renderAt('/tasks', (request) => {
      if (!request.url.startsWith('/api/decisions?')) return undefined
      decisionCalls.push(request.url)
      // task-010 は判断待ちが開いている。task-011 は提案を作っている途中で、判断待ちがまだ無い。
      return page([
        { id: 'D10', kind: 'route', taskId: 'task-010', executionId: null, subjectId: 'P10', cause: null, status: 'open', detail: {}, createdAt: '2026-10-28T00:00:00Z' },
      ])
    })
    await screen.findByRole('table', { name: 'タスク' })
    const [pending, open, started] = rows()
    await waitFor(() => expect(within(open).getByRole('link', { name: '判断待ちを開く' })).toHaveAttribute('href', '/decisions/D10'))
    expect(within(open).getByTestId('routing-wait')).toHaveTextContent('振り分けを待っています')
    expect(within(open).getByRole('link', { name: 'ENG-10' })).toHaveAttribute('href', 'https://linear.app/acme/issue/ENG-10')
    expect(within(pending).getByTestId('routing-wait')).toHaveTextContent('振り分けを待っています振り分けの提案を作っています')
    expect(within(pending).queryByRole('link', { name: '判断待ちを開く' })).not.toBeInTheDocument()
    expect(within(started).queryByTestId('routing-wait')).not.toBeInTheDocument()
    expect(decisionCalls).toEqual(['/api/decisions?status=open&kind=route&limit=200'])
  })

  it('振り分けを待つタスクが無ければ判断待ちを読まない', async () => {
    const fake = renderAt('/tasks')
    await screen.findByRole('table', { name: 'タスク' })
    expect(screen.queryByTestId('routing-wait')).not.toBeInTheDocument()
    expect(fake.calls.some((c) => c.url.startsWith('/api/decisions'))).toBe(false)
  })

  it('状態と振り分け先で絞り込め、条件は URL に残る', async () => {
    const fake = renderAt('/tasks')
    await screen.findByRole('table', { name: 'タスク' })
    await userEvent.selectOptions(screen.getByLabelText('振り分け先'), 'project:alpha')
    expect(await screen.findByText('件名 task-003')).toBeVisible()
    expect(rows()).toHaveLength(2)
    expect(window.location.search).toBe('?projectId=alpha')

    await userEvent.selectOptions(screen.getByLabelText('状態'), 'queued')
    await screen.findByText('件名 task-001')
    expect(screen.queryByText('件名 task-003')).toBeNull()
    expect(rows()).toHaveLength(1)
    expect(fake.calls.at(-1)?.url).toBe('/api/tasks?status=queued&projectId=alpha&limit=200')
    expect(window.location.search).toBe('?status=queued&projectId=alpha')

    await userEvent.selectOptions(screen.getByLabelText('振り分け先'), 'project:beta')
    expect(await screen.findByText('条件に合うタスクはありません。')).toBeVisible()

    await userEvent.click(screen.getByRole('button', { name: '絞り込みを解除' }))
    await screen.findByText('件名 task-002')
    expect(rows()).toHaveLength(3)
    expect(window.location.search).toBe('')
  })

  it('状態の選択肢は 02 5.1 の10状態で、廃止済みの状態は出ない', async () => {
    renderAt('/tasks')
    await screen.findByRole('table', { name: 'タスク' })
    const options = within(screen.getByLabelText('状態')).getAllByRole('option').map((o) => o.textContent)
    expect(options).toEqual([
      'すべて',
      'Received',
      'Routing',
      'Queued',
      'Running',
      'Waiting Approval',
      'Blocked',
      'Completed',
      'Failed',
      'Cancelled',
      'Timed Out',
    ])
    expect(options).not.toContain('Verifying')
    expect(options).not.toContain('Planning')
  })

  it('Routing と Waiting Approval で絞り込める', async () => {
    TASKS = [task('task-009', '', 'routing', { latestExecution: null }), task('task-008', 'alpha', 'waiting_approval'), ...TASKS]
    const fake = renderAt('/tasks')
    await screen.findByText('件名 task-009')
    await userEvent.selectOptions(screen.getByLabelText('状態'), 'Routing')
    await waitFor(() => expect(rows()).toHaveLength(1))
    expect(rows()[0]).toHaveTextContent('件名 task-009')
    expect(rows()[0]).toHaveTextContent('Routing')
    expect(fake.calls.at(-1)?.url).toBe('/api/tasks?status=routing&limit=200')

    await userEvent.selectOptions(screen.getByLabelText('状態'), 'Waiting Approval')
    await screen.findByText('件名 task-008')
    expect(rows()).toHaveLength(1)
    expect(rows()[0]).toHaveTextContent('Waiting Approval')
    expect(fake.calls.at(-1)?.url).toBe('/api/tasks?status=waiting_approval&limit=200')
  })

  it('URL に廃止済みの状態があっても API へ送らない', async () => {
    const fake = renderAt('/tasks?status=verifying')
    await screen.findByText('件名 task-001')
    expect(screen.getByLabelText('状態')).toHaveValue('')
    expect(fake.calls.filter((c) => c.url.startsWith('/api/tasks')).every((c) => !c.url.includes('verifying'))).toBe(true)
  })

  it('スペシャルハーネスの振り分け先で絞り込める', async () => {
    TASKS = [task('task-007', '', 'queued', { harnessId: 'engineer' }), ...TASKS]
    const fake = renderAt('/tasks')
    await screen.findByText('件名 task-007')
    await userEvent.selectOptions(screen.getByLabelText('振り分け先'), 'harness:engineer')
    await waitFor(() => expect(rows()).toHaveLength(1))
    expect(rows()[0]).toHaveTextContent('件名 task-007')
    expect(fake.calls.at(-1)?.url).toBe('/api/tasks?harnessId=engineer&limit=200')
    expect(window.location.search).toBe('?harnessId=engineer')
  })

  it('URL の絞り込み条件で開ける', async () => {
    renderAt('/tasks?status=completed')
    await screen.findByText('件名 task-002')
    expect(rows()).toHaveLength(1)
    expect(screen.getByLabelText('状態')).toHaveValue('completed')
  })

  it('実行中のタスクは status_changed を受けると、画面を読み直さずに状態と絞り込み結果が更新される', async () => {
    const fake = renderAt('/tasks?status=queued')
    await screen.findByText('件名 task-001')
    expect(rows()).toHaveLength(1)
    // 終端でない最新 Execution（task-001 の pending）だけを購読する。終端（cancelled・succeeded）は購読しない。
    const url = '/api/executions/e-task-001/events'
    // 購読は描画の後の effect で張られるので、張られるのを待ってから確かめる。
    await waitFor(() => expect(fake.events.sources.map((s) => s.url)).toEqual([url]))
    const [source] = fake.events.open(url)
    const fetches = () => fake.calls.filter((c) => c.url.startsWith('/api/tasks')).length
    const before = fetches()

    // 購読開始時に届く過去のイベント（表示中の状態への遷移）では取り直さない。
    act(() => source.emit('status_changed', { from: 'received', to: 'queued', reason: 'task_created' }, 1))
    await new Promise((r) => setTimeout(r, 150))
    expect(fetches()).toBe(before)

    // Runner が claim した。一覧を取り直し、Queued の絞り込みから外れる。
    TASKS[2] = { ...TASKS[2], status: 'running', latestExecution: { ...TASKS[2].latestExecution!, status: 'claimed' } }
    act(() => source.emit('status_changed', { from: 'pending', to: 'claimed', reason: 'runner_claimed' }, 2))
    expect(await screen.findByText('条件に合うタスクはありません。')).toBeVisible()
    expect(fetches()).toBe(before + 1)
    expect(window.location.pathname + window.location.search).toBe('/tasks?status=queued')
    // 一覧から外れた Execution の購読は閉じる（閉じるのは描画の後の effect なので、待ってから確かめる）。
    await waitFor(() => expect(source.closed).toBe(true))

    // 絞り込みを外すと Running で出て、引き続き購読する。
    await userEvent.click(screen.getByRole('button', { name: '絞り込みを解除' }))
    const running = await screen.findByText('件名 task-001')
    const row = running.closest('tr') as HTMLElement
    expect(row).toHaveTextContent('Running')
    expect(row).toHaveTextContent('#1 claimed')
    await waitFor(() => expect(fake.events.open(url)).toHaveLength(1))
    const [again] = fake.events.open(url)

    // 完了すると状態が更新され、終端になったので購読を閉じる。
    TASKS[2] = {
      ...TASKS[2],
      status: 'completed',
      latestExecution: { ...TASKS[2].latestExecution!, status: 'succeeded' },
    }
    act(() => again.emit('status_changed', { from: 'publishing', to: 'succeeded', reason: 'runner_result' }, 9))
    expect(await within(row).findByText('Completed')).toBeVisible()
    expect(row).toHaveTextContent('#1 succeeded')
    await waitFor(() => {
      expect(again.closed).toBe(true)
      expect(fake.events.sources.every((s) => s.closed)).toBe(true)
    })
  })

  it('画面を離れると購読を閉じる', async () => {
    const fake = renderAt('/tasks')
    await screen.findByText('件名 task-001')
    await waitFor(() => expect(fake.events.open('/api/executions/e-task-001/events')).toHaveLength(1))
    await userEvent.click(screen.getByRole('navigation', { name: '画面' }).querySelector('a[href="/projects"]') as HTMLElement)
    await screen.findByRole('heading', { name: 'Projects' })
    await waitFor(() => expect(fake.events.sources.every((s) => s.closed)).toBe(true))
  })

  it('タスク状態が running のまま Execution が preparing → running に進むと、最新 Execution の表示が更新される', async () => {
    TASKS[2] = { ...TASKS[2], status: 'running', latestExecution: { ...TASKS[2].latestExecution!, status: 'preparing' } }
    const fake = renderAt('/tasks')
    const row = (await screen.findByText('件名 task-001')).closest('tr') as HTMLElement
    expect(row).toHaveTextContent('#1 preparing')
    // 購読は描画の後の effect で張られるので、張られるのを待ってから emit する。
    await waitFor(() => expect(fake.events.open('/api/executions/e-task-001/events')).toHaveLength(1))
    const [source] = fake.events.open('/api/executions/e-task-001/events')
    const fetches = () => fake.calls.filter((c) => c.url.startsWith('/api/tasks')).length
    const before = fetches()

    // 表示中の Execution の状態（preparing）への遷移は、購読開始時に届く過去のイベントなので取り直さない。
    act(() => source.emit('status_changed', { from: 'claimed', to: 'preparing', reason: 'runner_result' }, 3))
    await new Promise((r) => setTimeout(r, 150))
    expect(fetches()).toBe(before)

    // タスク状態は running のまま、Execution だけ preparing → running に進む（02 5.2）。
    TASKS[2] = { ...TASKS[2], latestExecution: { ...TASKS[2].latestExecution!, status: 'running' } }
    act(() => source.emit('status_changed', { from: 'preparing', to: 'running', reason: 'runner_result' }, 4))
    expect(await within(row).findByText('running')).toBeVisible()
    expect(row).toHaveTextContent('#1 running')
    expect(row).toHaveTextContent('Running')
    expect(fetches()).toBe(before + 1)
  })

  it('購読の上限から漏れたタスク（Blocked 4件＋Queued 1件の Queued）が claim されても、一定間隔の取り直しで一覧が更新される', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    try {
      const blocked = Array.from({ length: MAX_TASK_SUBSCRIPTIONS }, (_, i) =>
        task(`task-00${i + 2}`, 'beta', 'blocked', {
          latestExecution: { id: `e-blocked-${i}`, seq: 1, mode: 'continue', status: 'blocked', errorClass: null },
        }),
      )
      const queued = task('task-001', 'alpha', 'queued')
      TASKS = [...blocked.reverse(), queued]
      const fake = renderAt('/tasks')
      const row = (await screen.findByText('件名 task-001')).closest('tr') as HTMLElement
      expect(row).toHaveTextContent('Queued')
      // Blocked の4件だけを購読し、Queued の Execution は購読していない。
      await waitFor(() => {
        const subscribed = fake.events.sources.filter((s) => !s.closed).map((s) => s.url)
        expect(subscribed).toHaveLength(MAX_TASK_SUBSCRIPTIONS)
        expect(subscribed).not.toContain('/api/executions/e-task-001/events')
      })

      // 購読していない Execution が claim された。SSE は届かないが、一定間隔の取り直しで拾う。
      TASKS = TASKS.map((t) =>
        t.id === 'task-001' ? { ...t, status: 'running', latestExecution: { ...t.latestExecution!, status: 'claimed' } } : t,
      )
      await act(async () => {
        await vi.advanceTimersByTimeAsync(TASK_LIST_POLL_MS)
      })
      expect(await within(row).findByText('Running')).toBeVisible()
      expect(row).toHaveTextContent('#1 claimed')
      expect(window.location.pathname).toBe('/tasks')
    } finally {
      vi.useRealTimers()
    }
  })

  it('購読の上限に収まっている間は、一定間隔の取り直しをしない', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    try {
      const fake = renderAt('/tasks')
      await screen.findByText('件名 task-001')
      const fetches = () => fake.calls.filter((c) => c.url.startsWith('/api/tasks')).length
      const before = fetches()
      await act(async () => {
        await vi.advanceTimersByTimeAsync(TASK_LIST_POLL_MS * 2)
      })
      expect(fetches()).toBe(before)
    } finally {
      vi.useRealTimers()
    }
  })
})

