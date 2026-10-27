import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

import { buildEvaloomd, startEvaloomd } from './evaloomd.ts'

/**
 * evaloomd をビルドして E2E_PORT で起動し、ビルド済みの管理画面（dist/）を配信させる。
 * 起動時に出る一回限りのログイン URL を EVALOOM_LOGIN_URL としてテストへ渡す。
 * ビルドしたバイナリは EVALOOMD_BINARY として渡し、専用のデータで別に起動したいテストが使う。
 */
export default async function globalSetup(): Promise<() => Promise<void>> {
  const port = process.env.E2E_PORT ?? '62210'
  const { binary, cleanup } = buildEvaloomd()
  const temp = mkdtempSync(join(tmpdir(), 'evaloom-e2e-'))
  const server = await startEvaloomd(binary, join(temp, 'evaloom.db'), port).catch((cause: unknown) => {
    rmSync(temp, { recursive: true, force: true })
    cleanup()
    throw cause
  })
  process.env.EVALOOM_LOGIN_URL = server.loginURL
  process.env.EVALOOMD_BINARY = binary

  return async () => {
    await server.stop()
    rmSync(temp, { recursive: true, force: true })
    cleanup()
  }
}
