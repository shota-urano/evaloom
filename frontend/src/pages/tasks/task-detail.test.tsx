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
    harnessId: '',
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
    contextRevisions: [],
    params: {},
    contractVersion: '1',
    resumeStage: null,
    stageRuns: [],
    checkResults: [],
    artifacts: [],
    ...extra,
  }
}

let TASK: TaskDetail
let EXECUTIONS: Record<string, ExecutionDetail>
let DECISIONS: unknown[]
let APPROVALS: unknown[]
let streams: FakeStream[]

beforeEach(() => {
  streams = []
  DECISIONS = []
  APPROVALS = []
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
    if (request.url.startsWith('/api/decisions?')) return json(200, { items: DECISIONS, nextCursor: null })
    if (request.url.startsWith('/api/approvals?')) return json(200, { items: APPROVALS, nextCursor: null })
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
  it('概要・Execution 履歴・固定した Context を表示し、PR・検証結果・Block の続行は出さない', async () => {
    const item = history(E1, 1, 'blocked')
    TASK = taskWith('blocked', [item])
    EXECUTIONS = {
      [E1]: detailOf(item, {
        contextRevisions: [
          { id: 'c1', layer: 'project', kind: 'agents_md', sourcePath: 'AGENTS.md', sourceCommitSha: 'a'.repeat(40), contentHash: 'f'.repeat(64) },
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
    const context = await screen.findByRole('table', { name: '固定した文脈' })
    expect(context).toHaveTextContent('project')
    expect(context).toHaveTextContent('AGENTS.md')
    expect(context).toHaveTextContent('f'.repeat(64))
    expect(screen.queryByText('PR')).toBeNull()
    expect(screen.queryByRole('heading', { name: /^検証結果/ })).toBeNull()
    expect(screen.queryByRole('button', { name: /続行/ })).toBeNull()
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

    act(() => streams[0].log(10, 'work', 'npm install'))
    await screen.findByText('npm install')
    expect(logLines()).toEqual(['npm install'])

    act(() => streams[0].log(11, 'work', 'テストを実行しています'))
    await screen.findByText('テストを実行しています')
    expect(logLines()).toEqual(['npm install', 'テストを実行しています'])

    // stream で切り替える。別の stream の行は work には出ない。
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
      streams[0].log(20, 'work', 'line 1')
      streams[0].log(21, 'work', 'line 2')
    })
    await screen.findByText('line 2')

    act(() => streams[0].fail())
    await waitFor(() => expect(streams).toHaveLength(2))
    const eventRequests = calls.filter((c) => c.url === `/api/executions/${E1}/events`)
    expect(eventRequests[1].headers['Last-Event-ID']).toBe('21')

    // 切断中に出たログ（22, 23）は再接続後に届く。境目の 21 が重ねて届いても1回だけ表示する。
    act(() => {
      streams[1].log(21, 'work', 'line 2')
      streams[1].log(22, 'work', 'line 3')
      streams[1].log(23, 'work', 'line 4')
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
    await userEvent.click(screen.getByRole('tab', { name: 'check (1)' }))
    expect(logLines()).toEqual(['FAIL x'])
    await userEvent.click(screen.getByRole('tab', { name: 'prepare (0)' }))
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
      streams[0].log(40, 'work', 'live 1')
      streams[0].log(41, 'work', 'live 2')
    })
    await screen.findByText('live 2')
    expect(screen.getByTestId('log-source')).toHaveTextContent('受信中')
    expect(calls.some((c) => c.url.endsWith('/logs'))).toBe(false)

    // 同じ Execution が running → failed になる（status_changed を受けてタスクを読み直す）。
    const done = history(E1, 1, 'failed', { errorClass: 'harness_failed', finishedAt: '2026-10-28T04:00:00Z' })
    TASK = taskWith('failed', [done])
    EXECUTIONS = { [E1]: detailOf(done) }
    // EventSource が開くまで待ってから取る（findByRole('log') の時点ではまだ開いていないことがある）。
    await waitFor(() => expect(events.open(`/api/executions/${E1}/events`)).toHaveLength(1))
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
    // EventSource が開くまで待ってから取る（findByRole('log') の時点ではまだ開いていないことがある）。
    await waitFor(() => expect(events.open(`/api/executions/${E1}/events`)).toHaveLength(1))
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

/** Linear から取り込んで保留したタスク（15-linear-intake 3.6）。Execution が無く、Project が決まっていないことがある。 */
function heldTask(cause: string, extra: Partial<TaskDetail> = {}): TaskDetail {
  return {
    ...taskWith('blocked', []),
    projectId: '',
    acceptanceCriteria: '',
    shortName: 'eng-123',
    source: 'linear',
    intakeBlockCause: cause,
    linear: { identifier: 'ENG-123', url: 'https://linear.app/acme/issue/ENG-123/login' },
    ...extra,
  }
}

describe('Linear 起因のタスク', () => {
  it('source=linear のタスクは概要に Issue の識別子とリンクを出す', async () => {
    const item = history(E1, 1, 'pending', { startedAt: null, profileVersion: null, profileCommitSha: null })
    TASK = { ...taskWith('queued', [item]), source: 'linear', intakeBlockCause: null, linear: { identifier: 'ENG-7', url: 'https://linear.app/acme/issue/ENG-7' } }
    EXECUTIONS = { [E1]: detailOf(item) }
    renderTask()
    const link = await screen.findByRole('link', { name: 'ENG-7' })
    expect(link).toHaveAttribute('href', 'https://linear.app/acme/issue/ENG-7')
    expect(link).toHaveAttribute('target', '_blank')
    // 保留していないタスクは今までどおり中止できる。
    expect(screen.getByRole('button', { name: '停止（中止）' })).toBeInTheDocument()
    expect(screen.queryByTestId('held-review')).not.toBeInTheDocument()
  })

  it('手動作成のタスクには Linear Issue の行を出さない', async () => {
    const item = history(E1, 1, 'pending', { startedAt: null, profileVersion: null, profileCommitSha: null })
    TASK = { ...taskWith('queued', [item]), source: 'manual', intakeBlockCause: null, linear: null }
    EXECUTIONS = { [E1]: detailOf(item) }
    renderTask()
    await screen.findByRole('heading', { name: 'ログイン画面' })
    expect(screen.queryByText('Linear Issue')).not.toBeInTheDocument()
    expect(screen.queryByTestId('linear-issue-link')).not.toBeInTheDocument()
  })

  it.each([
    ['missing_acceptance_criteria', '振り分け先の版が受け入れ条件を求めているのに、Issue に受け入れ条件が無い', 'Linear の Issue の本文に「受け入れ条件」の見出しで'],
    ['config_missing_profile', 'スペシャルハーネスへ直接振り分けたが、そのスペシャルハーネスに既定の版が無い', 'このスペシャルハーネスに既定の版を設定すると'],
    ['some_future_cause', 'some_future_cause', '保留の理由が解消されると'],
  ])('保留中（%s）は理由を文字で出し、続行・再実行を出さずに中止だけを出す', async (cause, label, hint) => {
    TASK = heldTask(cause)
    EXECUTIONS = {}
    const { calls } = renderTask()
    const held = await screen.findByTestId('held-review')
    expect(within(held).getByTestId('held-cause')).toHaveTextContent(label)
    expect(within(held).getByTestId('held-cause')).toHaveTextContent(cause)
    expect(within(held).getByTestId('held-hint')).toHaveTextContent(hint)
    expect(screen.queryByTestId('routing-review')).not.toBeInTheDocument()
    expect(within(held).getByRole('button', { name: '中止する' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /続行/ })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /再実行/ })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '停止（中止）' })).not.toBeInTheDocument()
    expect(screen.getAllByRole('button', { name: /中止/ })).toHaveLength(1)
    // Project が決まっていないので Project を読みに行かない。
    expect(calls.some((c) => c.url.startsWith('/api/projects'))).toBe(false)
    expect(screen.getByRole('link', { name: 'ENG-123' })).toHaveAttribute('href', 'https://linear.app/acme/issue/ENG-123/login')
  })

  it('config_missing_profile の保留では、既定の版を設定するスペシャルハーネスへのリンクを出す', async () => {
    TASK = heldTask('config_missing_profile', { harnessId: 'web' })
    EXECUTIONS = {}
    renderTask((request) => (request.url === '/api/harnesses/web' ? json(200, { id: 'web', name: 'Web ハーネス' }) : undefined))
    const held = await screen.findByTestId('held-review')
    expect(within(held).getByRole('link', { name: 'スペシャルハーネスを開く' })).toHaveAttribute('href', '/harnesses/web')
  })

  it('振り分けを待つタスクは、そのことと振り分けの判断待ちへのリンクを出し、中止できる', async () => {
    TASK = { ...heldTask('missing_acceptance_criteria'), status: 'routing', intakeBlockCause: null }
    EXECUTIONS = {}
    DECISIONS = [
      { id: 'D7', kind: 'route', taskId: 'task-001', executionId: null, subjectId: 'P7', cause: null, status: 'open', detail: {}, createdAt: '2026-10-28T03:00:00Z' },
    ]
    renderTask()
    const routing = await screen.findByTestId('routing-review')
    expect(within(routing).getByRole('heading', { name: '振り分けを待っています' })).toBeInTheDocument()
    expect(await within(routing).findByRole('link', { name: '振り分けの判断待ちを開く' })).toHaveAttribute('href', '/decisions/D7')
    expect(screen.queryByTestId('held-review')).not.toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'ENG-123' })).toHaveAttribute('href', 'https://linear.app/acme/issue/ENG-123/login')
    expect(screen.getByRole('button', { name: '停止（中止）' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /続行|再実行/ })).not.toBeInTheDocument()
  })

  it('振り分けを待つタスクで判断待ちがまだ無ければ、提案を作っていることを出す', async () => {
    TASK = { ...heldTask('missing_acceptance_criteria'), status: 'routing', intakeBlockCause: null }
    EXECUTIONS = {}
    renderTask()
    const routing = await screen.findByTestId('routing-review')
    expect(await within(routing).findByTestId('routing-pending')).toHaveTextContent('振り分けの提案を作っています。提案ができると判断待ちが開きます。')
    expect(within(routing).queryByRole('link', { name: '振り分けの判断待ちを開く' })).not.toBeInTheDocument()
  })

  it('保留中のタスクを中止すると cancel を送り、中止済みになって再実行も出さない', async () => {
    TASK = heldTask('missing_acceptance_criteria')
    EXECUTIONS = {}
    const cancels: Recorded[] = []
    renderTask((request) => {
      if (request.url === '/api/tasks/task-001/cancel') {
        cancels.push(request)
        TASK = { ...TASK, status: 'cancelled' }
        return new Response(null, { status: 204 })
      }
      return undefined
    })
    await userEvent.click(await within(await screen.findByTestId('held-review')).findByRole('button', { name: '中止する' }))
    expect(await screen.findByText('中止を受け付けました。')).toBeInTheDocument()
    expect(cancels).toHaveLength(1)
    expect(cancels[0].method).toBe('POST')
    await waitFor(() => expect(screen.queryByTestId('held-review')).not.toBeInTheDocument())
    expect(screen.getByText('Cancelled')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /再実行/ })).not.toBeInTheDocument()
  })

  it('保留中のタスクの中止が 409 なら状態が変わったことを表示して読み直す', async () => {
    TASK = heldTask('config_missing_profile', { harnessId: 'web' })
    EXECUTIONS = {}
    let gets = 0
    renderTask((request) => {
      if (request.url === '/api/tasks/task-001' && request.method === 'GET') gets += 1
      if (request.url === '/api/tasks/task-001/cancel') return apiError(409, 'conflict', 'Task state does not allow cancel')
      return undefined
    })
    await userEvent.click(await within(await screen.findByTestId('held-review')).findByRole('button', { name: '中止する' }))
    expect(await screen.findByText('状態が変わりました')).toBeInTheDocument()
    await waitFor(() => expect(gets).toBeGreaterThanOrEqual(2))
  })
})

