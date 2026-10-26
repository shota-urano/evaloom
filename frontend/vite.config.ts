import { fileURLToPath, URL } from 'node:url'
import tailwindcss from '@tailwindcss/vite'
import react from '@vitejs/plugin-react'
import { defineConfig } from 'vitest/config'

import { evaloomdProxy } from './dev-proxy.ts'

// 開発サーバ（make run）から evaloomd へ中継する先。照合と書き換えは dev-proxy.ts。
const evaloomd = process.env.EVALOOMD_URL ?? 'http://127.0.0.1:8080'
// 中継で許可する Host はこのポートから決まるので、別のポートへずれないよう strictPort にする。
const devPort = 5173

// https://vite.dev/config/
export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  server: {
    port: devPort,
    strictPort: true,
    proxy: { '/api': evaloomdProxy(evaloomd, devPort), '/auth': evaloomdProxy(evaloomd, devPort) },
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./src/test/setup.ts'],
  },
})
