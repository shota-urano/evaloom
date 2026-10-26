// 開発サーバ（make run）から evaloomd への中継（01-architecture 3.4「開発時の例外」）。
//
// evaloomd は Host と Origin が 127.0.0.1:<port> であることを要求し、その許可値は緩めない。
// 開発サーバは evaloomd へ転送する前に、受信した Host と Origin を照合する。
// - 照合に通った要求だけ、Host と Origin を evaloomd の許可値へ書き換えて転送する。
// - 照合に通らない要求は、開発サーバが 403 を返して終える。evaloomd へは転送しない。
//   受信した値がたまたま evaloomd の許可値と一致していても転送しない（転送すると
//   evaloomd の境界検証を通ってしまい、他オリジンが CSRF トークンを読めるため）。
// - 拒否した要求は evaloomd の audit_logs に残らないので、data/security/ 配下のファイルへ
//   1行1件の JSON で記録する（時刻、メソッド、パス、受信した Host と Origin、拒否理由）。
//   Cookie・トークンの値は記録しない。パスにはクエリ（ログイン URL の token）を含めない。

import { appendFileSync, mkdirSync } from 'node:fs'
import type { IncomingMessage, ServerResponse } from 'node:http'
import { join } from 'node:path'
import type { Plugin, ProxyOptions } from 'vite'

export type RejectReason = 'host_mismatch' | 'origin_mismatch'

/** 拒否の記録を書くファイル名（logDir の下に置く）。 */
export const REJECTION_LOG_FILE = 'dev-proxy-rejections.jsonl'

/** rawHeaders から指定ヘッダの値をすべて取り出す（重複を検出するため）。 */
export function headerValues(rawHeaders: readonly string[], name: string): string[] {
  const values: string[] = []
  for (let i = 0; i + 1 < rawHeaders.length; i += 2) {
    if (rawHeaders[i].toLowerCase() === name) values.push(rawHeaders[i + 1])
  }
  return values
}

/** evaloomd と同じく、安全なメソッド以外（未知のメソッドを含む）は状態を変える要求として扱う。 */
export function changesState(method: string | undefined): boolean {
  return method !== 'GET' && method !== 'HEAD' && method !== 'OPTIONS'
}

/**
 * 転送してよい要求かを照合する。通れば null、通らなければ拒否理由を返す。
 * - Host は 1つだけで、開発サーバの許可値（allowedHosts）のどれかであること。
 * - Origin は 1つだけで `http://<受信 Host>` と一致すること。読み取り（GET / HEAD / OPTIONS）に
 *   限り Origin 無しを許す。状態を変える要求の Origin 無しは evaloomd と同じく拒否する。
 */
export function checkRequest(
  method: string | undefined,
  rawHeaders: readonly string[],
  allowedHosts: readonly string[],
): RejectReason | null {
  const hosts = headerValues(rawHeaders, 'host')
  if (hosts.length !== 1 || !allowedHosts.includes(hosts[0])) return 'host_mismatch'
  const origins = headerValues(rawHeaders, 'origin')
  if (origins.length > 1) return 'origin_mismatch'
  if (origins.length === 1 && origins[0] !== `http://${hosts[0]}`) return 'origin_mismatch'
  if (origins.length === 0 && changesState(method)) return 'origin_mismatch'
  return null
}

/** 記録用に受信値をまとめる。無ければ null、重複していれば配列のまま残す。 */
function received(values: string[]): string | string[] | null {
  if (values.length === 0) return null
  return values.length === 1 ? values[0] : values
}

export type RejectionRecord = {
  time: string
  method: string
  path: string
  host: string | string[] | null
  origin: string | string[] | null
  reason: RejectReason
}

