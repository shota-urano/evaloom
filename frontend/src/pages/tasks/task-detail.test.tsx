import { act, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'

import { ApiClient } from '@/api/client'
import { streamTiming } from '@/api/event-stream'
import type { ExecutionDetail, ExecutionHistoryItem, TaskDetail } from '@/api/tasks'
import { fakeEvents } from '@/test/fake-events'
import { apiError, fakeFetch, json, type Recorded } from '@/test/fake-fetch'
import { FakeStream } from '@/test/fake-stream'
import App from '@/App'

const E1 = '01HX0000000000000000000001'
const E2 = '01HX0000000000000000000002'

const history = (id: string, seq: number, status: string, extra: Partial<ExecutionHistoryItem> = {}): ExecutionHistoryItem => ({
  id,
  seq,
  mode: 'continue',
  status,
  errorClass: null,
  profileVersion: '1.0.0',
  profileCommitSha: 'abcdef0123456789abcdef0123456789abcdef01',
  startedAt: '2026-10-28T01:00:00Z',
  finishedAt: null,
  ...extra,
})

function taskWith(status: TaskDetail['status'], executions: ExecutionHistoryItem[]): TaskDetail {
  const latest = executions.at(-1)
  return {
    id: 'task-001',
    projectId: 'alpha',
    title: 'ログイン画面',
    body: 'ログイン画面を作る',
    acceptanceCriteria: 'ログインできる',
    shortName: 'login',
    taskType: 'development',
    collaborationMode: 'solo',
    status,
    createdAt: '2026-10-28T00:00:00Z',
    updatedAt: '2026-10-28T00:00:00Z',
    latestExecution: latest ? { id: latest.id, seq: latest.seq, mode: latest.mode, status: latest.status, errorClass: latest.errorClass } : null,
    prUrl: null,
    executions,
  }
}

function detailOf(item: ExecutionHistoryItem, extra: Partial<ExecutionDetail> = {}): ExecutionDetail {
  return {
    ...item,
    taskId: 'task-001',
    runnerId: 'local',
    role: 'developer',
    harnessId: 'claude-developer',
    profileId: 'p1',
    prUrl: null,
    contextRevisions: [],
    gateResults: [],
    ...extra,
  }
}

let TASK: TaskDetail
let EXECUTIONS: Record<string, ExecutionDetail>
let streams: FakeStream[]

beforeEach(() => {
  streams = []
  streamTiming.reconnectDelayMs = 1
  streamTiming.maxReconnectDelayMs = 4
})
afterEach(() => {
  streamTiming.reconnectDelayMs = 1000
  streamTiming.maxReconnectDelayMs = 5000
})

function renderTask(handler: (request: Recorded) => Response | Promise<Response> | undefined = () => undefined) {
  window.history.pushState(null, '', '/tasks/task-001')
  const fake = fakeFetch((request) => {
    if (request.url === '/api/session') return json(200, { csrfToken: 'csrf-1' })
    const handled = handler(request)
    if (handled) return handled
    if (request.url === '/api/tasks/task-001' && request.method === 'GET') return json(200, TASK)
    if (request.url === '/api/projects/alpha') return json(200, { id: 'alpha', name: 'Alpha アプリ' })
    const execution = /^\/api\/executions\/([^/]+)$/.exec(request.url)
    if (execution && EXECUTIONS[execution[1]]) return json(200, EXECUTIONS[execution[1]])
    if (/^\/api\/executions\/[^/]+\/events$/.test(request.url)) {
      const stream = new FakeStream()
      streams.push(stream)
      return stream.response
    }
    return json(500, {})
  })
  const events = fakeEvents()
  render(<App api={new ApiClient(fake.fetch, events.factory)} />)
  return { ...fake, events }
}

const logLines = () => within(screen.getByRole('log')).queryAllByText(/./, { selector: '[data-log-line]' }).map((el) => el.textContent)
const historyRows = () => within(screen.getByRole('table', { name: 'Execution 履歴' })).getAllByRole('row').slice(1)

describe('タスク詳細', () => {
  it('概要・Execution 履歴・固定した Context・検証結果を表示する', async () => {
    const item = history(E1, 1, 'blocked')
    TASK = taskWith('blocked', [item])
    EXECUTIONS = {
      [E1]: detailOf(item, {
        contextRevisions: [
          { id: 'c1', layer: 'project', kind: 'agents_md', sourcePath: 'AGENTS.md', sourceCommitSha: 'a'.repeat(40), contentHash: 'f'.repeat(64) },
        ],
        gateResults: [
          {
            id: 'g1',
            gate: 1,
            result: 'blocked',
            baseSha: '1'.repeat(40),
            targetSha: '2'.repeat(40),
            findings: [{ rule: 'existing_test_deleted', path: 'src/foo.test.ts', line: 42, message: '既存のテストファイルが削除された' }],
            createdAt: '2026-10-28T02:00:00Z',
          },
        ],
      }),
    }
    renderTask()
    expect(await screen.findByRole('heading', { name: 'ログイン画面' })).toBeInTheDocument()
    expect(screen.getByText('ログインできる')).toBeInTheDocument()
    expect(await screen.findByRole('link', { name: 'Alpha アプリ' })).toHaveAttribute('href', '/projects/alpha')
    expect(historyRows()).toHaveLength(1)
    expect(historyRows()[0]).toHaveTextContent('#1')
    expect(historyRows()[0]).toHaveTextContent('1.0.0 · abcdef0')
    const context = await screen.findByRole('table', { name: '固定した Context' })
    expect(context).toHaveTextContent('project')
    expect(context).toHaveTextContent('AGENTS.md')
    expect(context).toHaveTextContent('f'.repeat(64))
    const gate = screen.getByRole('listitem', { name: 'Gate 1' })
    expect(gate).toHaveTextContent('blocked')
    expect(gate).toHaveTextContent('src/foo.test.ts:42')
    expect(gate).toHaveTextContent('既存のテストファイルが削除された')
    // Blocked は中止と再実行ができる（04 3.2）。
    expect(screen.getByRole('button', { name: '停止（中止）' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '再実行（継続）' })).toBeInTheDocument()
  })

  it('実行中のログが、届くたびに画面を読み直さずに1行ずつ追記される', async () => {
    const item = history(E1, 1, 'running')
    TASK = taskWith('running', [item])
    EXECUTIONS = { [E1]: detailOf(item) }
    const { calls } = renderTask()
    await screen.findByRole('log')
    await waitFor(() => expect(streams).toHaveLength(1))
    expect(calls.find((c) => c.url === `/api/executions/${E1}/events`)?.headers['Last-Event-ID']).toBeUndefined()
    expect(logLines()).toEqual([])
    expect(screen.getByTestId('log-source')).toHaveTextContent('受信中')

    act(() => streams[0].log(10, 'harness', 'npm install'))
    await screen.findByText('npm install')
    expect(logLines()).toEqual(['npm install'])

    act(() => streams[0].log(11, 'harness', 'テストを実行しています'))
    await screen.findByText('テストを実行しています')
    expect(logLines()).toEqual(['npm install', 'テストを実行しています'])

    // stream で切り替える。別の stream の行は harness には出ない。
    act(() => streams[0].log(12, 'runner', 'worktree を準備しました'))
    await screen.findByRole('tab', { name: 'runner (1)' })
    expect(logLines()).toEqual(['npm install', 'テストを実行しています'])
    await userEvent.click(screen.getByRole('tab', { name: 'runner (1)' }))
    expect(logLines()).toEqual(['worktree を準備しました'])
    // 保存済みログは読まない（実行中は SSE で追記する）。
    expect(calls.some((c) => c.url.endsWith('/logs'))).toBe(false)
  })

  it('切断されると Last-Event-ID から再接続し、欠けも重複も無く続きが表示される', async () => {
    const item = history(E1, 1, 'running')
    TASK = taskWith('running', [item])
    EXECUTIONS = { [E1]: detailOf(item) }
    const { calls } = renderTask()
    await screen.findByRole('log')
    await waitFor(() => expect(streams).toHaveLength(1))
    act(() => {
      streams[0].log(20, 'harness', 'line 1')
      streams[0].log(21, 'harness', 'line 2')
    })
    await screen.findByText('line 2')

    act(() => streams[0].fail())
    await waitFor(() => expect(streams).toHaveLength(2))
    const eventRequests = calls.filter((c) => c.url === `/api/executions/${E1}/events`)
    expect(eventRequests[1].headers['Last-Event-ID']).toBe('21')

    // 切断中に出たログ（22, 23）は再接続後に届く。境目の 21 が重ねて届いても1回だけ表示する。
    act(() => {
      streams[1].log(21, 'harness', 'line 2')
      streams[1].log(22, 'harness', 'line 3')
      streams[1].log(23, 'harness', 'line 4')
    })
    await screen.findByText('line 4')
    expect(logLines()).toEqual(['line 1', 'line 2', 'line 3', 'line 4'])
    expect(screen.getByTestId('log-source')).toHaveTextContent('受信中')
  })

  it('終了した Execution は保存済みログを表示する', async () => {
    const item = history(E1, 1, 'failed', { errorClass: 'test_failed', finishedAt: '2026-10-28T03:00:00Z' })
    TASK = taskWith('failed', [item])
    EXECUTIONS = { [E1]: detailOf(item) }
    const { calls } = renderTask((request) =>
      request.url === `/api/executions/${E1}/logs` ? json(200, { harness: 'a\nb\n', setup: '', test: 'FAIL x\n', runner: 'done\n' }) : undefined,
    )
    await screen.findByText('b')
    expect(logLines()).toEqual(['a', 'b'])
    expect(screen.getByTestId('log-source')).toHaveTextContent('保存済み')
    await userEvent.click(screen.getByRole('tab', { name: 'test (1)' }))
    expect(logLines()).toEqual(['FAIL x'])
    await userEvent.click(screen.getByRole('tab', { name: 'setup (0)' }))
    expect(screen.getByRole('log')).toHaveTextContent('この stream の保存済みログはありません。')
    expect(streams).toHaveLength(0)
    expect(calls.some((c) => c.url.endsWith('/events'))).toBe(false)
    expect(historyRows()[0]).toHaveTextContent('test_failed')
  })

  it('表示中の Execution が running から終端状態になると、SSE の購読を止めて保存済みログへ切り替える', async () => {
    const item = history(E1, 1, 'running')
    TASK = taskWith('running', [item])
    EXECUTIONS = { [E1]: detailOf(item) }
    const { calls, events } = renderTask((request) =>
      request.url === `/api/executions/${E1}/logs` ? json(200, { harness: 'live 1\nlive 2\nsaved 3\n', setup: '', test: '', runner: 'runner done\n' }) : undefined,
    )
    await screen.findByRole('log')
    await waitFor(() => expect(streams).toHaveLength(1))
    act(() => {
      streams[0].log(40, 'harness', 'live 1')
      streams[0].log(41, 'harness', 'live 2')
    })
    await screen.findByText('live 2')
    expect(screen.getByTestId('log-source')).toHaveTextContent('受信中')
    expect(calls.some((c) => c.url.endsWith('/logs'))).toBe(false)

    // 同じ Execution が running → failed になる（status_changed を受けてタスクを読み直す）。
    const done = history(E1, 1, 'failed', { errorClass: 'harness_failed', finishedAt: '2026-10-28T04:00:00Z' })
    TASK = taskWith('failed', [done])
    EXECUTIONS = { [E1]: detailOf(done) }
    const source = events.open(`/api/executions/${E1}/events`)[0]
    act(() => source.emit('status_changed', { from: 'running', to: 'failed', reason: 'runner_result' }, 42))

    await waitFor(() => expect(screen.getByTestId('log-source')).toHaveTextContent('保存済みのログを表示しています'))
    await screen.findByText('saved 3')
    expect(logLines()).toEqual(['live 1', 'live 2', 'saved 3'])
    expect(calls.filter((c) => c.url === `/api/executions/${E1}/logs`)).toHaveLength(1)
    await userEvent.click(screen.getByRole('tab', { name: 'runner (1)' }))
    expect(logLines()).toEqual(['runner done'])

    // 購読は止めたので、元の接続が切れても再接続しない。
    act(() => streams[0].fail())
    await new Promise((resolve) => setTimeout(resolve, 30))
    expect(streams).toHaveLength(1)
    expect(calls.filter((c) => c.url === `/api/executions/${E1}/events`)).toHaveLength(1)
  })

  it('再実行（継続）で新しい Execution が履歴に増え、その Execution を表示する', async () => {
    const first = history(E1, 1, 'cancelled', { errorClass: 'cancelled', finishedAt: '2026-10-28T03:00:00Z' })
    TASK = taskWith('cancelled', [first])
    const second = history(E2, 2, 'pending', { profileVersion: null, profileCommitSha: null, startedAt: null })
    EXECUTIONS = { [E1]: detailOf(first), [E2]: detailOf(second) }
    const retries: Recorded[] = []
    renderTask((request) => {
      if (request.url === `/api/executions/${E1}/logs`) return json(200, { harness: '', setup: '', test: '', runner: '' })
      if (request.url === '/api/tasks/task-001/retry') {
        retries.push(request)
        TASK = taskWith('queued', [first, second])
        return new Response(null, { status: 204 })
      }
      return undefined
    })
    await screen.findByRole('heading', { name: 'ログ（#1）' })
    expect(historyRows()).toHaveLength(1)
    expect(screen.queryByRole('button', { name: '停止（中止）' })).not.toBeInTheDocument()

    await userEvent.click(screen.getByRole('button', { name: '再実行（継続）' }))
    expect(await screen.findByRole('status')).toHaveTextContent('再実行（継続）を受け付けました。')
    expect(retries).toHaveLength(1)
    expect(JSON.parse(retries[0].body!)).toEqual({ mode: 'continue' })
    expect(retries[0].headers['X-CSRF-Token']).toBe('csrf-1')

    await screen.findByRole('heading', { name: 'ログ（#2）' })
    expect(historyRows()).toHaveLength(2)
    expect(historyRows()[0]).toHaveTextContent('#2')
    expect(historyRows()[0]).toHaveTextContent('pending')
    expect(historyRows()[1]).toHaveTextContent('#1')
    // Queued になったので、中止はできるが再実行のボタンは出ない。
    expect(screen.getByRole('button', { name: '停止（中止）' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '再実行（継続）' })).not.toBeInTheDocument()
    // 古い Execution も選んで表示できる。
    await userEvent.click(screen.getByRole('button', { name: '#1 を表示' }))
    await screen.findByRole('heading', { name: 'ログ（#1）' })
  })

  it('再実行（最初から）は確認ダイアログを出し、確定したときだけ送る', async () => {
    const first = history(E1, 1, 'failed', { errorClass: 'test_failed' })
    TASK = taskWith('failed', [first])
    const second = history(E2, 2, 'pending', { mode: 'fresh' })
    EXECUTIONS = { [E1]: detailOf(first), [E2]: detailOf(second) }
    const retries: Recorded[] = []
    renderTask((request) => {
      if (request.url.endsWith('/logs')) return json(200, { harness: '', setup: '', test: '', runner: '' })
      if (request.url === '/api/tasks/task-001/retry') {
        retries.push(request)
        TASK = taskWith('queued', [first, second])
        return new Response(null, { status: 204 })
      }
      return undefined
    })
    await userEvent.click(await screen.findByRole('button', { name: '再実行（最初から）' }))
    const dialog = screen.getByRole('alertdialog', { name: '最初から再実行しますか' })
    await userEvent.click(within(dialog).getByRole('button', { name: 'やめる' }))
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
    expect(retries).toHaveLength(0)

    await userEvent.click(screen.getByRole('button', { name: '再実行（最初から）' }))
    await userEvent.click(within(screen.getByRole('alertdialog')).getByRole('button', { name: '最初から再実行する' }))
    await screen.findByRole('heading', { name: 'ログ（#2）' })
    expect(retries).toHaveLength(1)
    expect(JSON.parse(retries[0].body!)).toEqual({ mode: 'fresh' })
    expect(historyRows()).toHaveLength(2)
    expect(historyRows()[0]).toHaveTextContent('最初から')
  })

  it('中止を送り、409 なら状態が変わったことを表示して読み直す', async () => {
    const item = history(E1, 1, 'pending', { startedAt: null, profileVersion: null, profileCommitSha: null })
    TASK = taskWith('queued', [item])
    EXECUTIONS = { [E1]: detailOf(item) }
    let gets = 0
    const cancels: Recorded[] = []
    renderTask((request) => {
      if (request.url === '/api/tasks/task-001' && request.method === 'GET') gets += 1
      if (request.url === '/api/tasks/task-001/cancel') {
        cancels.push(request)
        return apiError(409, 'conflict', 'Task state does not allow cancel')
      }
      return undefined
    })
    await userEvent.click(await screen.findByRole('button', { name: '停止（中止）' }))
    expect(await screen.findByText('状態が変わりました')).toBeInTheDocument()
    expect(cancels).toHaveLength(1)
    expect(cancels[0].method).toBe('POST')
    expect(cancels[0].headers['X-CSRF-Token']).toBe('csrf-1')
    expect(gets).toBeGreaterThanOrEqual(2)
  })

  it('status_changed を受けると、画面を読み直さずに状態と履歴を更新する', async () => {
    const item = history(E1, 1, 'running')
    TASK = taskWith('running', [item])
    EXECUTIONS = { [E1]: detailOf(item) }
    const { events } = renderTask()
    await screen.findByRole('log')
    const source = events.open(`/api/executions/${E1}/events`)[0]
    expect(source).toBeDefined()
    const done = history(E1, 1, 'failed', { errorClass: 'harness_failed', finishedAt: '2026-10-28T04:00:00Z' })
    TASK = taskWith('failed', [done])
    EXECUTIONS = { [E1]: detailOf(done) }
    act(() => source.emit('status_changed', { from: 'running', to: 'failed', reason: 'runner_result' }, 30))
    await screen.findByText('harness_failed')
    expect(screen.getByRole('button', { name: '再実行（継続）' })).toBeInTheDocument()
  })
})
