import assert from 'node:assert/strict'
import { spawn } from 'node:child_process'
import { createHash } from 'node:crypto'
import { existsSync, readFileSync } from 'node:fs'
import { createServer } from 'node:net'
import { join } from 'node:path'
import { test } from 'node:test'

import { root, run } from './evaloomd.mts'

function fingerprint() {
  const hash = createHash('sha256')
  for (const path of run('git', ['ls-files', '-z'], { cwd: root }).split('\0').filter(Boolean)) {
    hash.update(path)
    try {
      hash.update('present:').update(readFileSync(join(root, path)))
    } catch (error) {
      // A worker may delete a tracked file before the orchestrator commits it.
      // Include its absence so the demo must preserve that state as well.
      if (error.code !== 'ENOENT') throw error
      hash.update('absent:')
    }
  }
  for (const name of ['evaloom.db', 'evaloom.db-wal', 'evaloom.db-shm']) {
    const path = join(root, 'data', name)
    hash.update(existsSync(path) ? readFileSync(path) : `absent:${name}`)
  }
  return hash.digest('hex')
}

function launch(port) {
  const env = { ...process.env }
  delete env.DEMO_PORT
  if (port !== undefined) env.DEMO_PORT = String(port)
  const child = spawn('make', ['demo'], { cwd: root, env, detached: true, stdio: ['ignore', 'pipe', 'pipe'] })
  let output = ''
  child.stdout.on('data', (chunk) => { output += chunk })
  child.stderr.on('data', (chunk) => { output += chunk })
  const closed = new Promise((resolve, reject) => {
    child.once('error', reject)
    child.once('close', (code, signal) => resolve({ code, signal }))
  })
  async function until(pattern, timeout = 90_000) {
    const deadline = Date.now() + timeout
    while (Date.now() < deadline) {
      const match = pattern.exec(output)
      if (match) return match[1]
      if (child.exitCode !== null || child.signalCode !== null) throw new Error(`demo が終了した: ${output}`)
      await new Promise((resolve) => setTimeout(resolve, 30))
    }
    throw new Error(`demo の待機時間を超えた: ${output}`)
  }
  async function stop(signal = 'SIGINT') {
    if (child.exitCode === null && child.signalCode === null) process.kill(-child.pid, signal)
    const timer = setTimeout(() => {
      try { process.kill(-child.pid, 'SIGKILL') } catch { /* 終了済み */ }
    }, 12_000)
    try { return await closed } finally { clearTimeout(timer) }
  }
  return { child, closed, until, stop, output: () => output }
}

function assertClean(temp, output) {
  assert.equal(existsSync(temp), false, `一時ディレクトリが残った: ${temp}`)
  for (const [, pid] of output.matchAll(/evaloomd PID (\d+)/g)) {
    assert.throws(() => process.kill(Number(pid), 0), { code: 'ESRCH' }, `evaloomd が残った: ${pid}`)
  }
}

async function listen() {
  const server = createServer()
  await new Promise((resolve, reject) => {
    server.once('error', reject)
    server.listen(0, '127.0.0.1', resolve)
  })
  return { server, port: server.address().port, close: () => new Promise((resolve) => server.close(resolve)) }
}

