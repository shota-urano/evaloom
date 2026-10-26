import { mkdirSync } from 'node:fs'
import { dirname } from 'node:path'
import { fileURLToPath } from 'node:url'

import { expect, test, type Request } from '@playwright/test'

// evaloom-52y.1: evaloomd が配信するビルド成果物の表示と、ログイン画面の操作。
// 状態を変える要求の X-CSRF-Token と 400 details のフォーム表示は、まだ画面に状態変更の
// 操作が無いため、src/api/client.test.ts と src/components/api-form.test.tsx で確かめている。

const SCREENSHOT = fileURLToPath(new URL('../../docs/design/screenshots/evaloom-52y.1-login.png', import.meta.url))

function loginURL(): string {
  const url = process.env.EVALOOM_LOGIN_URL
  if (!url) throw new Error('EVALOOM_LOGIN_URL が無い（e2e/global-setup.ts が設定する）')
  return url
}

test('evaloomd が配信する管理画面で、未ログインならログイン画面が表示される', async ({ page }) => {
  await page.goto('/')
  await expect(page.getByText('Evaloom にログイン')).toBeVisible()
  await expect(page.getByLabel('ログイン URL')).toBeVisible()
  mkdirSync(dirname(SCREENSHOT), { recursive: true })
  await page.screenshot({ path: SCREENSHOT, fullPage: true })
})

test('空のまま送るとログイン URL の項目にエラーが出る', async ({ page }) => {
  await page.goto('/')
  await page.getByRole('button', { name: 'ログイン' }).click()
  await expect(page.getByText('ログイン URL を入力してください')).toBeVisible()
  await expect(page.getByLabel('ログイン URL')).toHaveAttribute('aria-invalid', 'true')
})

test('無効なログイン URL はサーバーの拒否がログイン URL の項目に出る', async ({ page }) => {
  await page.goto('/')
  const port = new URL(loginURL()).port
  await page.getByLabel('ログイン URL').fill(`http://127.0.0.1:${port}/auth/login?token=invalid`)
  await page.getByRole('button', { name: 'ログイン' }).click()
  await expect(page.getByText('ログイン URL が無効か、すでに使われています。')).toBeVisible()
  await expect(page.getByLabel('ログイン URL')).toHaveAttribute('aria-invalid', 'true')
})

test('一回限りのログイン URL を貼り付けるとログインでき、再読み込み後もセッションが続く', async ({ page }) => {
  const requests: Request[] = []
  page.on('request', (request) => requests.push(request))
  await page.goto('/')
  await page.getByLabel('ログイン URL').fill(loginURL())
  await page.getByRole('button', { name: 'ログイン' }).click()
  await expect(page.getByRole('heading', { name: 'ログインしました' })).toBeVisible()
  expect(requests.some((r) => r.url().includes('/auth/login?token='))).toBe(true)

  await page.reload()
  await expect(page.getByRole('heading', { name: 'ログインしました' })).toBeVisible()
})
