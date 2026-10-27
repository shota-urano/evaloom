import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it } from 'vitest'

import { ApiClient } from '@/api/client'
import { registry, type Project } from '@/api/registry'
import { matchPath } from '@/lib/router'
import { apiError, fakeFetch, json, type Recorded } from '@/test/fake-fetch'
import App from '@/App'

const RUNNERS = { items: [{ id: 'local', name: 'local', status: 'online', lastHeartbeatAt: null, createdAt: '2026-10-27T00:00:00Z' }], nextCursor: '' }

const PROJECT: Project = {
  id: 'alpha',
  name: 'Alpha',
  localRepoPath: '/repos/alpha',
  gitRemoteUrl: 'git@example.com:alpha.git',
  runnerId: 'local',
  baseBranch: 'main',
  defaultRole: 'developer',
  reviewRole: null,
  contextLocation: 'AGENTS.md',
  testCommand: 'make test',
  testReportFormat: null,
  testReportPath: null,
  setupCommand: null,
  copyAllowlist: ['config/test.json'],
  testFilePatterns: [],
  allowedOperations: {},
  approvalPolicy: {},
  timeouts: { testSec: 600 },
  createdAt: '2026-10-27T00:00:00Z',
  updatedAt: '2026-10-27T00:00:00Z',
  harnesses: [],
}

function renderAt(path: string, handler: (request: Recorded) => Response) {
  window.history.pushState(null, '', path)
  const fake = fakeFetch((request) => (request.url === '/api/session' ? json(200, { csrfToken: 't' }) : handler(request)))
  render(<App api={new ApiClient(fake.fetch)} />)
  return fake
}

function fieldOf(label: string) {
  return screen.getByLabelText(label).closest('[data-field]') as HTMLElement
}

afterEach(() => window.history.pushState(null, '', '/'))

describe('matchPath', () => {
  it.each([
    ['/projects/:id', '/projects/alpha', { id: 'alpha' }],
    ['/projects/:id/edit', '/projects/alpha/edit', { id: 'alpha' }],
    ['/projects/:id', '/projects/alpha/edit', null],
    ['/projects/:id', '/harnesses/alpha', null],
  ])('%s と %s', (pattern, path, want) => {
    expect(matchPath(pattern, path)).toEqual(want)
  })
})

describe('一覧 API', () => {
  it('nextCursor が空になるまで続きを読む', async () => {
    const fake = fakeFetch((request) =>
      request.url.includes('cursor=')
        ? json(200, { items: [{ id: 'b' }], nextCursor: '' })
        : json(200, { items: [{ id: 'a' }], nextCursor: 'next+1' }),
    )
    const items = await registry.listProjects(new ApiClient(fake.fetch))
    expect(items.map((p) => p.id)).toEqual(['a', 'b'])
    expect(fake.calls.map((c) => c.url)).toEqual(['/api/projects?limit=200', '/api/projects?limit=200&cursor=next%2B1'])
  })
})

