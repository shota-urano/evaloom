import { spawn } from 'node:child_process'
import { realpathSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

import { backend, frontend, loginOwner, run, setupOwner, startEvaloomd, testOwner, type Evaloomd } from './evaloomd.mts'
import { createRepo, git, tagProfile, type Repo } from './fixtures.mts'

const controller = new AbortController()
const { signal } = controller
const interrupt = () => controller.abort()
process.on('SIGINT', interrupt)
process.on('SIGTERM', interrupt)

/** ビルド中も Ctrl-C を処理し、npm が起動する子プロセスまで終了を待つ。 */
async function build(command: string, args: string[], cwd: string): Promise<void> {
  signal.throwIfAborted()
  const child = spawn(command, args, { cwd, stdio: 'inherit', detached: true })
  const kill = (name: NodeJS.Signals) => {
    if (!child.pid) return
    try { process.kill(-child.pid, name) } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== 'ESRCH') throw error
    }
  }
  let timer: ReturnType<typeof setTimeout> | undefined
  const abort = () => {
    kill('SIGTERM')
    timer = setTimeout(() => kill('SIGKILL'), 7_000)
  }
  signal.addEventListener('abort', abort, { once: true })
  try {
    await new Promise<void>((resolve, reject) => {
      child.once('error', reject)
      child.once('close', (code) => code === 0 ? resolve() : reject(new Error(`${command} のビルドに失敗した（code ${code}）`)))
    })
    signal.throwIfAborted()
  } finally {
    clearTimeout(timer)
    signal.removeEventListener('abort', abort)
  }
}

/**
 * evaloomd は具体的な Adapter をまだ登録していないため、Harness 本体だけ直接登録する。
 * Adapter 実装後は、この関数を POST /api/harnesses に置き換える。Profile の版は実 API を使う。
 */
function seedHarness(db: string, repo: Repo) {
  const sql = (value: string) => `'${value.replaceAll("'", "''")}'`
  const now = sql(new Date().toISOString())
  run('sqlite3', [db, `INSERT INTO harnesses
    (id, name, repo_path, repo_url, created_at, updated_at)
    VALUES ('claude-developer', 'Demo Harness', ${sql(repo.path)}, ${sql(repo.url)}, ${now}, ${now});`])
}

async function seedAPI(server: Evaloomd) {
  const request = (url: string, init: RequestInit = {}) => fetch(url, {
    ...init, signal: AbortSignal.any([signal, AbortSignal.timeout(15_000)]),
  })
  const login = await loginOwner(server.origin, signal)
  const cookie = login.headers.getSetCookie().find((value) => value.startsWith('evaloom_session='))?.split(';')[0]
  if (!login.ok || !cookie) throw new Error(`確認用データのログインに失敗した: ${login.status}`)
  const { csrfToken } = await login.json() as { csrfToken: string }
  const post = async (path: string, body: unknown) => {
    const response = await request(server.origin + path, {
      method: 'POST',
      headers: { Cookie: cookie, Origin: server.origin, 'X-CSRF-Token': csrfToken, 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    })
    if (!response.ok) throw new Error(`${path}: ${response.status} ${await response.text()}`)
    return await response.json() as { id: string }
  }
  let profileId = ''
  for (const version of ['1.0.0', '1.1.0']) {
    const profile = await post('/api/harness-profiles', { harnessId: 'claude-developer', version })
    if (version === '1.0.0') profileId = profile.id
  }
  await post('/api/projects', { id: 'demo-app', name: 'Demo Project', runnerId: 'local', harnessId: 'claude-developer', profileId })
}

async function main() {
  const port = process.env.DEMO_PORT ?? '0'
  if (!/^\d+$/.test(port) || Number(port) > 65535) throw new Error('DEMO_PORT は 0〜65535 の整数にしてください')
  // DB・リポジトリ・バイナリをすべて所有する新しいディレクトリだけを後始末する。
  const temp = realpathSync(run('mktemp', ['-d', join(tmpdir(), 'evaloom-demo-XXXXXX')]).trim())
  console.log(`demo: 一時ディレクトリ ${temp}`)
  let server: Evaloomd | undefined
  try {
    await build('npm', ['run', 'build'], frontend)
    const binary = join(temp, 'evaloomd')
    await build('go', ['build', '-o', binary, './cmd/evaloomd'], backend)
    signal.throwIfAborted()
    const db = join(temp, 'evaloom.db')
    const harness = createRepo(temp, 'harness')
    for (const version of ['1.0.0', '1.1.0']) tagProfile(harness.path, version, `demo-${version}`)
    git(harness.path, 'push', '--quiet', 'origin', 'main', '--tags')
    run(binary, ['-db', db, 'runner', 'register', '-worktree-root', join(temp, 'worktrees'), '-name', 'Demo Runner', 'local'])
    seedHarness(db, harness)
    setupOwner(binary, db)
    server = await startEvaloomd(binary, db, port, signal)
    console.log(`demo: データ投入用 evaloomd PID ${server.pid}`)
    await seedAPI(server)
    console.log(`demo: evaloomd PID ${server.pid}`)
    console.log('Runner は local。終了は Ctrl-C。')
    console.log(`この URL を開く: ${server.origin}/`)
    console.log(`テスト用 ID: ${testOwner.id}`)
    console.log(`テスト用パスワード: ${testOwner.password}`)
    await server.exited
  } finally {
    await server?.stop()
    rmSync(temp, { recursive: true, force: true })
    console.log(`demo: 一時ディレクトリを削除しました ${temp}`)
  }
}

try {
  await main()
} catch (error) {
  if (!signal.aborted) {
    console.error(error instanceof Error ? error.message : error)
    process.exitCode = 1
  }
} finally {
  process.removeListener('SIGINT', interrupt)
  process.removeListener('SIGTERM', interrupt)
}
