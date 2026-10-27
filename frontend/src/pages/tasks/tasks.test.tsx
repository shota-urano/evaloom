import { act, render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { ApiClient } from '@/api/client'
import type { Project } from '@/api/registry'
import type { TaskSummary } from '@/api/tasks'
import { fakeEvents } from '@/test/fake-events'
import { apiError, fakeFetch, json, type Recorded } from '@/test/fake-fetch'
import App from '@/App'
import { MAX_TASK_SUBSCRIPTIONS, TASK_LIST_POLL_MS } from './live'

const page = <T,>(items: T[]) => json(200, { items, nextCursor: '' })

const PROJECTS = [
  { id: 'alpha', name: 'Alpha', harnesses: [] },
  { id: 'beta', name: 'Beta', harnesses: [] },
] as unknown as Project[]

const task = (id: string, projectId: string, status: TaskSummary['status'], extra: Partial<TaskSummary> = {}): TaskSummary => ({
  id,
  projectId,
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
  prUrl: null,
  ...extra,
})

let TASKS: TaskSummary[] = []
const INITIAL_TASKS = [
  task('task-003', 'alpha', 'cancelled', { latestExecution: { id: 'e3', seq: 1, mode: 'continue', status: 'cancelled', errorClass: 'cancelled' } }),
  task('task-002', 'beta', 'completed', {
    latestExecution: { id: 'e2', seq: 2, mode: 'continue', status: 'succeeded', errorClass: null },
    prUrl: 'https://example.com/pr/2',
  }),
  task('task-001', 'alpha', 'queued'),
]

/** GET /api/tasks の status・projectId を本物の API と同じく完全一致で絞り込む。 */
beforeEach(() => {
  TASKS = INITIAL_TASKS.map((t) => ({ ...t }))
})

function serveTasks(request: Recorded) {
  const url = new URL(request.url, 'http://127.0.0.1')
  if (url.pathname !== '/api/tasks' || request.method !== 'GET') return undefined
  const status = url.searchParams.get('status')
  const projectId = url.searchParams.get('projectId')
  return page(TASKS.filter((t) => (!status || t.status === status) && (!projectId || t.projectId === projectId)))
}

function renderAt(path: string, handler: (request: Recorded) => Response | Promise<Response> | undefined = () => undefined) {
  window.history.pushState(null, '', path)
  const fake = fakeFetch((request) => {
    if (request.url === '/api/session') return json(200, { csrfToken: 't' })
    const handled = handler(request)
    if (handled) return handled
    if (request.url.startsWith('/api/projects')) return page(PROJECTS)
    return serveTasks(request) ?? json(500, {})
  })
  const events = fakeEvents()
  render(<App api={new ApiClient(fake.fetch, events.factory)} />)
  return { ...fake, events }
}

function fieldOf(label: string) {
  return screen.getByLabelText(label).closest('[data-field]') as HTMLElement
}

async function fillForm(acceptanceCriteria: string) {
  await userEvent.selectOptions(await screen.findByLabelText('プロジェクト'), 'alpha')
  await userEvent.type(screen.getByLabelText('件名'), 'ログイン画面')
  await userEvent.type(screen.getByLabelText('本文'), 'ログインフォームを作る')
  if (acceptanceCriteria) await userEvent.type(screen.getByLabelText('受け入れ条件'), acceptanceCriteria)
}

const posts = (fake: { calls: Recorded[] }) => fake.calls.filter((c) => c.method === 'POST' && c.url === '/api/tasks')

afterEach(() => window.history.pushState(null, '', '/'))

describe('タスク作成', () => {
  it('受け入れ条件が空なら送信せず、項目にエラーを出す', async () => {
    const fake = renderAt('/tasks/_new')
    await fillForm('')
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

  it('サーバーが受け入れ条件を 400 で拒否したら details を項目に出す', async () => {
    renderAt('/tasks/_new', (request) =>
      request.method === 'POST' ? apiError(400, 'validation_failed', 'invalid request', [{ field: 'acceptanceCriteria', reason: 'required' }]) : undefined,
    )
    await fillForm('条件')
    await userEvent.click(screen.getByRole('button', { name: '作成' }))
    expect(await within(fieldOf('受け入れ条件')).findByText('入力してください')).toBeVisible()
  })

  it('二重クリックしても POST は1回だけで、Idempotency-Key を付けて API の項目名で送る', async () => {
    let resolve: (response: Response) => void = () => {}
    const fake = renderAt('/tasks/_new', (request) =>
      request.method === 'POST' ? new Promise<Response>((r) => (resolve = r)) : undefined,
    )
    await fillForm('ログインのテストが通る')
    await userEvent.type(screen.getByLabelText('短い名前'), 'add-login')
    await userEvent.dblClick(screen.getByRole('button', { name: '作成' }))
    expect(screen.getByRole('button', { name: '作成しています…' })).toBeDisabled()
    expect(posts(fake)).toHaveLength(1)
    const [post] = posts(fake)
    expect(post.headers['Idempotency-Key']).toMatch(/^[0-9a-f-]{36}$/)
    expect(post.headers['X-CSRF-Token']).toBe('t')
    expect(JSON.parse(post.body ?? '')).toEqual({
      projectId: 'alpha',
      title: 'ログイン画面',
      body: 'ログインフォームを作る',
      acceptanceCriteria: 'ログインのテストが通る',
      shortName: 'add-login',
    })

    resolve(json(201, task('task-004', 'alpha', 'queued')))
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

  it('Default Harness の割り当てが無い 422 はプロジェクトの項目に出す', async () => {
    renderAt('/tasks/_new', (request) =>
      request.method === 'POST'
        ? apiError(422, 'precondition_failed', 'Project has no Default Harness assignment', [{ field: 'projectId', reason: 'default_harness_missing' }])
        : undefined,
    )
    await fillForm('条件')
    await userEvent.click(screen.getByRole('button', { name: '作成' }))
    expect(await within(fieldOf('プロジェクト')).findByText(/既定の役割に Harness が割り当てられていません/)).toBeVisible()
  })
})

describe('Tasks 一覧', () => {
  const rows = () => within(screen.getByRole('table', { name: 'タスク' })).getAllByRole('row').slice(1)

  it('タスク、状態、プロジェクト、最新 Execution、PR へのリンクを出す', async () => {
    renderAt('/tasks')
    await screen.findByRole('table', { name: 'タスク' })
    expect(rows()).toHaveLength(3)
    const done = rows()[1]
    expect(done).toHaveTextContent('件名 task-002')
    expect(done).toHaveTextContent('Completed')
    expect(done).toHaveTextContent('Beta')
    expect(done).toHaveTextContent('#2 succeeded')
    expect(within(done).getByRole('link', { name: 'PR を開く' })).toHaveAttribute('href', 'https://example.com/pr/2')
    expect(rows()[2]).toHaveTextContent('Queued')
  })

  it('状態とプロジェクトで絞り込め、条件は URL に残る', async () => {
    const fake = renderAt('/tasks')
    await screen.findByRole('table', { name: 'タスク' })
    await userEvent.selectOptions(screen.getByLabelText('プロジェクト'), 'alpha')
    expect(await screen.findByText('件名 task-003')).toBeVisible()
    expect(rows()).toHaveLength(2)
    expect(window.location.search).toBe('?projectId=alpha')

    await userEvent.selectOptions(screen.getByLabelText('状態'), 'queued')
    await screen.findByText('件名 task-001')
    expect(screen.queryByText('件名 task-003')).toBeNull()
    expect(rows()).toHaveLength(1)
    expect(fake.calls.at(-1)?.url).toBe('/api/tasks?status=queued&projectId=alpha&limit=200')
    expect(window.location.search).toBe('?status=queued&projectId=alpha')

    await userEvent.selectOptions(screen.getByLabelText('プロジェクト'), 'beta')
    expect(await screen.findByText('条件に合うタスクはありません。')).toBeVisible()

    await userEvent.click(screen.getByRole('button', { name: '絞り込みを解除' }))
    await screen.findByText('件名 task-002')
    expect(rows()).toHaveLength(3)
    expect(window.location.search).toBe('')
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
    expect(fake.events.sources.map((s) => s.url)).toEqual([url])
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
    // 一覧から外れた Execution の購読は閉じる。
    expect(source.closed).toBe(true)

    // 絞り込みを外すと Running で出て、引き続き購読する。
    await userEvent.click(screen.getByRole('button', { name: '絞り込みを解除' }))
    const running = await screen.findByText('件名 task-001')
    const row = running.closest('tr') as HTMLElement
    expect(row).toHaveTextContent('Running')
    expect(row).toHaveTextContent('#1 claimed')
    const [again] = fake.events.open(url)

    // PR まで作って完了すると、状態と PR へのリンクが更新され、終端になったので購読を閉じる。
    TASKS[2] = {
      ...TASKS[2],
      status: 'completed',
      latestExecution: { ...TASKS[2].latestExecution!, status: 'succeeded' },
      prUrl: 'https://example.com/pr/1',
    }
    act(() => again.emit('status_changed', { from: 'publishing', to: 'succeeded', reason: 'runner_result' }, 9))
    expect(await within(row).findByText('Completed')).toBeVisible()
    expect(row).toHaveTextContent('#1 succeeded')
    expect(within(row).getByRole('link', { name: 'PR を開く' })).toHaveAttribute('href', 'https://example.com/pr/1')
    expect(again.closed).toBe(true)
    expect(fake.events.sources.every((s) => s.closed)).toBe(true)
  })

  it('画面を離れると購読を閉じる', async () => {
    const fake = renderAt('/tasks')
    await screen.findByText('件名 task-001')
    expect(fake.events.open('/api/executions/e-task-001/events')).toHaveLength(1)
    await userEvent.click(screen.getByRole('navigation', { name: '画面' }).querySelector('a[href="/projects"]') as HTMLElement)
    await screen.findByRole('heading', { name: 'Projects' })
    expect(fake.events.sources.every((s) => s.closed)).toBe(true)
  })

  it('タスク状態が running のまま Execution が preparing → running に進むと、最新 Execution の表示が更新される', async () => {
    TASKS[2] = { ...TASKS[2], status: 'running', latestExecution: { ...TASKS[2].latestExecution!, status: 'preparing' } }
    const fake = renderAt('/tasks')
    const row = (await screen.findByText('件名 task-001')).closest('tr') as HTMLElement
    expect(row).toHaveTextContent('#1 preparing')
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
      const subscribed = fake.events.sources.filter((s) => !s.closed).map((s) => s.url)
      expect(subscribed).toHaveLength(MAX_TASK_SUBSCRIPTIONS)
      expect(subscribed).not.toContain('/api/executions/e-task-001/events')

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

