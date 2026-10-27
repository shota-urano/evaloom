// @vitest-environment node
import { mkdtempSync, readFileSync, rmSync } from 'node:fs'
import { createServer as createHttpServer, type IncomingMessage, type Server } from 'node:http'
import { connect, type AddressInfo } from 'node:net'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { createServer, type ViteDevServer } from 'vite'
import { afterAll, beforeAll, beforeEach, describe, expect, it } from 'vitest'

import { checkRequest, evaloomdDevProxy, headerValues, REJECTION_LOG_FILE } from './dev-proxy.ts'

const ALLOWED = ['localhost:5173', '127.0.0.1:5173']

function raw(headers: [string, string][]): string[] {
  return headers.flat()
}

describe('checkRequest（開発サーバが転送前に Host / Origin を照合する）', () => {
  it('許可 Host と、それに一致する Origin の要求は通す', () => {
    for (const host of ALLOWED) {
      for (const method of ['GET', 'POST', 'DELETE']) {
        expect(checkRequest(method, raw([['Host', host], ['Origin', `http://${host}`]]), ALLOWED)).toBeNull()
      }
    }
  })

  it('読み取りに限り Origin 無しを通す', () => {
    for (const method of ['GET', 'HEAD', 'OPTIONS']) {
      expect(checkRequest(method, raw([['Host', 'localhost:5173']]), ALLOWED)).toBeNull()
    }
    for (const method of ['POST', 'PUT', 'PATCH', 'DELETE', 'PROPFIND']) {
      expect(checkRequest(method, raw([['Host', 'localhost:5173']]), ALLOWED)).toBe('origin_mismatch')
    }
  })

  it.each([
    ['evaloomd の許可値の Host', [['Host', '127.0.0.1:8080'], ['Origin', 'http://127.0.0.1:8080']]],
    ['許可外の Host（DNS リバインディング）', [['Host', 'evil.example:5173'], ['Origin', 'http://evil.example:5173']]],
    ['別ポート', [['Host', 'localhost:5174'], ['Origin', 'http://localhost:5174']]],
    ['重複した Host', [['Host', 'localhost:5173'], ['Host', 'localhost:5173'], ['Origin', 'http://localhost:5173']]],
    ['Host 無し', [['Origin', 'http://localhost:5173']]],
  ] as [string, [string, string][]][])('%s は host_mismatch', (_, headers) => {
    expect(checkRequest('GET', raw(headers), ALLOWED)).toBe('host_mismatch')
  })

  it.each([
    ['evaloomd の許可値の Origin', 'localhost:5173', ['http://127.0.0.1:8080']],
    ['evaloomd の許可値の Origin（127.0.0.1 側）', '127.0.0.1:5173', ['http://127.0.0.1:8080']],
    ['外部 Origin', 'localhost:5173', ['http://evil.example']],
    ['null', 'localhost:5173', ['null']],
    ['もう一方の許可 Host の Origin', 'localhost:5173', ['http://127.0.0.1:5173']],
    ['別ポート', 'localhost:5173', ['http://localhost:5174']],
    ['別 scheme', 'localhost:5173', ['https://localhost:5173']],
    ['重複した Origin', 'localhost:5173', ['http://localhost:5173', 'http://localhost:5173']],
  ] as [string, string, string[]][])('%s は origin_mismatch', (_, host, origins) => {
    const headers: [string, string][] = [['Host', host], ...origins.map((o): [string, string] => ['Origin', o])]
    expect(checkRequest('GET', raw(headers), ALLOWED)).toBe('origin_mismatch')
  })
})

// ---- 実際の HTTP サーバでの回帰テスト ----
// 開発サーバ = vite（照合の plugin + server.proxy）、evaloomd 役 = 偽 upstream。
// 偽 upstream は evaloomd の境界検証（backend/internal/api/auth.go の boundary）と同じ照合をし、
// 通れば CSRF トークンを返し、/auth/login では Cookie を発行してトークンを消費する。

