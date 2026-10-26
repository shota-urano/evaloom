// @vitest-environment node
import { createServer as createHttpServer, request, type IncomingMessage, type Server } from 'node:http'
import type { AddressInfo } from 'node:net'
import { createServer, type ViteDevServer } from 'vite'
import { afterAll, beforeAll, describe, expect, it } from 'vitest'

import { evaloomdProxy, forwardHeaders } from './dev-proxy.ts'

const TARGET = new URL('http://127.0.0.1:8080')
const ALLOWED = ['localhost:5173', '127.0.0.1:5173']

function raw(headers: [string, string][]): string[] {
  return headers.flat()
}

describe('forwardHeaders（開発サーバの中継で Host / Origin を照合する）', () => {
  it('開発サーバ自身のオリジンからの要求だけ evaloomd の Host と Origin へ書き換える', () => {
    for (const host of ALLOWED) {
      expect(forwardHeaders(raw([['Host', host], ['Origin', `http://${host}`]]), ALLOWED, TARGET)).toEqual({
        host: '127.0.0.1:8080',
        origin: 'http://127.0.0.1:8080',
      })
    }
  })

  it('Origin が無い要求は Origin を付けずに転送する（状態変更なら evaloomd が 403 にする）', () => {
    expect(forwardHeaders(raw([['Host', 'localhost:5173']]), ALLOWED, TARGET)).toEqual({
      host: '127.0.0.1:8080',
      origin: undefined,
    })
  })

  it.each([
    ['他オリジン', [['Origin', 'http://evil.example']]],
    ['null', [['Origin', 'null']]],
    ['許可 Host だが別の許可 Host のオリジン', [['Origin', 'http://127.0.0.1:5173']]],
    ['https', [['Origin', 'https://localhost:5173']]],
  ] as [string, [string, string][]][])('不正な Origin（%s）は書き換えずに転送する', (_, origin) => {
    const headers = forwardHeaders(raw([['Host', 'localhost:5173'], ...origin]), ALLOWED, TARGET)
    expect(headers.origin).toBe(origin[0][1])
    expect(headers.origin).not.toBe('http://127.0.0.1:8080')
  })

  it('重複した Origin は書き換えず、両方の値のまま転送する', () => {
    const headers = forwardHeaders(
      raw([['Host', 'localhost:5173'], ['Origin', 'http://localhost:5173'], ['Origin', 'http://evil.example']]),
      ALLOWED,
      TARGET,
    )
    expect(headers.origin).toEqual(['http://localhost:5173', 'http://evil.example'])
  })

  it.each([
    ['許可外の Host（DNS リバインディング）', [['Host', 'evil.example:5173'], ['Origin', 'http://evil.example:5173']]],
    ['別ポート', [['Host', 'localhost:5174']]],
    ['重複した Host', [['Host', 'localhost:5173'], ['Host', 'evil.example']]],
  ] as [string, [string, string][]][])('%s は Host も Origin も書き換えない', (_, headers) => {
    const result = forwardHeaders(raw(headers), ALLOWED, TARGET)
    expect(result.host).not.toBe('127.0.0.1:8080')
    expect(result.origin).not.toBe('http://127.0.0.1:8080')
  })
})

// 実際の vite 開発サーバの中継を通し、evaloomd 役のサーバが受け取るヘッダを確かめる。
describe('vite 開発サーバの中継（回帰テスト）', () => {
  let upstream: Server
  let vite: ViteDevServer
  let devPort: number
  let upstreamHost: string
  const received: { host: string[]; origin: string[] }[] = []

  beforeAll(async () => {
    upstream = createHttpServer((req: IncomingMessage, res) => {
      const headers = req.rawHeaders
      const pick = (name: string) => {
        const out: string[] = []
        for (let i = 0; i + 1 < headers.length; i += 2) if (headers[i].toLowerCase() === name) out.push(headers[i + 1])
        return out
      }
      received.push({ host: pick('host'), origin: pick('origin') })
      res.setHeader('Content-Type', 'application/json')
      res.end('{"csrfToken":"secret"}')
    })
    await new Promise<void>((resolve) => upstream.listen(0, '127.0.0.1', resolve))
    upstreamHost = `127.0.0.1:${(upstream.address() as AddressInfo).port}`
    const probe = createHttpServer()
    await new Promise<void>((resolve) => probe.listen(0, '127.0.0.1', resolve))
    devPort = (probe.address() as AddressInfo).port
    await new Promise<void>((resolve) => probe.close(() => resolve()))
    vite = await createServer({
      configFile: false,
      logLevel: 'silent',
      server: {
        host: '127.0.0.1',
        port: devPort,
        strictPort: true,
        proxy: { '/api': evaloomdProxy(`http://${upstreamHost}`, devPort) },
      },
    })
    await vite.listen()
  })

  afterAll(async () => {
    await vite?.close()
    await new Promise<void>((resolve) => upstream.close(() => resolve()))
  })

  function send(method: string, headers: [string, string][]) {
    received.length = 0
    return new Promise<number>((resolve, reject) => {
      const req = request({ host: '127.0.0.1', port: devPort, method, path: '/api/session', setHost: false })
      for (const [name, value] of headers) req.setHeader(name, value)
      req.on('response', (res) => {
        res.resume()
        res.on('end', () => resolve(res.statusCode ?? 0))
      })
      req.on('error', reject)
      req.end()
    })
  }

  it('正規の要求は evaloomd の Host / Origin で届く', async () => {
    await send('GET', [['Host', `localhost:${devPort}`], ['Origin', `http://localhost:${devPort}`]])
    expect(received).toEqual([{ host: [upstreamHost], origin: [`http://${upstreamHost}`] }])
  })

  it('Origin の無い GET は Origin 無しのまま届く', async () => {
    await send('GET', [['Host', `localhost:${devPort}`]])
    expect(received).toEqual([{ host: [upstreamHost], origin: [] }])
  })

  it.each(['http://evil.example', 'null'])('不正な Origin（%s）は許可値に書き換えられずに届く', async (origin) => {
    await send('GET', [['Host', `localhost:${devPort}`], ['Origin', origin]])
    expect(received).toHaveLength(1)
    expect(received[0].origin).toEqual([origin])
  })

  it('不正な Origin の POST も書き換えられない', async () => {
    await send('POST', [['Host', `127.0.0.1:${devPort}`], ['Origin', 'http://evil.example']])
    expect(received.every((r) => !r.origin.includes(`http://${upstreamHost}`))).toBe(true)
  })
})
