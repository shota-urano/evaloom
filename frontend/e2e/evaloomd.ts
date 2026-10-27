import { spawn, spawnSync, type SpawnSyncOptions } from 'node:child_process'
import { existsSync, mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

const frontend = fileURLToPath(new URL('..', import.meta.url))
const backend = join(frontend, '..', 'backend')
export const web = join(frontend, 'dist')

export type Evaloomd = {
  /** 例: http://127.0.0.1:62210 */
  origin: string
  /** 一回限りのログイン URL。 */
  loginURL: string
  stop: () => Promise<void>
}

export function run(command: string, args: string[], options: SpawnSyncOptions = {}): string {
  const result = spawnSync(command, args, { encoding: 'utf8', ...options })
  if (result.status !== 0) {
    throw new Error(`${command} ${args.join(' ')} が失敗した（code ${result.status}）: ${String(result.stderr ?? '')}`)
  }
  return String(result.stdout ?? '')
}

/** evaloomd を一時ディレクトリへビルドする。戻り値はバイナリのパスと、片付けの関数。 */
export function buildEvaloomd(): { binary: string; cleanup: () => void } {
  if (!existsSync(join(web, 'index.html'))) throw new Error('frontend/dist が無い。先に npm run build を実行すること')
  const temp = mkdtempSync(join(tmpdir(), 'evaloom-e2e-bin-'))
  const binary = join(temp, 'evaloomd')
  const built = spawnSync('go', ['build', '-o', binary, './cmd/evaloomd'], { cwd: backend, stdio: 'inherit' })
  if (built.status !== 0) throw new Error('evaloomd のビルドに失敗した')
  return { binary, cleanup: () => rmSync(temp, { recursive: true, force: true }) }
}

/**
 * ビルド済みの管理画面（dist/）を配信する evaloomd を 127.0.0.1:<port> で起動する。
 * port に 0 を渡すと空いているポートを使う。起動時に出るログイン URL を返す。
 */
export async function startEvaloomd(binary: string, db: string, port: string): Promise<Evaloomd> {
  const child = spawn(binary, ['-db', db, '-port', port, '-web', web], { stdio: ['ignore', 'pipe', 'pipe'] })
  // evaloomd は起動の案内（待ち受けアドレスとログイン URL）を stderr に出す。両方を読む。
  const started = await new Promise<Omit<Evaloomd, 'stop'>>((resolve, reject) => {
    let output = ''
    const timer = setTimeout(() => reject(new Error(`evaloomd が起動しない: ${output}`)), 15_000)
    const onData = (chunk: Buffer) => {
      output += chunk.toString('utf8')
      const listening = /listening on (\S+)/.exec(output)
      const login = /owner login URL: (\S+)/.exec(output)
      if (listening && login) {
        clearTimeout(timer)
        resolve({ origin: `http://${listening[1]}`, loginURL: login[1] })
      }
    }
    child.stdout.on('data', onData)
    child.stderr.on('data', onData)
    child.on('exit', (code) => {
      clearTimeout(timer)
      reject(new Error(`evaloomd が終了した（code ${code}）: ${output}`))
    })
  }).catch((cause: unknown) => {
    child.kill('SIGTERM')
    throw cause
  })
  return {
    ...started,
    stop: async () => {
      child.removeAllListeners('exit')
      if (child.exitCode !== null) return
      child.kill('SIGTERM')
      await new Promise((resolve) => child.once('exit', resolve))
    },
  }
}