/** 拒否の記録を作る。Cookie・トークンを含みうるヘッダとクエリは入れない。 */
export function rejectionRecord(req: IncomingMessage, reason: RejectReason, now: Date = new Date()): RejectionRecord {
  const url = req.url ?? ''
  const queryAt = url.search(/[?#]/)
  return {
    time: now.toISOString(),
    method: req.method ?? '',
    path: queryAt === -1 ? url : url.slice(0, queryAt),
    host: received(headerValues(req.rawHeaders, 'host')),
    origin: received(headerValues(req.rawHeaders, 'origin')),
    reason,
  }
}

const MESSAGES: Record<RejectReason, string> = {
  host_mismatch: 'request host is not allowed',
  origin_mismatch: 'request origin is not allowed',
}

export type DevProxyOptions = {
  /** evaloomd の URL（例: http://127.0.0.1:8080）。 */
  target: string
  /** 開発サーバの待ち受けポート。許可する Host はここから決まる。 */
  devPort: number
  /** 中継するパスの接頭辞。vite の server.proxy と同じく前方一致で判定する。 */
  prefixes: readonly string[]
  /** 拒否を記録するディレクトリ（data/security）。 */
  logDir: string
}

/**
 * 中継の前に置く照合の middleware。中継するパスの要求だけを照合し、
 * 通らなければ 403 を返して終える（next を呼ばないので中継されない）。
 */
export function devProxyGuard(options: Omit<DevProxyOptions, 'target'>) {
  const allowedHosts = [`localhost:${options.devPort}`, `127.0.0.1:${options.devPort}`]
  const logFile = join(options.logDir, REJECTION_LOG_FILE)
  return (req: IncomingMessage, res: ServerResponse, next: () => void) => {
    const url = req.url ?? ''
    if (!options.prefixes.some((prefix) => url.startsWith(prefix))) return next()
    const reason = checkRequest(req.method, req.rawHeaders, allowedHosts)
    if (reason === null) return next()
    try {
      mkdirSync(options.logDir, { recursive: true })
      appendFileSync(logFile, `${JSON.stringify(rejectionRecord(req, reason))}\n`)
    } catch (cause) {
      // 記録に失敗しても転送はしない（Fail Closed）。
      console.error('[dev-proxy] failed to record a rejected request:', cause)
    }
    res.statusCode = 403
    res.setHeader('Content-Type', 'application/json')
    res.setHeader('Cache-Control', 'no-store')
    res.end(JSON.stringify({ error: { code: 'forbidden', message: MESSAGES[reason], details: [] } }))
  }
}

type ProxyRequest = {
  setHeader(name: string, value: string | string[]): void
  removeHeader(name: string): void
  destroy(error?: Error): void
}

/**
 * vite の開発サーバに中継を組み込む。plugin（照合の middleware）と proxy（server.proxy の値）を
 * 両方とも設定すること。plugin の middleware は vite の中継より前に動く。
 */
export function evaloomdDevProxy(options: DevProxyOptions): { plugin: Plugin; proxy: Record<string, ProxyOptions> } {
  const targetURL = new URL(options.target)
  const allowedHosts = [`localhost:${options.devPort}`, `127.0.0.1:${options.devPort}`]
  const guard = devProxyGuard(options)
  const proxyOptions: ProxyOptions = {
    target: options.target,
    configure: (proxy) => {
      proxy.on('proxyReq', (proxyReq: ProxyRequest, req: IncomingMessage) => {
        // 照合は guard が済ませている。念のためここでも照合し、通らなければ送らずに切る。
        if (checkRequest(req.method, req.rawHeaders, allowedHosts) !== null) {
          proxyReq.destroy(new Error('dev-proxy: request did not pass the Host / Origin check'))
          return
        }
        proxyReq.setHeader('host', targetURL.host)
        if (headerValues(req.rawHeaders, 'origin').length === 0) proxyReq.removeHeader('origin')
        else proxyReq.setHeader('origin', targetURL.origin)
      })
    },
  }
  return {
    plugin: {
      name: 'evaloom-dev-proxy-guard',
      apply: 'serve',
      configureServer(server) {
        // vite 自身の Host 検査（server.allowedHosts）より前に置き、中継するパスの拒否は
        // すべてここで記録する。後ろに置くと vite が先に 403 を返して記録が残らない。
        server.middlewares.stack.unshift({ route: '', handle: guard })
      },
    },
    proxy: Object.fromEntries(options.prefixes.map((prefix) => [prefix, proxyOptions])),
  }
}
