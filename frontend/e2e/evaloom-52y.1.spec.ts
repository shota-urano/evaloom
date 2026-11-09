import { expect, test } from '@playwright/test'

import { testOwner } from './evaloomd.ts'

// evaloom-6pt.3: CLI で設定したパスワードでログインし、配信された管理画面を使う。
// ID・パスワード入力画面の操作は spec 18 の Frontend タスクで検証する。

test('evaloomd が配信する管理画面で、未ログインならログイン画面が表示される', async ({ page }) => {
  await page.goto('/')
  await expect(page.getByText('Evaloom にログイン')).toBeVisible()
})

test('廃止したログイン URL は 404 となり、セッションを発行しない', async ({ page }) => {
  const response = await page.request.get('/auth/login?token=retired')
  expect(response.status()).toBe(404)
  expect((await response.json()).error.code).toBe('not_found')
  expect(response.headers()['set-cookie']).toBeUndefined()
  expect((await page.request.get('/api/session')).status()).toBe(401)
  await page.goto('/')
  await expect(page.getByText('Evaloom にログイン')).toBeVisible()
})

test('違う ID と違うパスワードは同じ 401 を返し、ログインしない', async ({ page, baseURL }) => {
  const errors: unknown[] = []
  for (const credentials of [{ ...testOwner, id: 'unknown' }, { ...testOwner, password: 'incorrect password' }]) {
    const response = await page.request.post('/auth/login', { headers: { Origin: baseURL! }, data: credentials })
    expect(response.status()).toBe(401)
    expect(response.headers()['set-cookie']).toBeUndefined()
    errors.push(await response.json())
  }
  expect(errors[0]).toEqual(errors[1])
  expect((await page.request.get('/api/session')).status()).toBe(401)
})

test('ログインの入力が API の定義と違うと 400 details を返す', async ({ page, baseURL }) => {
  const response = await page.request.post('/auth/login', {
    headers: { Origin: baseURL! }, data: { ...testOwner, unexpected: true },
  })
  expect(response.status()).toBe(400)
  const body = await response.json()
  expect(body.error.code).toBe('validation_failed')
  expect(body.error.details.length).toBeGreaterThan(0)
  expect(response.headers()['set-cookie']).toBeUndefined()
})

test('CLI で設定したパスワードでログインでき、再読み込み後もセッションが続く', async ({ page, baseURL }) => {
  const response = await page.request.post('/auth/login', { headers: { Origin: baseURL! }, data: testOwner })
  expect(response.status()).toBe(200)
  expect((await response.json()).csrfToken).toBeTruthy()
  await page.goto('/')
  await expect(page.getByRole('heading', { name: 'Dashboard' })).toBeVisible()
  await page.reload()
  await expect(page.getByRole('heading', { name: 'Dashboard' })).toBeVisible()
})
