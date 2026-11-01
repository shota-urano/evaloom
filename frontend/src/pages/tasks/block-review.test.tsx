import { act, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it } from 'vitest'

import { ApiClient } from '@/api/client'
import type { ExecutionDetail, ExecutionHistoryItem, GateResult, TaskDetail } from '@/api/tasks'
import { SessionProvider } from '@/auth/session'
import { fakeEvents } from '@/test/fake-events'
import { apiError, fakeFetch, json, type Recorded } from '@/test/fake-fetch'
import { FakeStream } from '@/test/fake-stream'
import App from '@/App'
import type { ArtifactSource } from './block'
import { TaskDetailPage } from './task-detail-page'

// evaloom-52y.6: Blocked の確認と続行・中止（13 3.4、04 3.8）。

const E1 = '01HX0000000000000000000001'
const BLOCK = '01HX00000000000000000000B1'
const HEAD = 'c'.repeat(40)

const item: ExecutionHistoryItem = {
  id: E1,
  seq: 1,
  mode: 'continue',
  status: 'blocked',
  errorClass: null,
  profileVersion: '1.0.0',
  profileCommitSha: 'abcdef0123456789abcdef0123456789abcdef01',
  startedAt: '2026-10-28T01:00:00Z',
  finishedAt: null,
}

function taskWith(status: TaskDetail['status'], execution: ExecutionHistoryItem): TaskDetail {
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
    latestExecution: { id: execution.id, seq: execution.seq, mode: execution.mode, status: execution.status, errorClass: execution.errorClass },
    prUrl: null,
    executions: [execution],
  }
}

const gate2: GateResult = {
  id: 'g2',
  gate: 2,
  result: 'blocked',
  baseSha: 'a'.repeat(40),
  targetSha: HEAD,
  findings: [{ rule: 'existing_test_not_run', path: 'src/login.test.ts', line: 12, message: '既存のテスト "login works" が実行されていない' }],
  createdAt: '2026-10-28T02:00:00Z',
}

function detail(gateResults: GateResult[]): ExecutionDetail {
  return { ...item, taskId: 'task-001', runnerId: 'local', role: 'developer', harnessId: 'h', profileId: 'p', prUrl: null, contextRevisions: [], gateResults }
}

let TASK: TaskDetail
let EXECUTION: ExecutionDetail

beforeEach(() => {
  TASK = taskWith('blocked', item)
  EXECUTION = detail([gate2])
})

function setup(handler: (request: Recorded) => Response | undefined = () => undefined, artifactSource?: ArtifactSource) {
  window.history.pushState(null, '', '/tasks/task-001')
  const fake = fakeFetch((request) => {
    if (request.url === '/api/session') return json(200, { csrfToken: 'csrf-1' })
    const handled = handler(request)
    if (handled) return handled
    if (request.url === '/api/tasks/task-001' && request.method === 'GET') return json(200, TASK)
    if (request.url === '/api/projects/alpha') return json(200, { id: 'alpha', name: 'Alpha アプリ' })
    if (request.url === `/api/executions/${E1}`) return json(200, EXECUTION)
    if (request.url === `/api/executions/${E1}/events`) return new FakeStream().response
    return json(500, {})
  })
  const events = fakeEvents()
  const api = new ApiClient(fake.fetch, events.factory)
  if (artifactSource) {
    render(
      <SessionProvider api={api}>
        <TaskDetailPage id="task-001" artifactSource={artifactSource} />
      </SessionProvider>,
    )
  } else {
    render(<App api={api} />)
  }
  /** Execution の SSE に blocked イベントを流す（02 5.3 の payload は cause と blockId）。 */
  async function block(cause: string) {
    await waitFor(() => expect(events.open(`/api/executions/${E1}/events`)).toHaveLength(1))
    act(() => events.open(`/api/executions/${E1}/events`)[0].emit('blocked', { cause, blockId: BLOCK }, 5))
  }
  return { ...fake, block }
}

const panel = () => screen.getByRole('heading', { name: 'Blocked の確認（#1）' }).closest('[data-slot=card]') as HTMLElement

