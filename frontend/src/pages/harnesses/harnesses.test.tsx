import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, expect, it } from 'vitest'
import { ApiClient } from '@/api/client'
import type { Harness, HarnessProfile } from '@/api/registry'
import { fakeFetch, json, type Recorded } from '@/test/fake-fetch'
import App from '@/App'

const T = '2026-10-27T00:00:00Z'
const HARNESS: Harness = { id: 'engineer', name: 'Engineer', repoPath: '/repos/engineer', repoUrl: 'https://example.invalid/h.git', defaultProfileId: null, approvalWaivers: [], createdAt: T, updatedAt: T }
const PROFILE: HarnessProfile = {
  id: '01JE2E0P0000000000000000P1',
  harnessId: 'engineer',
  version: '1.0.0',
  commitSha: 'a'.repeat(40),
  contractVersion: '1',
  modelId: 'model-x',
  operationStatus: 'provisional',
  registeredAt: T,
  definition: {
    contractVersion: '1',
    description: 'PR を作るハーネス',
    acceptanceCriteria: 'required',
    stages: {
      work: { microHarness: 'claude_code', model: 'model-x', timeoutSec: 600 },
      check: { command: ['evaloom/check/run'], timeoutSec: 60 },
      publish: { command: ['evaloom/publish/run'], timeoutSec: 120, network: { allow: ['api.github.com:443'] }, secrets: [{ slot: 'github_token', access: 'write' }] },
    },
    approvals: [
      { operation: 'merge_pull_request', description: 'PR をマージする', waivable: true },
      { operation: 'delete_branch', description: 'ブランチを消す' },
    ],
    records: { keep: ['notes.md'], pass: { recent: 3 } },
  },
}
const page = (items: unknown[]) => ({ items, nextCursor: '' })

function renderAt(path: string, handler: (r: Recorded) => Response) {
  window.history.pushState(null, '', path)
  const fake = fakeFetch((r) => (r.url === '/api/session' ? json(200, { csrfToken: 't' }) : handler(r)))
  render(<App api={new ApiClient(fake.fetch)} />)
  return fake
}
function api(harness: Harness, onPatch?: (r: Recorded) => Response) {
  return (r: Recorded) => {
    if (r.method === 'PATCH') return onPatch ? onPatch(r) : json(200, harness)
    if (r.url.startsWith('/api/harnesses/engineer')) return json(200, harness)
    if (r.url.startsWith('/api/harness-profiles')) return json(200, page([PROFILE]))
    return json(200, page([]))
  }
}
afterEach(() => window.history.pushState(null, '', '/'))

it('スペシャルハーネスはリポジトリの情報だけを登録する', async () => {
  const fake = renderAt('/harnesses/_new', (r) => (r.method === 'POST' ? json(201, HARNESS) : api(HARNESS)(r)))
  await userEvent.type(await screen.findByLabelText('スペシャルハーネス ID'), 'engineer')
  await userEvent.type(screen.getByLabelText('名称'), 'Engineer')
  await userEvent.type(screen.getByLabelText('ハーネスリポジトリのパス'), '/repos/engineer')
  await userEvent.type(screen.getByLabelText('ハーネスリポジトリ URL'), 'https://example.invalid/h.git')
  for (const label of ['Adapter', '専門分野', 'Tool Permissions', '対応モデル', '既定の版']) expect(screen.queryByLabelText(label)).toBeNull()
  await userEvent.click(screen.getByRole('button', { name: '登録' }))
  expect(JSON.parse(fake.calls.find((r) => r.method === 'POST')?.body ?? '')).toEqual({ id: 'engineer', name: 'Engineer', repoPath: '/repos/engineer', repoUrl: 'https://example.invalid/h.git' })
})

it('編集で既定の版を選び、waivable の操作だけを確認のうえ省く設定に足せる', async () => {
  const fake = renderAt('/harnesses/engineer/edit', api(HARNESS))
  await userEvent.selectOptions(await screen.findByLabelText('既定の版'), PROFILE.id)
  expect(screen.getByRole('checkbox', { name: /merge_pull_request/ })).toBeTruthy()
  expect(screen.queryByRole('checkbox', { name: /delete_branch/ })).toBeNull()
  await userEvent.click(screen.getByRole('checkbox', { name: /merge_pull_request/ }))
  await userEvent.click(within(screen.getByRole('alertdialog')).getByRole('button', { name: '承認を省く' }))
  await userEvent.click(screen.getByRole('button', { name: '保存' }))
  expect(JSON.parse(fake.calls.find((r) => r.method === 'PATCH')?.body ?? '')).toEqual({
    name: 'Engineer', repoPath: '/repos/engineer', repoUrl: 'https://example.invalid/h.git', defaultProfileId: PROFILE.id, approvalWaivers: ['merge_pull_request'],
  })
})

it('詳細に版の宣言（段ごとの上限時間・許可宛先・鍵の名前、承認と waivable）が出て、鍵の値の欄は無い', async () => {
  renderAt('/harnesses/engineer', api(HARNESS))
  const stages = await screen.findByRole('table', { name: '段ごとの宣言' })
  expect(within(stages).getByText('120 秒')).toBeTruthy()
  expect(within(stages).getByText('api.github.com:443')).toBeTruthy()
  expect(within(stages).getByText('github_token')).toBeTruthy()
  expect(within(stages).getByText(/モデルの API だけ/)).toBeTruthy()
  const approvals = screen.getByRole('table', { name: '承認が要る操作' })
  expect(within(approvals).getByText('省ける（waivable: true）')).toBeTruthy()
  expect(within(approvals).getByText('省けない（waivable: false）')).toBeTruthy()
  expect(screen.getByText('必須（required）')).toBeTruthy()
  expect(screen.queryByText(/鍵の値/)).toBeNull()
  expect(document.querySelector('input[type=password]')).toBeNull()
})

it('詳細の「既定の版にする」で defaultProfileId だけを送る', async () => {
  const fake = renderAt('/harnesses/engineer', api(HARNESS, () => json(200, { ...HARNESS, defaultProfileId: PROFILE.id })))
  await userEvent.click(await screen.findByRole('button', { name: '既定の版にする' }))
  expect(JSON.parse(fake.calls.find((r) => r.method === 'PATCH')?.body ?? '')).toEqual({ defaultProfileId: PROFILE.id })
  expect(await screen.findByText(/既定の版を 1.0.0 にしました/)).toBeTruthy()
})
