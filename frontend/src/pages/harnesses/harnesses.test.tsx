import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it } from 'vitest'

import { ApiClient } from '@/api/client'
import type { Harness, HarnessProfile, Project } from '@/api/registry'
import { apiError, fakeFetch, json, type Recorded } from '@/test/fake-fetch'
import App from '@/App'

const HARNESS: Harness = {
  id: 'claude-dev',
  name: 'Claude Developer',
  specialty: 'developer',
  supportedTasks: ['development'],
  adapter: 'claude_code',
  repoPath: '/repos/harness',
  repoUrl: 'git@example.com:harness.git',
  supportedModels: ['model-a'],
  availableTools: [],
  toolPermissions: {},
  contractVersion: '1',
  createdAt: '2026-10-27T00:00:00Z',
  updatedAt: '2026-10-27T00:00:00Z',
}

const SHA = '0123456789abcdef0123456789abcdef01234567'
const OTHER_SHA = 'fedcba9876543210fedcba9876543210fedcba98'

const profile = (version: string, commitSha: string): HarnessProfile => ({
  id: `p-${version}`,
  harnessId: HARNESS.id,
  version,
  commitSha,
  modelId: 'model-a',
  operationStatus: 'provisional',
  registeredAt: '2026-10-27T00:00:00Z',
})

const page = <T,>(items: T[]) => json(200, { items, nextCursor: '' })

const PROJECT = {
  id: 'alpha',
  name: 'Alpha',
  harnesses: [{ projectId: 'alpha', role: 'developer', harnessId: HARNESS.id, profileId: 'p-1.0.0', version: '1.0.0', commitSha: SHA, assignedAt: '' }],
} as unknown as Project

function renderAt(path: string, handler: (request: Recorded) => Response | undefined) {
  window.history.pushState(null, '', path)
  const fake = fakeFetch((request) => {
    if (request.url === '/api/session') return json(200, { csrfToken: 't' })
    return handler(request) ?? (request.url.startsWith('/api/projects') ? page([]) : json(500, {}))
  })
  render(<App api={new ApiClient(fake.fetch)} />)
  return fake
}

function fieldOf(label: string) {
  return screen.getByLabelText(label).closest('[data-field]') as HTMLElement
}

afterEach(() => window.history.pushState(null, '', '/'))

describe('Harness の登録', () => {
  it('必須項目が空なら送信せず、項目ごとにエラーを出す', async () => {
    const fake = renderAt('/harnesses/_new', () => undefined)
    await userEvent.click(await screen.findByRole('button', { name: '登録' }))
    for (const label of ['Harness ID', '名称', 'ハーネスリポジトリのパス', 'ハーネスリポジトリ URL']) {
      expect(within(fieldOf(label)).getByText('入力してください')).toBeVisible()
    }
    expect(fieldOf('Adapter').querySelector('[data-slot=field-error]')).toHaveTextContent('選択してください')
    expect(fake.calls.some((c) => c.method === 'POST')).toBe(false)
  })

  it('API の項目名で送り、400 の details は項目に、422 のパス検証の理由は項目と一覧に出す', async () => {
    let response = apiError(400, 'validation_failed', 'invalid request', [{ field: 'adapter', reason: 'invalid' }])
    const fake = renderAt('/harnesses/_new', (request) => (request.method === 'POST' ? response : undefined))
    await userEvent.type(await screen.findByLabelText('Harness ID'), 'claude-dev')
    await userEvent.type(screen.getByLabelText('名称'), 'Claude Developer')
    await userEvent.selectOptions(screen.getByLabelText('Adapter'), 'claude_code')
    await userEvent.type(screen.getByLabelText('ハーネスリポジトリのパス'), '/repos/harness')
    await userEvent.type(screen.getByLabelText('ハーネスリポジトリ URL'), 'git@example.com:harness.git')
    await userEvent.type(screen.getByLabelText('対応モデル'), 'model-a{enter}{enter}model-b')
    await userEvent.click(screen.getByRole('button', { name: '登録' }))
    expect(await within(fieldOf('Adapter')).findByText('値が正しくありません')).toBeVisible()
    expect(JSON.parse(fake.calls.find((c) => c.method === 'POST')?.body ?? '')).toEqual({
      id: 'claude-dev',
      name: 'Claude Developer',
      adapter: 'claude_code',
      repoPath: '/repos/harness',
      repoUrl: 'git@example.com:harness.git',
      supportedModels: ['model-a', 'model-b'],
      availableTools: [],
      toolPermissions: {},
    })

    response = apiError(422, 'precondition_failed', 'repository validation failed', [{ field: 'path', reason: 'not_git_repository' }])
    await userEvent.click(screen.getByRole('button', { name: '登録' }))
    expect(await within(fieldOf('ハーネスリポジトリのパス')).findByText('Git リポジトリではありません')).toBeVisible()
    expect(screen.getByRole('alert')).toHaveTextContent('ハーネスリポジトリのパス: Git リポジトリではありません')
    expect(within(fieldOf('Adapter')).queryByText('値が正しくありません')).toBeNull()
  })
})

