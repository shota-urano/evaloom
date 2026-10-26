import { fileURLToPath, URL } from 'node:url'
import tailwindcss from '@tailwindcss/vite'
import react from '@vitejs/plugin-react'
import { defineConfig } from 'vitest/config'

import { evaloomdDevProxy } from './dev-proxy.ts'

// 開発サーバ（make run）から evaloomd へ中継する先。照合と書き換えは dev-proxy.ts。
const evaloomd = process.env.EVALOOMD_URL ?? 'http://127.0.0.1:8080'
// 中継で許可する Host はこのポートから決まるので、別のポートへずれないよう strictPort にする。
const devPort = 5173
// 開発サーバが中継の前に拒否した要求の記録先（01-architecture 3.4・6章）。リポジトリ直下の data/ は git の管理外。
const devProxy = evaloomdDevProxy({
  target: evaloomd,
  devPort,
  prefixes: ['/api', '/auth'],
  logDir: fileURLToPath(new URL('../data/security', import.meta.url)),
})

// https://vite.dev/config/
export default defineConfig({
  plugins: [react(), tailwindcss(), devProxy.plugin],
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  server: {
    port: devPort,
    strictPort: true,
    proxy: devProxy.proxy,
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./src/test/setup.ts'],
    // e2e/ は Playwright（npm run e2e）で動かす。
    exclude: ['**/node_modules/**', 'e2e/**'],
  },
})