const COOKIE = 'evaloom_session=SESSION-COOKIE-VALUE'
const CSRF = 'CSRF-TOKEN-VALUE'
const LOGIN_TOKEN = 'LOGIN-TOKEN-VALUE'
const UPSTREAM_CSRF = 'upstream-csrf-token'

type Reply = { status: number; headers: string[]; body: string }

/** ヘッダを生のまま（重複も含めて）送るため、ソケットへ直接書く。 */
function send(port: number, method: string, path: string, headers: [string, string][]): Promise<Reply> {
  return new Promise((resolve, reject) => {
    const socket = connect(port, '127.0.0.1')
    const chunks: Buffer[] = []
    socket.on('connect', () => {
      const lines = [`${method} ${path} HTTP/1.1`, ...headers.map(([k, v]) => `${k}: ${v}`), 'Connection: close', 'Content-Length: 0']
      socket.write(`${lines.join('\r\n')}\r\n\r\n`)
    })
    socket.on('data', (chunk) => chunks.push(chunk))
    socket.on('error', reject)
    socket.on('end', () => {
      const text = Buffer.concat(chunks).toString('utf8')
      const [head, ...rest] = text.split('\r\n\r\n')
      const [statusLine, ...headerLines] = head.split('\r\n')
      resolve({ status: Number(statusLine.split(' ')[1]), headers: headerLines, body: rest.join('\r\n\r\n') })
    })
  })
}