describe('Harness ID「new」（slug として正当な値）', () => {
  it('/harnesses/new は登録画面ではなく詳細を開き、登録画面は /harnesses/_new にある', async () => {
    const harness = { ...HARNESS, id: 'new', name: 'New という名前の Harness' }
    renderAt('/harnesses', (request) => {
      if (request.url.startsWith('/api/harnesses')) return page([harness])
      if (request.url.startsWith('/api/harness-profiles')) return page([])
      return undefined
    })
    await userEvent.click(await screen.findByRole('link', { name: 'Harness を登録' }))
    expect(window.location.pathname).toBe('/harnesses/_new')
    expect(await screen.findByRole('heading', { name: 'Harness の登録' })).toBeVisible()

    await userEvent.click(screen.getByRole('navigation', { name: '画面' }).querySelector('a[href="/harnesses"]') as HTMLElement)
    await userEvent.click(await screen.findByRole('link', { name: harness.name }))
    expect(window.location.pathname).toBe('/harnesses/new')
    expect(await screen.findByRole('heading', { name: harness.name })).toBeVisible()
    expect(screen.getByText('Profile の版')).toBeVisible()
    expect(screen.queryByRole('button', { name: '登録' })).toBeNull()
  })
})

describe('Harness の編集', () => {
  it('読み込んだ値を入力欄に出し、PATCH には id を送らない', async () => {
    let current = HARNESS
    const fake = renderAt('/harnesses/claude-dev/edit', (request) => {
      if (request.method === 'PATCH') {
        current = { ...HARNESS, name: 'Claude Developer 2' }
        return json(200, current)
      }
      if (request.url.startsWith('/api/harnesses')) return page([current])
      if (request.url.startsWith('/api/harness-profiles')) return page([])
      return undefined
    })
    const name = await screen.findByLabelText('名称')
    expect(name).toHaveValue('Claude Developer')
    expect(screen.getByLabelText('Harness ID')).toBeDisabled()
    expect(screen.getByLabelText('Adapter')).toHaveValue('claude_code')
    expect(screen.getByLabelText('対応モデル')).toHaveValue('model-a')
    await userEvent.clear(name)
    await userEvent.type(name, 'Claude Developer 2')
    await userEvent.click(screen.getByRole('button', { name: '保存' }))
    expect(await screen.findByRole('heading', { name: 'Claude Developer 2' })).toBeVisible()
    const patch = fake.calls.find((c) => c.method === 'PATCH')
    expect(patch?.url).toBe('/api/harnesses/claude-dev')
    const body = JSON.parse(patch?.body ?? '')
    expect(body).not.toHaveProperty('id')
    expect(body).toMatchObject({ name: 'Claude Developer 2', adapter: 'claude_code', supportedModels: ['model-a'] })
  })

  it('一覧に無い ID は見つからないことを表示する', async () => {
    renderAt('/harnesses/missing/edit', (request) => (request.url.startsWith('/api/harnesses') ? page([HARNESS]) : undefined))
    expect(await screen.findByRole('alert')).toHaveTextContent('対象が見つかりません')
  })
})