describe('プロジェクトの登録', () => {
  it('必須項目が空なら送信せず、項目ごとにエラーを出す', async () => {
    const fake = renderAt('/projects/new', (request) => (request.url.startsWith('/api/runners') ? json(200, RUNNERS) : json(500, {})))
    await userEvent.click(await screen.findByRole('button', { name: '登録' }))
    for (const label of ['Project ID', '名称', 'ローカルリポジトリのパス', 'Git リポジトリ URL', 'Runner']) {
      expect(within(fieldOf(label)).getByText('入力してください')).toBeVisible()
    }
    expect(fake.calls.some((c) => c.method === 'POST')).toBe(false)
  })

  it('送信内容は API の項目名で、空欄の任意項目は null、許可リストは行ごとの配列になる', async () => {
    const fake = renderAt('/projects/new', (request) => {
      if (request.url.startsWith('/api/runners')) return json(200, RUNNERS)
      if (request.method === 'POST') return json(201, { ...PROJECT, warnings: [] })
      return json(200, PROJECT)
    })
    await userEvent.type(await screen.findByLabelText('Project ID'), 'alpha')
    await userEvent.type(screen.getByLabelText('名称'), 'Alpha')
    await userEvent.type(screen.getByLabelText('ローカルリポジトリのパス'), '/repos/alpha')
    await userEvent.type(screen.getByLabelText('Git リポジトリ URL'), 'git@example.com:alpha.git')
    await userEvent.selectOptions(screen.getByLabelText('Runner'), 'local')
    await userEvent.type(screen.getByLabelText('コピー許可リスト'), 'config/test.json{enter}{enter}.env.test')
    await userEvent.type(screen.getByLabelText('テストのタイムアウト（秒）'), '600')
    await userEvent.click(screen.getByRole('button', { name: '登録' }))
    expect(await screen.findByRole('heading', { name: 'Alpha' })).toBeVisible()
    const post = fake.calls.find((c) => c.method === 'POST')
    expect(JSON.parse(post?.body ?? '')).toEqual({
      id: 'alpha',
      name: 'Alpha',
      localRepoPath: '/repos/alpha',
      gitRemoteUrl: 'git@example.com:alpha.git',
      runnerId: 'local',
      baseBranch: 'main',
      defaultRole: 'developer',
      testCommand: null,
      testReportFormat: null,
      testReportPath: null,
      setupCommand: null,
      copyAllowlist: ['config/test.json', '.env.test'],
      testFilePatterns: [],
      allowedOperations: {},
      approvalPolicy: {},
      timeouts: { testSec: 600 },
    })
  })

  it('保存の応答に warnings があれば詳細画面に表示する', async () => {
    renderAt('/projects/new', (request) => {
      if (request.url.startsWith('/api/runners')) return json(200, RUNNERS)
      if (request.method === 'POST') return json(201, { ...PROJECT, warnings: ['.env.test may contain secrets; use test-only dummy values'] })
      return json(200, PROJECT)
    })
    await userEvent.type(await screen.findByLabelText('Project ID'), 'alpha')
    await userEvent.type(screen.getByLabelText('名称'), 'Alpha')
    await userEvent.type(screen.getByLabelText('ローカルリポジトリのパス'), '/repos/alpha')
    await userEvent.type(screen.getByLabelText('Git リポジトリ URL'), 'git@example.com:alpha.git')
    await userEvent.selectOptions(screen.getByLabelText('Runner'), 'local')
    await userEvent.click(screen.getByRole('button', { name: '登録' }))
    const alert = await screen.findByText('コピー許可リストに秘密情報を含むおそれのあるファイルがあります')
    expect(alert.parentElement).toHaveTextContent('.env.test may contain secrets')
  })

  it('400 の details は同じ名前の項目に、422 のパス検証の理由は対応する項目と一覧に出す', async () => {
    let status = 400
    renderAt('/projects/new', (request) => {
      if (request.url.startsWith('/api/runners')) return json(200, RUNNERS)
      return status === 400
        ? apiError(400, 'validation_failed', 'invalid', [{ field: 'timeouts.testSec', reason: 'out_of_range' }])
        : apiError(422, 'precondition_failed', 'repository validation failed: baseBranch (not_found)', [
            { field: 'baseBranch', reason: 'not_found' },
          ])
    })
    await userEvent.type(await screen.findByLabelText('Project ID'), 'alpha')
    await userEvent.type(screen.getByLabelText('名称'), 'Alpha')
    await userEvent.type(screen.getByLabelText('ローカルリポジトリのパス'), '/repos/alpha')
    await userEvent.type(screen.getByLabelText('Git リポジトリ URL'), 'git@example.com:alpha.git')
    await userEvent.selectOptions(screen.getByLabelText('Runner'), 'local')
    await userEvent.click(screen.getByRole('button', { name: '登録' }))
    expect(await within(fieldOf('テストのタイムアウト（秒）')).findByText('範囲外の値です')).toBeVisible()

    status = 422
    await userEvent.click(screen.getByRole('button', { name: '登録' }))
    expect(await within(fieldOf('Base Branch')).findByText('見つかりません')).toBeVisible()
    expect(screen.getByRole('alert')).toHaveTextContent('Base Branch: 見つかりません')
    expect(within(fieldOf('テストのタイムアウト（秒）')).queryByText('範囲外の値です')).toBeNull()
  })
})

