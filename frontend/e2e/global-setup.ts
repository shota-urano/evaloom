import { spawn, spawnSync } from 'node:child_process'
import { existsSync, mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

const frontend = fileURLToPath(new URL('..', import.meta.url))
const backend = join(frontend, '..', 'backend')

/**
 * evaloomd をビルドして E2E_PORT で起動し、ビルド済みの管理画面（dist/）を配信させる。
 * 起動時に出る一回限りのログイン URL を EVALOOM_LOGIN_URL としてテストへ渡す。
 */
export default async function globalSetup(): Promise<() => Promise<void>> {
  const port = process.env.E2E_PORT ?? '62210'
  const web = join(frontend, 'dist')
  if (!existsSync(join(web, 'index.html'))) throw new Error('frontend/dist が無い。先に npm run build を実行すること')
  const temp = mkdtempSync(join(tmpdir(), 'evaloom-e2e-'))
  const binary = join(temp, 'evaloomd')
  const built = spawnSync('go', ['build', '-o', binary, './cmd/evaloomd'], { cwd: backend, stdio: 'inherit' })
  if (built.status !== 0) throw new Error('evaloomd のビルドに失敗した')

  const child = spawn(binary, ['-db', join(temp, 'evaloom.db'), '-port', port, '-web', web], {
    stdio: ['ignore', 'pipe', 'pipe'],
  })
  // evaloomd は起動の案内（ログイン URL を含む）を stderr に出す。両方を読む。
  const loginURL = await new Promise<string>((resolve, reject) => {
    let output = ''
    const timer = setTimeout(() => reject(new Error(`evaloomd が起動しない: ${output}`)), 15_000)
    const onData = (chunk: Buffer) => {
      output += chunk.toString('utf8')
      const match = /owner login URL: (\S+)/.exec(output)
      if (match) {
        clearTimeout(timer)
        resolve(match[1])
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
    rmSync(temp, { recursive: true, force: true })
    throw cause
  })
  process.env.EVALOOM_LOGIN_URL = loginURL

  return async () => {
    child.removeAllListeners('exit')
    child.kill('SIGTERM')
    await new Promise((resolve) => child.once('exit', resolve))
    rmSync(temp, { recursive: true, force: true })
  }
}
