// 開発サーバ（make run）から evaloomd への中継（01-architecture 3.4）。
//
// evaloomd は Host と Origin が 127.0.0.1:<port> であることを要求する。中継先へは
// Host が書き換わるので、正規の要求（開発サーバ自身のオリジンからの要求）に限って
// Host と Origin を evaloomd の値へ書き換える。照合に通らない要求は受信した Host と
// Origin をそのまま転送し、evaloomd に 403 で拒否させて audit_logs に残させる。
// （開発サーバ側で黙って書き換えると、他オリジンの要求が CSRF トークンを読めてしまう）

import type { IncomingMessage } from 'node:http'
import type { ProxyOptions } from 'vite'

export type ForwardHeaders = {
  host: string
  /** undefined なら Origin を付けずに転送する。 */
  origin: string | string[] | undefined
}

/** rawHeaders から指定ヘッダの値をすべて取り出す（重複を検出するため）。 */
export function headerValues(rawHeaders: readonly string[], name: string): string[] {
  const values: string[] = []
  for (let i = 0; i + 1 < rawHeaders.length; i += 2) {
    if (rawHeaders[i].toLowerCase() === name) values.push(rawHeaders[i + 1])
  }
  return values
}

/**
 * 中継先へ送る Host と Origin を決める。
 * - Host は 1つだけで、開発サーバの許可値（allowedHosts）のどれかであること。
 * - Origin は無いか、1つだけで `http://<受信 Host>` と一致すること。
 *   無い場合は書き換えずに無いまま転送する。evaloomd は 3.4 に従い、読み取りは
 *   Origin 無しを許し、状態を変える要求は Origin 無しを 403 にする。
 * 照合に通らなければ受信値のまま転送する（evaloomd が host_mismatch / origin_mismatch で拒否する）。
 */
export function forwardHeaders(
  rawHeaders: readonly string[],
  allowedHosts: readonly string[],
  target: URL,
): ForwardHeaders {
  const hosts = headerValues(rawHeaders, 'host')
  const origins = headerValues(rawHeaders, 'origin')
  const original: ForwardHeaders = {
    host: hosts.length === 1 ? hosts[0] : hosts.join(','),
    origin: origins.length === 0 ? undefined : origins.length === 1 ? origins[0] : origins,
  }
  if (hosts.length !== 1 || !allowedHosts.includes(hosts[0])) return original
  if (origins.length > 1 || (origins.length === 1 && origins[0] !== `http://${hosts[0]}`)) {
    // Host は evaloomd の値にし、Origin だけを受信値のまま送って origin_mismatch で拒否させる。
    return { host: target.host, origin: original.origin }
  }
  return { host: target.host, origin: origins.length === 1 ? target.origin : undefined }
}

type ProxyRequest = {
  setHeader(name: string, value: string | string[]): void
  removeHeader(name: string): void
}

/** vite の server.proxy に渡す設定。devPort は開発サーバの待ち受けポート。 */
export function evaloomdProxy(target: string, devPort: number): ProxyOptions {
  const targetURL = new URL(target)
  const allowedHosts = [`localhost:${devPort}`, `127.0.0.1:${devPort}`]
  return {
    target,
    configure: (proxy) => {
      proxy.on('proxyReq', (proxyReq: ProxyRequest, req: IncomingMessage) => {
        const headers = forwardHeaders(req.rawHeaders, allowedHosts, targetURL)
        proxyReq.setHeader('host', headers.host)
        if (headers.origin === undefined) proxyReq.removeHeader('origin')
        else proxyReq.setHeader('origin', headers.origin)
      })
    },
  }
}
