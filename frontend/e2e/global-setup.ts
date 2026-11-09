import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

import { buildEvaloomd, setupOwner, startEvaloomd } from './evaloomd.ts'

/**
 * evaloomd をビルドして E2E_PORT で起動し、ビルド済みの管理画面（dist/）を配信させる。
 * 起動前に CLI でテスト用の ID・パスワードを設定する。
 * ビルドしたバイナリは EVALOOMD_BINARY として渡し、専用のデータで別に起動したいテストが使う。
 */
export default async function globalSetup(): Promise<() => Promise<void>> {
  const port = process.env.E2E_PORT ?? '62210'
  const { binary, cleanup } = buildEvaloomd()
  const temp = mkdtempSync(join(tmpdir(), 'evaloom-e2e-'))
  const db = join(temp, 'evaloom.db')
  const server = await (async () => {
    setupOwner(binary, db)
    return startEvaloomd(binary, db, port)
  })().catch((cause: unknown) => {
    rmSync(temp, { recursive: true, force: true })
    cleanup()
    throw cause
  })
  process.env.EVALOOMD_BINARY = binary

  return async () => {
    await server.stop()
    rmSync(temp, { recursive: true, force: true })
    cleanup()
  }
}