describe('Blocked の確認と続行・中止', () => {
  it('Block 原因・確認する HEAD SHA・Finding を表示し、続行で表示中の headSha を送る', async () => {
    const resumes: Recorded[] = []
    const { block } = setup((request) => {
      if (request.url === '/api/tasks/task-001/resume') {
        resumes.push(request)
        TASK = taskWith('queued', { ...item, status: 'pending' })
        return new Response(null, { status: 204 })
      }
      return undefined
    })
    await screen.findByRole('heading', { name: 'Blocked の確認（#1）' })
    expect(within(panel()).getByRole('status')).toHaveTextContent('Block の情報を受信しています')
    expect(within(panel()).queryByRole('button', { name: '続行する' })).not.toBeInTheDocument()

    await block('gate2_missing_tests')
    expect(await within(panel()).findByText('gate2_missing_tests')).toBeInTheDocument()
    expect(within(panel()).getByTestId('block-head-sha')).toHaveTextContent(HEAD)
    const findings = within(panel()).getByRole('list', { name: 'Finding' })
    expect(findings).toHaveTextContent('src/login.test.ts:12')
    expect(findings).toHaveTextContent('既存のテスト "login works" が実行されていない')
    // 差分の一覧を返す API が無いときは、その旨を出す。
    expect(panel()).toHaveTextContent('成果物（差分）の一覧を返す API が無いため')

    await userEvent.click(within(panel()).getByRole('button', { name: '続行する' }))
    expect(await screen.findByText(/続行を受け付けました/)).toBeInTheDocument()
    expect(resumes).toHaveLength(1)
    expect(JSON.parse(resumes[0].body!)).toEqual({ blockId: BLOCK, decision: 'continue', headSha: HEAD })
    expect(resumes[0].headers['X-CSRF-Token']).toBe('csrf-1')
    // Queued に戻ったので確認画面は消える。
    await waitFor(() => expect(screen.queryByRole('heading', { name: 'Blocked の確認（#1）' })).not.toBeInTheDocument())
  })

  it('中止は decision=abort と表示中の headSha を送る', async () => {
    const resumes: Recorded[] = []
    const { block } = setup((request) => {
      if (request.url === '/api/tasks/task-001/resume') {
        resumes.push(request)
        TASK = taskWith('cancelled', { ...item, status: 'cancelled' })
        return new Response(null, { status: 204 })
      }
      return undefined
    })
    await screen.findByRole('heading', { name: 'Blocked の確認（#1）' })
    await block('gate2_missing_tests')
    await userEvent.click(await within(panel()).findByRole('button', { name: '中止する' }))
    expect(await screen.findByText('中止を受け付けました。')).toBeInTheDocument()
    expect(JSON.parse(resumes[0].body!)).toEqual({ blockId: BLOCK, decision: 'abort', headSha: HEAD })
  })

  it('Gate 3 の Block では Gate 3 の結果の SHA を表示して送る', async () => {
    const gate3: GateResult = { ...gate2, id: 'g3', gate: 3, targetSha: 'd'.repeat(40), findings: [{ rule: 'uncommitted', path: 'README.md', line: 0, message: '未コミットの変更' }], createdAt: '2026-10-28T03:00:00Z' }
    EXECUTION = detail([{ ...gate2, result: 'pass', findings: [] }, gate3])
    const resumes: Recorded[] = []
    const { block } = setup((request) => {
      if (request.url === '/api/tasks/task-001/resume') {
        resumes.push(request)
        return new Response(null, { status: 204 })
      }
      return undefined
    })
    await screen.findByRole('heading', { name: 'Blocked の確認（#1）' })
    await block('gate3_mismatch')
    expect(await within(panel()).findByTestId('block-head-sha')).toHaveTextContent('d'.repeat(40))
    expect(within(panel()).getByRole('list', { name: 'Finding' })).toHaveTextContent('未コミットの変更')
    await userEvent.click(within(panel()).getByRole('button', { name: '続行する' }))
    await waitFor(() => expect(resumes).toHaveLength(1))
    expect(JSON.parse(resumes[0].body!).headSha).toBe('d'.repeat(40))
  })

  it.each(['context_conflict', 'git_state'])('%s の Block では続行ボタンを出さず、中止と再実行だけを出す', async (cause) => {
    EXECUTION = detail([])
    const { block, calls } = setup()
    await screen.findByRole('heading', { name: 'Blocked の確認（#1）' })
    await block(cause)
    expect(await within(panel()).findByText(cause)).toBeInTheDocument()
    expect(panel()).toHaveTextContent('この原因の Block は続行できません')
    expect(screen.queryByRole('button', { name: '続行する' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /続行/ })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: '停止（中止）' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '再実行（継続）' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '再実行（最初から）' })).toBeInTheDocument()
    expect(calls.some((c) => c.url.endsWith('/resume'))).toBe(false)
  })

  it('422（SHA 不一致）なら内容が変わったことを表示し、読み直させる', async () => {
    let gets = 0
    const { block } = setup((request) => {
      if (request.url === `/api/executions/${E1}`) gets += 1
      if (request.url === '/api/tasks/task-001/resume') return apiError(422, 'precondition_failed', "Head SHA must equal the Block's recorded SHA")
      return undefined
    })
    await screen.findByRole('heading', { name: 'Blocked の確認（#1）' })
    await block('gate2_unverifiable')
    await userEvent.click(await within(panel()).findByRole('button', { name: '続行する' }))
    const stale = await screen.findByTestId('block-stale')
    expect(stale).toHaveTextContent('内容が変わりました')
    expect(stale).toHaveTextContent('画面を読み直して')
    const before = gets
    // 読み直すと、最新の内容（別の SHA）が表示される。
    EXECUTION = detail([{ ...gate2, targetSha: 'e'.repeat(40) }])
    await userEvent.click(within(stale).getByRole('button', { name: '画面を読み直す' }))
    await waitFor(() => expect(gets).toBeGreaterThan(before))
    await waitFor(() => expect(within(panel()).getByTestId('block-head-sha')).toHaveTextContent('e'.repeat(40)))
    expect(screen.queryByTestId('block-stale')).not.toBeInTheDocument()
  })

  it('成果物の一覧から差分を取得して表示する', async () => {
    const source: ArtifactSource = () => [{ id: 'A1', kind: 'diff' }]
    const { block, calls } = setup((request) => {
      if (request.url === `/api/executions/${E1}/artifacts/A1`) return new Response('--- a/login.ts\n+++ b/login.ts\n+export const ok = true\n', { status: 200 })
      return undefined
    }, source)
    await screen.findByRole('heading', { name: 'Blocked の確認（#1）' })
    await block('gate2_missing_tests')
    const diff = await screen.findByLabelText('差分')
    expect(diff).toHaveTextContent('+export const ok = true')
    expect(calls.filter((c) => c.url === `/api/executions/${E1}/artifacts/A1`)).toHaveLength(1)
  })
})