describe('プロジェクトの編集', () => {
  it('PATCH には id・reviewRole・contextLocation を送らず、読み込んだ値を入力欄に出す', async () => {
    let current = PROJECT
    const fake = renderAt('/projects/alpha/edit', (request) => {
      if (request.url.startsWith('/api/runners')) return json(200, RUNNERS)
      if (request.url.startsWith('/api/harnesses')) return json(200, { items: [], nextCursor: '' })
      if (request.method === 'PATCH') {
        current = { ...PROJECT, name: 'Alpha 2' }
        return json(200, { ...current, warnings: [] })
      }
      return json(200, current)
    })
    const name = await screen.findByLabelText('名称')
    expect(name).toHaveValue('Alpha')
    expect(screen.getByLabelText('Project ID')).toBeDisabled()
    expect(screen.getByLabelText('コピー許可リスト')).toHaveValue('config/test.json')
    expect(screen.getByLabelText('テストのタイムアウト（秒）')).toHaveValue('600')
    await userEvent.clear(name)
    await userEvent.type(name, 'Alpha 2')
    await userEvent.click(screen.getByRole('button', { name: '保存' }))
    await screen.findByRole('heading', { name: 'Alpha 2' })
    const patch = fake.calls.find((c) => c.method === 'PATCH')
    expect(patch?.url).toBe('/api/projects/alpha')
    const body = JSON.parse(patch?.body ?? '')
    expect(body).not.toHaveProperty('id')
    expect(body).not.toHaveProperty('reviewRole')
    expect(body).not.toHaveProperty('contextLocation')
    expect(body).toMatchObject({ name: 'Alpha 2', testCommand: 'make test', timeouts: { testSec: 600 } })
  })

  it('409 は最新を読み直すボタンを出し、押すと読み直した値で入力欄を作り直す', async () => {
    let reads = 0
    renderAt('/projects/alpha/edit', (request) => {
      if (request.url.startsWith('/api/runners')) return json(200, RUNNERS)
      if (request.method === 'PATCH') return apiError(409, 'conflict', 'Project changed during repository validation; retry the request')
      reads += 1
      return json(200, reads === 1 ? PROJECT : { ...PROJECT, name: 'Alpha（別の操作で変更）', updatedAt: '2026-10-27T01:00:00Z' })
    })
    await userEvent.type(await screen.findByLabelText('名称'), ' edited')
    await userEvent.click(screen.getByRole('button', { name: '保存' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('状態が変わりました')
    await userEvent.click(screen.getByRole('button', { name: '最新を読み直す' }))
    expect(await screen.findByDisplayValue('Alpha（別の操作で変更）')).toBeVisible()
    expect(screen.queryByRole('alert')).toBeNull()
  })
})

describe('Harness Profile の割り当て', () => {
  it('Harness を選ぶとその版だけを読み、役割・Harness・版を送る。422 の理由は版の項目に出す', async () => {
    const fake = renderAt('/projects/alpha', (request) => {
      if (request.url.startsWith('/api/harnesses')) return json(200, { items: [{ id: 'h1', name: 'H1', adapter: 'codex', specialty: '' }], nextCursor: '' })
      if (request.url.startsWith('/api/harness-profiles')) {
        return json(200, {
          items: [{ id: 'p1', harnessId: 'h1', version: '1.2.0', commitSha: 'abcdef0123456789', modelId: 'm', operationStatus: 'provisional', registeredAt: '' }],
          nextCursor: '',
        })
      }
      if (request.method === 'POST') {
        return apiError(422, 'precondition_failed', 'Profile version is not registered for this Harness', [{ field: 'version', reason: 'not_found' }])
      }
      return json(200, PROJECT)
    })
    await screen.findByRole('option', { name: 'H1（h1）' })
    await userEvent.selectOptions(screen.getByLabelText('Harness'), 'h1')
    const version = screen.getByLabelText('版')
    await screen.findByRole('option', { name: '1.2.0（abcdef0・暫定運用）' })
    await userEvent.selectOptions(version, '1.2.0')
    await userEvent.click(screen.getByRole('button', { name: '割り当てる' }))
    expect(await within(fieldOf('版')).findByText('見つかりません')).toBeVisible()
    expect(screen.getByRole('alert')).toHaveTextContent('版: 見つかりません')
    expect(fake.calls.some((c) => c.url === '/api/harness-profiles?harnessId=h1&limit=200')).toBe(true)
    const post = fake.calls.find((c) => c.method === 'POST')
    expect(post?.url).toBe('/api/projects/alpha/harnesses')
    expect(JSON.parse(post?.body ?? '')).toEqual({ role: 'developer', harnessId: 'h1', version: '1.2.0' })
  })
})
