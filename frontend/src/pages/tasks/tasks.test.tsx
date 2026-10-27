import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it } from 'vitest'

import { ApiClient } from '@/api/client'
import type { Project } from '@/api/registry'
import type { TaskSummary } from '@/api/tasks'
import { apiError, fakeFetch, json, type Recorded } from '@/test/fake-fetch'
import App from '@/App'

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

const TASKS = [
  task('task-003', 'alpha', 'cancelled'),
  task('task-002', 'beta', 'completed', {
    latestExecution: { id: 'e2', seq: 2, mode: 'continue', status: 'succeeded', errorClass: null },
    prUrl: 'https://example.com/pr/2',
  }),
  task('task-001', 'alpha', 'queued'),
]

/** GET /api/tasks の status・projectId を本物の API と同じく完全一致で絞り込む。 */
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
  render(<App api={new ApiClient(fake.fetch)} />)
  return fake
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
})