describe('タスク詳細の区画（13 3.3）', () => {
  it('概要に最新の Execution の outputs と、このタスクの開いている判断待ちへのリンクを出す', async () => {
    const first = history(E1, 1, 'failed', { finishedAt: '2026-10-28T02:00:00Z', outputs: [{ kind: 'pr', label: '古い PR', url: 'https://example.com/old' }] })
    const second = history(E2, 2, 'waiting_approval', { outputs: [{ kind: 'pull_request', label: 'PR #7', url: 'https://github.com/acme/app/pull/7' }] })
    TASK = taskWith('waiting_approval', [first, second])
    EXECUTIONS = { [E1]: detailOf(first), [E2]: detailOf(second) }
    DECISIONS = [
      { id: 'D1', kind: 'approval', taskId: 'task-001', executionId: E2, subjectId: 'A1', cause: null, status: 'open', detail: {}, createdAt: '2026-10-28T03:00:00Z' },
      { id: 'D2', kind: 'question', taskId: 'task-999', executionId: 'X', subjectId: null, cause: 'question', status: 'open', detail: {}, createdAt: '2026-10-28T03:00:00Z' },
    ]
    const { calls } = renderTask()
    const outputs = await screen.findByRole('list', { name: '外に出したもの' })
    expect(within(outputs).getByRole('link', { name: 'PR #7' })).toHaveAttribute('href', 'https://github.com/acme/app/pull/7')
    expect(screen.queryByRole('link', { name: '古い PR' })).toBeNull()
    const decisions = await screen.findByRole('list', { name: '開いている判断待ち' })
    expect(within(decisions).getByRole('link', { name: '操作の承認' })).toHaveAttribute('href', '/decisions/D1')
    expect(within(decisions).getAllByRole('link')).toHaveLength(1)
    expect(calls.some((c) => c.url.startsWith('/api/decisions?status=open'))).toBe(true)
  })

  it('段の結果・確かめた結果・承認・固定した文脈・成果物・渡した値を出す', async () => {
    const item = history(E1, 1, 'succeeded', { finishedAt: '2026-10-28T03:00:00Z' })
    TASK = taskWith('completed', [item])
    EXECUTIONS = {
      [E1]: detailOf(item, {
        params: { branch: 'main' },
        contractVersion: '1',
        resumeStage: 'publish',
        stageRuns: [
          { id: 's1', stage: 'work', attempt: 1, status: 'succeeded', errorClass: null, startedAt: '2026-10-28T01:00:00Z', finishedAt: '2026-10-28T02:00:00Z', message: '実装しました', summary: 'ログイン画面' },
          { id: 's2', stage: 'check', attempt: 2, status: 'failed', errorClass: 'check_failed', startedAt: '2026-10-28T02:00:00Z', finishedAt: null, message: null, summary: null },
        ],
        checkResults: [
          { id: 'c1', result: 'fail', contentId: 'sha-1', summary: 'lint が落ちた', findings: [{ rule: 'no-unused', severity: 'error', message: '未使用の変数', location: { path: 'src/a.ts', line: 3 } }] },
        ],
        contextRevisions: [
          { id: 'r1', layer: 'reported', kind: 'file', sourcePath: 'NOTES.md', sourceCommitSha: 'b'.repeat(40), contentHash: 'e'.repeat(64) },
        ],
        artifacts: [
          { id: 'A1', kind: 'work_summary', stage: 'work', name: 'out/summary.md' },
          { id: 'A2', kind: 'result_json', stage: 'check', name: 'result.json' },
        ],
      }),
    }
    APPROVALS = [
      { id: 'P1', decisionId: 'D1', taskId: 'task-001', executionId: E1, requestedStage: 'publish', operation: 'merge_pull_request', target: { label: 'PR #7', url: 'https://example.com/pr/7' }, summary: null, contentId: 'sha-1', status: 'consumed', invalidatedReason: null, decidedAt: '2026-10-28T02:30:00Z', expiresAt: null, consumedAt: '2026-10-28T02:40:00Z', createdAt: '2026-10-28T02:00:00Z' },
      { id: 'P2', decisionId: 'D9', taskId: 'task-999', executionId: 'X', requestedStage: 'publish', operation: 'deploy', target: {}, summary: null, contentId: 'other', status: 'pending', invalidatedReason: null, decidedAt: null, expiresAt: null, consumedAt: null, createdAt: '2026-10-28T02:00:00Z' },
    ]
    renderTask((request) => (request.url === `/api/executions/${E1}/logs` ? json(200, { harness: '', setup: '', test: '', publish: '', runner: '' }) : undefined))
    const stages = await screen.findByRole('table', { name: '段の結果' })
    expect(within(stages).getAllByRole('row')).toHaveLength(3)
    expect(stages).toHaveTextContent('実装しました')
    expect(stages).toHaveTextContent('ログイン画面')
    expect(stages).toHaveTextContent('check_failed')
    expect(screen.getByTestId('execution-params')).toHaveTextContent('"branch": "main"')
    expect(screen.getByText('publish', { selector: 'dd span' })).toBeInTheDocument()
    const check = screen.getByRole('article', { name: '確かめた結果 c1' })
    expect(check).toHaveTextContent('lint が落ちた')
    expect(check).toHaveTextContent('sha-1')
    expect(within(check).getByRole('table', { name: 'finding' })).toHaveTextContent('src/a.ts:3')
    expect(screen.getByRole('table', { name: '固定した文脈' })).toHaveTextContent('reported（記録のみ）')
    const artifacts = screen.getByRole('table', { name: '成果物' })
    expect(within(artifacts).getByRole('link', { name: 'out/summary.md' })).toHaveAttribute('href', `/api/executions/${E1}/artifacts/A1`)
    expect(artifacts).toHaveTextContent('作業の概要')
    expect(artifacts).toHaveTextContent('結果 JSON')
    const approvals = await screen.findByRole('table', { name: '承認' })
    expect(within(approvals).getAllByRole('row')).toHaveLength(2)
    expect(within(approvals).getByRole('link', { name: 'PR #7' })).toHaveAttribute('href', 'https://example.com/pr/7')
    expect(approvals).toHaveTextContent('consumed')
  })

  it('旧名の stream（harness / setup / test）で届いたログは work / prepare / check に出す', async () => {
    const item = history(E1, 1, 'working')
    TASK = taskWith('running', [item])
    EXECUTIONS = { [E1]: detailOf(item) }
    renderTask()
    await screen.findByRole('log')
    await waitFor(() => expect(streams).toHaveLength(1))
    act(() => {
      streams[0].log(1, 'harness', '作業')
      streams[0].log(2, 'setup', '準備')
      streams[0].log(3, 'test', '確認')
      streams[0].log(4, 'publish', '出す')
    })
    await screen.findByText('作業')
    for (const [tab, line] of [
      ['prepare (1)', '準備'],
      ['check (1)', '確認'],
      ['publish (1)', '出す'],
      ['work (1)', '作業'],
    ]) {
      await userEvent.click(screen.getByRole('tab', { name: tab }))
      expect(logLines()).toEqual([line])
    }
  })
})
