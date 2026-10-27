import { spawn, spawnSync, type SpawnSyncOptions } from 'node:child_process'
import { existsSync, mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

export const root = fileURLToPath(new URL('..', import.meta.url))
export const frontend = join(root, 'frontend')
export const backend = join(root, 'backend')
export const web = join(frontend, 'dist')

export type Evaloomd = {
  origin: string
  loginURL: string
  pid: number
  exited: Promise<void>
  stop: () => Promise<void>
}

export function run(command: string, args: string[], options: SpawnSyncOptions = {}): string {
  const result = spawnSync(command, args, { encoding: 'utf8', ...options })
  if (result.status !== 0) {
    throw new Error(`${command} ${args.join(' ')} が失敗した（code ${result.status}）: ${String(result.stderr ?? result.error ?? '')}`)
  }
  return String(result.stdout ?? '')
}

/** E2E 用。demo は中断可能な非同期ビルドを使う。 */
export function buildEvaloomd(): { binary: string; cleanup: () => void } {
  if (!existsSync(join(web, 'index.html'))) throw new Error('frontend/dist が無い。先に npm run build を実行すること')
  const temp = mkdtempSync(join(tmpdir(), 'evaloom-e2e-bin-'))
  const binary = join(temp, 'evaloomd')
  const cleanup = () => rmSync(temp, { recursive: true, force: true })
  try {
    run('go', ['build', '-o', binary, './cmd/evaloomd'], { cwd: backend, stdio: 'inherit' })
    return { binary, cleanup }
  } catch (error) {
    cleanup()
    throw error
  }
}

/** 一回限りのログイン URL を読む。停止時は終了を待ってから呼び出し元へ戻る。 */
export async function startEvaloomd(binary: string, db: string, port: string, signal?: AbortSignal): Promise<Evaloomd> {
  signal?.throwIfAborted()
  const child = spawn(binary, ['-db', db, '-port', port, '-web', web], { stdio: ['ignore', 'pipe', 'pipe'] })
  let output = ''
  let stopping = false
  const closed = new Promise<void>((resolve) => child.once('close', () => resolve()))
  const exited = closed.then(() => {
    if (!stopping) throw new Error(`evaloomd が終了した（code ${child.exitCode}）: ${output}`)
  })
  // 起動完了前の終了も、未処理の Promise rejection にしない。
  void exited.catch(() => {})
  const stop = async () => {
    stopping = true
    if (child.exitCode !== null || child.signalCode !== null) return closed
    child.kill('SIGTERM')
    const timer = setTimeout(() => child.kill('SIGKILL'), 7_000)
    try { await closed } finally { clearTimeout(timer) }
  }
  const abort = () => { void stop() }
  signal?.addEventListener('abort', abort, { once: true })
  void closed.then(() => signal?.removeEventListener('abort', abort))
  try {
    const started = await new Promise<{ origin: string; loginURL: string }>((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error(`evaloomd が起動しない: ${output}`)), 15_000)
      const onData = (chunk: Buffer) => {
        output = (output + chunk.toString('utf8')).slice(-64_000)
        const listening = /listening on (\S+)/.exec(output)
        const login = /owner login URL: (\S+)/.exec(output)
        if (listening && login) {
          clearTimeout(timer)
          resolve({ origin: `http://${listening[1]}`, loginURL: login[1] })
        }
      }
      child.stdout.on('data', onData)
      child.stderr.on('data', onData)
      child.once('error', (error) => { clearTimeout(timer); reject(error) })
      child.once('close', () => {
        clearTimeout(timer)
        reject(new Error(`evaloomd が終了した（code ${child.exitCode}）: ${output}`))
      })
    })
    signal?.throwIfAborted()
    return { ...started, pid: child.pid!, exited, stop }
  } catch (error) {
    await stop()
    throw error
  }
}