test('make demo: 新しいデータ、未使用ログイン URL、API、指定ポート、SIGINT/SIGTERM の後始末', { timeout: 240_000 }, async () => {
  const before = fingerprint()
  let previousTemp
  for (const explicitPort of [false, true]) {
    const reserved = explicitPort ? await listen() : undefined
    const port = reserved?.port
    await reserved?.close()
    const demo = launch(port)
    let temp
    let origin
    try {
      temp = await demo.until(/demo: 一時ディレクトリ (.+)/)
      assert.notEqual(temp, previousTemp)
      previousTemp = temp
      const loginURL = await demo.until(/この URL を開く: (http:\/\/\S+)/)
      assert.ok(await demo.until(/demo: evaloomd PID (\d+)/))
      origin = new URL(loginURL).origin
      if (port) assert.equal(new URL(loginURL).port, String(port))
      const login = await fetch(loginURL, { headers: { Accept: 'application/json' }, signal: AbortSignal.timeout(5000) })
      assert.equal(login.status, 200, await login.text())
      const cookie = login.headers.getSetCookie()[0]?.split(';')[0]
      assert.ok(cookie?.startsWith('evaloom_session='))
      const api = async (path) => {
        const response = await fetch(origin + path, { headers: { Cookie: cookie }, signal: AbortSignal.timeout(5000) })
        assert.equal(response.status, 200)
        return response.json()
      }
      const projects = (await api('/api/projects')).items
      const harnesses = (await api('/api/harnesses')).items
      const profiles = (await api('/api/harness-profiles')).items
      assert.equal(projects.length, 1)
      assert.equal(harnesses.length, 1)
      assert.deepEqual(profiles.map((p) => p.version).sort(), ['1.0.0', '1.1.0'])
      assert.equal((await api('/api/runners')).items.length, 1)
      const pin = projects[0].harnesses[0]
      assert.equal(pin.role, 'developer')
      assert.equal(pin.harnessId, harnesses[0].id)
      assert.equal(pin.profileId, profiles.find((p) => p.version === '1.0.0').id)
      for (const profile of profiles) {
        assert.equal(profile.operationStatus, 'provisional')
        assert.equal(profile.commitSha, run('git', ['rev-parse', `refs/tags/profile/v${profile.version}^{commit}`], { cwd: harnesses[0].repoPath }).trim())
      }
      for (const name of ['alpha', 'beta']) {
        const path = join(temp, 'repos', name)
        assert.ok(demo.output().includes(path))
        assert.equal(run('git', ['remote', 'get-url', 'origin'], { cwd: path }).trim(), join(temp, 'remotes', `${name}.git`))
        assert.ok(run('git', ['ls-remote', 'origin', 'refs/heads/main'], { cwd: path }).includes('refs/heads/main'))
      }
      // 表示された未登録リポジトリも実 API のパス検証を通る。
      const session = await api('/api/session')
      const registered = await fetch(origin + '/api/projects', {
        method: 'POST', headers: { Cookie: cookie, Origin: origin, 'X-CSRF-Token': session.csrfToken, 'Content-Type': 'application/json' },
        body: JSON.stringify({ id: 'second', name: 'Second', localRepoPath: join(temp, 'repos', 'beta'), gitRemoteUrl: join(temp, 'remotes', 'beta.git'), runnerId: 'local', baseBranch: 'main', defaultRole: 'developer' }),
        signal: AbortSignal.timeout(15_000),
      })
      assert.equal(registered.status, 201, await registered.text())
      const page = await fetch(origin, { signal: AbortSignal.timeout(5000) })
      assert.match(await page.text(), /<div id="root"><\/div>/)
    } finally {
      await demo.stop(explicitPort ? 'SIGTERM' : 'SIGINT')
    }
    assertClean(temp, demo.output())
    await assert.rejects(fetch(origin, { signal: AbortSignal.timeout(2000) }))
    assert.equal(fingerprint(), before, '追跡ファイルまたは普段の DB が変更された')
  }
})

test('make demo: ポート使用中の失敗とビルド前の中断でも後始末する', { timeout: 120_000 }, async () => {
  const before = fingerprint()
  const occupied = await listen()
  const demo = launch(occupied.port)
  let temp
  try {
    temp = await demo.until(/demo: 一時ディレクトリ (.+)/)
    const result = await demo.closed
    assert.notEqual(result.code, 0, demo.output())
    assert.match(demo.output(), /address already in use/)
    assert.doesNotMatch(demo.output(), /この URL を開く:/)
  } finally {
    await demo.stop()
    await occupied.close()
  }
  assertClean(temp, demo.output())
  const interrupted = launch()
  try {
    temp = await interrupted.until(/demo: 一時ディレクトリ (.+)/)
  } finally {
    await interrupted.stop()
  }
  assertClean(temp, interrupted.output())
  assert.equal(fingerprint(), before)
})
