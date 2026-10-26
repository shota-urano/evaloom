import { fileURLToPath, URL } from 'node:url'
import tailwindcss from '@tailwindcss/vite'
import react from '@vitejs/plugin-react'
import { defineConfig } from 'vitest/config'

// 開発サーバ（make run）から evaloomd へ中継する先。evaloomd は Host と Origin が
// 127.0.0.1:<port> であることを要求するので、中継時に両方を書き換える。
const evaloomd = process.env.EVALOOMD_URL ?? 'http://127.0.0.1:8080'

const proxy = {
  target: evaloomd,
  changeOrigin: true,
  configure: (server: { on: (event: 'proxyReq', listener: (req: { getHeader(name: string): unknown; setHeader(name: string, value: string): void }) => void) => void }) => {
    server.on('proxyReq', (req) => {
      if (req.getHeader('origin')) req.setHeader('origin', evaloomd)
    })
  },
}

// https://vite.dev/config/
export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  server: {
    proxy: { '/api': proxy, '/auth': proxy },
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./src/test/setup.ts'],
  },
})
