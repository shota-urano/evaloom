import { defineConfig } from '@playwright/test'

// E2E は evaloomd がビルド済みの管理画面（dist/）を配信する本番と同じ構成で動かす（13 3.1）。
// evaloomd の起動とログイン URL の受け取りは e2e/global-setup.ts が行う。
// evaloomd は Host が 127.0.0.1:<port> の要求だけを受けるので、baseURL も 127.0.0.1 にする。
const PORT = Number(process.env.E2E_PORT ?? 62210)

export default defineConfig({
  testDir: './e2e',
  outputDir: './test-results', // 動画・スクショの出力先。make artifacts がここから回収する
  globalSetup: './e2e/global-setup.ts',
  // ログイン URL は一回限りなので、テストは順番に1つずつ動かす。
  fullyParallel: false,
  workers: 1,
  retries: 0, // flaky を retry で隠さない
  reporter: [['list']],
  use: {
    baseURL: `http://127.0.0.1:${PORT}`,
    trace: 'retain-on-failure',
    video: { mode: 'on', size: { width: 1280, height: 720 } },
  },
})