describe('開発サーバの中継（回帰テスト: 照合に通らない要求は evaloomd へ転送せずに 403）', () => {
  let upstream: Server
  let vite: ViteDevServer
  let devPort: number
  let upstreamHost: string
  let tempRoot: string
  let logDir: string
  let loginUsed = false
  const reached: { method: string; url: string; host: string[]; origin: string[] }[] = []

  beforeAll(async () => {
    upstream = createHttpServer((req: IncomingMessage, res) => {
      const host = headerValues(req.rawHeaders, 'host')
      const origin = headerValues(req.rawHeaders, 'origin')
      reached.push({ method: req.method ?? '', url: req.url ?? '', host, origin })
      const mutating = req.method !== 'GET' && req.method !== 'HEAD' && req.method !== 'OPTIONS'
      res.setHeader('Content-Type', 'application/json')
      if (
        host.length !== 1 ||
        host[0] !== upstreamHost ||
        origin.length > 1 ||
        (origin.length === 1 && origin[0] !== `http://${upstreamHost}`) ||
        (origin.length === 0 && mutating)
      ) {
        res.statusCode = 403
        res.end('{"error":{"code":"forbidden","message":"upstream boundary","details":[]}}')
        return
      }
      if (req.url?.startsWith('/auth/login')) {
        loginUsed = true
        res.setHeader('Set-Cookie', `${COOKIE}; Path=/; HttpOnly; SameSite=Strict`)
      }
      res.end(JSON.stringify({ csrfToken: UPSTREAM_CSRF }))
    })
    await new Promise<void>((resolve) => upstream.listen(0, '127.0.0.1', resolve))
    upstreamHost = `127.0.0.1:${(upstream.address() as AddressInfo).port}`
    const probe = createHttpServer()
    await new Promise<void>((resolve) => probe.listen(0, '127.0.0.1', resolve))
    devPort = (probe.address() as AddressInfo).port
    await new Promise<void>((resolve) => probe.close(() => resolve()))
    tempRoot = mkdtempSync(join(tmpdir(), 'evaloom-dev-proxy-'))
    logDir = join(tempRoot, 'data', 'security')
    const devProxy = evaloomdDevProxy({ target: `http://${upstreamHost}`, devPort, prefixes: ['/api', '/auth'], logDir })
    vite = await createServer({
      configFile: false,
      logLevel: 'silent',
      plugins: [devProxy.plugin],
      server: { host: '127.0.0.1', port: devPort, strictPort: true, proxy: devProxy.proxy },
    })
    await vite.listen()
  })

  afterAll(async () => {
    await vite?.close()
    await new Promise<void>((resolve) => upstream.close(() => resolve()))
    rmSync(tempRoot, { recursive: true, force: true })
  })

  beforeEach(() => {
    reached.length = 0
    loginUsed = false
    rmSync(join(logDir, REJECTION_LOG_FILE), { force: true })
  })

  function logText(): string {
    try {
      return readFileSync(join(logDir, REJECTION_LOG_FILE), 'utf8')
    } catch {
      return ''
    }
  }

  function records(): Record<string, unknown>[] {
    return logText()
      .split('\n')
      .filter((line) => line !== '')
      .map((line) => JSON.parse(line) as Record<string, unknown>)
  }

  // 送るヘッダには Cookie と CSRF トークンを必ず含め、記録に漏れないことを確かめる。
  type Case = [name: string, method: string, headers: () => [string, string][], reason: string]
  const secrets = (): [string, string][] => [['Cookie', COOKIE], ['X-CSRF-Token', CSRF]]
  const rejected: Case[] = [
    ['Host localhost:<dev> + Origin が evaloomd の許可値', 'GET', () => [['Host', `localhost:${devPort}`], ['Origin', `http://${upstreamHost}`]], 'origin_mismatch'],
    ['Host 127.0.0.1:<dev> + Origin が evaloomd の許可値', 'GET', () => [['Host', `127.0.0.1:${devPort}`], ['Origin', `http://${upstreamHost}`]], 'origin_mismatch'],
    ['Host localhost:<dev> + Origin http://127.0.0.1:8080', 'POST', () => [['Host', `localhost:${devPort}`], ['Origin', 'http://127.0.0.1:8080']], 'origin_mismatch'],
    ['Host 127.0.0.1:<dev> + Origin http://127.0.0.1:8080', 'POST', () => [['Host', `127.0.0.1:${devPort}`], ['Origin', 'http://127.0.0.1:8080']], 'origin_mismatch'],
    ['Host が evaloomd の許可値', 'GET', () => [['Host', upstreamHost], ['Origin', `http://${upstreamHost}`]], 'host_mismatch'],
    ['Host 127.0.0.1:8080', 'GET', () => [['Host', '127.0.0.1:8080'], ['Origin', 'http://127.0.0.1:8080']], 'host_mismatch'],
    ['外部 Origin', 'POST', () => [['Host', `localhost:${devPort}`], ['Origin', 'http://evil.example']], 'origin_mismatch'],
    ['Origin null', 'GET', () => [['Host', `localhost:${devPort}`], ['Origin', 'null']], 'origin_mismatch'],
    ['別ポートの Origin', 'GET', () => [['Host', `localhost:${devPort}`], ['Origin', `http://localhost:${devPort + 1}`]], 'origin_mismatch'],
    ['別 scheme の Origin', 'GET', () => [['Host', `localhost:${devPort}`], ['Origin', `https://localhost:${devPort}`]], 'origin_mismatch'],
    ['重複した Origin', 'GET', () => [['Host', `localhost:${devPort}`], ['Origin', `http://localhost:${devPort}`], ['Origin', `http://localhost:${devPort}`]], 'origin_mismatch'],
    ['重複した Host', 'GET', () => [['Host', `localhost:${devPort}`], ['Host', `localhost:${devPort}`], ['Origin', `http://localhost:${devPort}`]], 'host_mismatch'],
    ['許可外の Host（DNS リバインディング）', 'GET', () => [['Host', `evil.example:${devPort}`], ['Origin', `http://evil.example:${devPort}`]], 'host_mismatch'],
    ['Origin 無しの状態変更要求', 'POST', () => [['Host', `localhost:${devPort}`]], 'origin_mismatch'],
  ]

  it.each(rejected)('%s: 403、CSRF トークンを返さず、evaloomd へ届かず、1件だけ記録する', async (_, method, headers, reason) => {
    const reply = await send(devPort, method, '/api/session', [...headers(), ...secrets()])
    expect(reply.status).toBe(403)
    expect(reply.body).not.toContain('csrfToken')
    expect(reply.body).not.toContain(UPSTREAM_CSRF)
    expect(reached).toHaveLength(0)
    const logged = records()
    expect(logged).toHaveLength(1)
    expect(logged[0]).toMatchObject({ method, path: '/api/session', reason })
    expect(Object.keys(logged[0]).sort()).toEqual(['host', 'method', 'origin', 'path', 'reason', 'time'])
    expect(logText()).not.toContain('SESSION-COOKIE-VALUE')
    expect(logText()).not.toContain(CSRF)
  })

  it('/auth/login の拒否でも evaloomd へ届かない（Cookie を発行させず、ログイン URL を消費させない）', async () => {
    const reply = await send(devPort, 'GET', `/auth/login?token=${LOGIN_TOKEN}`, [
      ['Host', `localhost:${devPort}`],
      ['Origin', `http://${upstreamHost}`],
      ...secrets(),
    ])
    expect(reply.status).toBe(403)
    expect(reply.body).not.toContain('csrfToken')
    expect(reply.headers.some((h) => h.toLowerCase().startsWith('set-cookie:'))).toBe(false)
    expect(reached).toHaveLength(0)
    expect(loginUsed).toBe(false)
    const logged = records()
    expect(logged).toHaveLength(1)
    expect(logged[0]).toMatchObject({ method: 'GET', path: '/auth/login', reason: 'origin_mismatch' })
    expect(logText()).not.toContain(LOGIN_TOKEN)
    expect(logText()).not.toContain('SESSION-COOKIE-VALUE')
  })

  // 対照: 正規の要求は evaloomd の許可値へ書き換えられて届き、成功する。
  it.each(['localhost', '127.0.0.1'])('許可 Host（%s:<dev>）と一致する Origin の要求は evaloomd へ届いて成功する', async (name) => {
    for (const method of ['GET', 'POST']) {
      reached.length = 0
      const host = `${name}:${devPort}`
      const reply = await send(devPort, method, '/api/session', [['Host', host], ['Origin', `http://${host}`], ...secrets()])
      expect(reply.status).toBe(200)
      expect(reply.body).toContain(UPSTREAM_CSRF)
      expect(reached).toEqual([{ method, url: '/api/session', host: [upstreamHost], origin: [`http://${upstreamHost}`] }])
    }
    expect(records()).toHaveLength(0)
  })

  it('読み取りの Origin 無しは Origin 無しのまま evaloomd へ届いて成功する', async () => {
    const reply = await send(devPort, 'GET', '/api/session', [['Host', `localhost:${devPort}`], ...secrets()])
    expect(reply.status).toBe(200)
    expect(reply.body).toContain(UPSTREAM_CSRF)
    expect(reached).toEqual([{ method: 'GET', url: '/api/session', host: [upstreamHost], origin: [] }])
    expect(records()).toHaveLength(0)
  })

  it('正規の /auth/login は evaloomd へ届き、Cookie が返る', async () => {
    const reply = await send(devPort, 'GET', `/auth/login?token=${LOGIN_TOKEN}`, [['Host', `127.0.0.1:${devPort}`]])
    expect(reply.status).toBe(200)
    expect(loginUsed).toBe(true)
    expect(reply.headers.some((h) => h.toLowerCase().startsWith('set-cookie:'))).toBe(true)
    expect(records()).toHaveLength(0)
  })

  it('中継しないパス（画面の HTML など）は照合の対象外', async () => {
    await send(devPort, 'GET', '/', [['Host', `localhost:${devPort}`], ['Origin', 'http://evil.example']])
    expect(reached).toHaveLength(0)
    expect(records()).toHaveLength(0)
  })
})