describe('Harness 詳細と Profile の版の登録', () => {
  function renderDetail(post: (request: Recorded) => Response) {
    let profiles: HarnessProfile[] = [profile('1.0.0', SHA)]
    const fake = renderAt('/harnesses/claude-dev', (request) => {
      if (request.method === 'POST') {
        const response = post(request)
        if (response.status === 201) profiles = [...profiles, profile('1.1.0', OTHER_SHA)]
        return response
      }
      if (request.url.startsWith('/api/harnesses')) return page([HARNESS])
      if (request.url.startsWith('/api/harness-profiles')) return page(profiles)
      if (request.url.startsWith('/api/projects')) return page([PROJECT])
      return undefined
    })
    return fake
  }

  it('版の一覧と使用中プロジェクトを出し、登録すると commit SHA と暫定運用を表示する', async () => {
    const fake = renderDetail(() => json(201, profile('1.1.0', OTHER_SHA)))
    const table = await screen.findByRole('table', { name: '登録済みの Profile の版' })
    expect(within(table).getAllByRole('row')[1]).toHaveTextContent(`1.0.0${SHA}model-a暫定運用`)
    const usages = screen.getByRole('table', { name: 'この Harness を使っているプロジェクト' })
    expect(usages).toHaveTextContent('Alpha')
    expect(usages).toHaveTextContent('0123456')

    await userEvent.type(screen.getByLabelText('版'), '1.1.0')
    await userEvent.click(screen.getByRole('button', { name: '版を登録' }))
    expect(await screen.findByRole('status')).toHaveTextContent(`版 1.1.0 を登録しました。commit SHA: ${OTHER_SHA}、状態: 暫定運用。`)
    expect(await within(table).findByText(OTHER_SHA)).toBeVisible()
    expect(screen.getByLabelText('版')).toHaveValue('')
    const post = fake.calls.find((c) => c.method === 'POST')
    expect(post?.url).toBe('/api/harness-profiles')
    expect(post?.headers['X-CSRF-Token']).toBe('t')
    expect(JSON.parse(post?.body ?? '')).toEqual({ harnessId: 'claude-dev', version: '1.1.0' })
  })

  it('同じ版・同じ SHA の再登録（200）は変更なしと表示する', async () => {
    renderDetail(() => json(200, profile('1.0.0', SHA)))
    await userEvent.type(await screen.findByLabelText('版'), '1.0.0')
    await userEvent.click(screen.getByRole('button', { name: '版を登録' }))
    expect(await screen.findByRole('status')).toHaveTextContent(`版 1.0.0 は同じ commit SHA（${SHA}）で登録済みです。変更はありません`)
  })

  it('同じ版の別 SHA（409）は、登録済みの SHA とサーバーの応答を表示し、一覧を読み直す', async () => {
    const fake = renderDetail(() => apiError(409, 'conflict', 'Profile version already registered with another commit SHA'))
    await userEvent.type(await screen.findByLabelText('版'), '1.0.0')
    await userEvent.click(screen.getByRole('button', { name: '版を登録' }))
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('状態が変わりました')
    expect(alert).toHaveTextContent('版 1.0.0 は別の commit SHA ですでに登録されています')
    expect(alert).toHaveTextContent(`登録済みの commit SHA: ${SHA}`)
    expect(alert).toHaveTextContent('Profile version already registered with another commit SHA')
    expect(fake.calls.filter((c) => c.url.startsWith('/api/harness-profiles?')).length).toBe(2)
  })

  it('SemVer でない版は送らず、422 の理由（タグが無い等）は一覧に出す', async () => {
    const fake = renderDetail(() =>
      apiError(422, 'precondition_failed', 'Profile tag not found', [{ field: 'tagName', reason: 'not_found' }]),
    )
    const version = await screen.findByLabelText('版')
    await userEvent.type(version, 'v1.2')
    await userEvent.click(screen.getByRole('button', { name: '版を登録' }))
    expect(within(fieldOf('版')).getByText('SemVer（例 1.2.0）で入力してください')).toBeVisible()
    expect(fake.calls.some((c) => c.method === 'POST')).toBe(false)

    await userEvent.clear(version)
    await userEvent.type(version, '9.9.9')
    await userEvent.click(screen.getByRole('button', { name: '版を登録' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('タグ: 見つかりません')
  })
})
