import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState, type FormEvent } from 'react'
import { describe, expect, it, vi } from 'vitest'

import { ApiClient } from '@/api/client'
import { ApiErrorAlert } from '@/components/api-error-alert'
import { TextAreaField, TextField } from '@/components/form-field'
import { Button } from '@/components/ui/button'
import { useApiErrors } from '@/hooks/use-api-errors'
import { apiError, fakeFetch, json } from '@/test/fake-fetch'

const FIELDS = ['title', 'acceptanceCriteria'] as const

/** 共通部品（TextField / ApiErrorAlert / useApiErrors）だけで組んだ最小のフォーム。 */
function DemoForm({ api, onReload }: { api: ApiClient; onReload?: () => void }) {
  const [title, setTitle] = useState('')
  const [criteria, setCriteria] = useState('')
  const { error, setError, clear, fieldErrors, unmatched } = useApiErrors(FIELDS)
  async function submit(event: FormEvent) {
    event.preventDefault()
    clear()
    try {
      await api.post('/api/tasks', { title, acceptanceCriteria: criteria })
    } catch (cause) {
      setError(cause)
    }
  }
  return (
    <form onSubmit={submit} noValidate>
      <ApiErrorAlert error={error} unmatchedDetails={unmatched} onReload={onReload} />
      <TextField name="title" label="件名" value={title} onChange={(e) => setTitle(e.target.value)} errors={fieldErrors.title} />
      <TextAreaField
        name="acceptanceCriteria"
        label="受け入れ条件"
        value={criteria}
        onChange={(e) => setCriteria(e.target.value)}
        errors={fieldErrors.acceptanceCriteria}
      />
      <Button type="submit">作成</Button>
    </form>
  )
}

async function renderAndSubmit(response: () => Response, onReload?: () => void) {
  const fake = fakeFetch((request) => (request.url === '/api/session' ? json(200, { csrfToken: 't' }) : response()))
  const api = new ApiClient(fake.fetch)
  await api.loadSession()
  render(<DemoForm api={api} onReload={onReload} />)
  await userEvent.type(screen.getByLabelText('件名'), 'テスト')
  await userEvent.click(screen.getByRole('button', { name: '作成' }))
  return fake
}

function fieldContainer(label: string) {
  return screen.getByLabelText(label).closest('[data-field]') as HTMLElement
}

describe('400 validation_failed の details をフォームの項目に表示する（13 6節）', () => {
  it('details の field に対応する項目の下にだけ表示し、入力欄を invalid にする', async () => {
    await renderAndSubmit(() =>
      apiError(400, 'validation_failed', 'invalid request', [{ field: 'acceptanceCriteria', reason: 'required' }]),
    )
    const criteria = screen.getByLabelText('受け入れ条件')
    const message = await within(fieldContainer('受け入れ条件')).findByText('入力してください')
    expect(criteria).toHaveAttribute('aria-invalid', 'true')
    expect(criteria).toHaveAccessibleDescription('入力してください')
    expect(message).toBeVisible()

    const title = screen.getByLabelText('件名')
    expect(title).not.toHaveAttribute('aria-invalid')
    expect(within(fieldContainer('件名')).queryByText('入力してください')).toBeNull()
    expect(screen.getByRole('alert')).toHaveTextContent('入力内容を確認してください')
  })

  it('複数項目の details をそれぞれの項目に出し、フォームに無い項目は上部に一覧する', async () => {
    await renderAndSubmit(() =>
      apiError(400, 'validation_failed', 'invalid request', [
        { field: 'title', reason: 'too_long' },
        { field: 'acceptanceCriteria', reason: 'required' },
        { field: 'projectId', reason: 'not_found' },
      ]),
    )
    expect(await within(fieldContainer('件名')).findByText('長すぎます')).toBeVisible()
    expect(within(fieldContainer('受け入れ条件')).getByText('入力してください')).toBeVisible()
    expect(screen.getByRole('alert')).toHaveTextContent('projectId: 見つかりません')
    expect(within(fieldContainer('件名')).queryByText(/projectId/)).toBeNull()
  })

  it('未知の reason は原文を添えて表示する', async () => {
    await renderAndSubmit(() => apiError(400, 'validation_failed', 'invalid', [{ field: 'title', reason: 'reserved_word' }]))
    expect(await within(fieldContainer('件名')).findByText('値が正しくありません（reserved_word）')).toBeVisible()
  })

  it('再送信で成功したら項目のエラー表示を消す', async () => {
    let first = true
    await renderAndSubmit(() => {
      if (first) {
        first = false
        return apiError(400, 'validation_failed', 'invalid', [{ field: 'title', reason: 'required' }])
      }
      return json(201, { id: 'task-1' })
    })
    expect(await within(fieldContainer('件名')).findByText('入力してください')).toBeVisible()
    await userEvent.click(screen.getByRole('button', { name: '作成' }))
    await vi.waitFor(() => expect(within(fieldContainer('件名')).queryByText('入力してください')).toBeNull())
    expect(screen.getByLabelText('件名')).not.toHaveAttribute('aria-invalid')
    expect(screen.queryByRole('alert')).toBeNull()
  })
})

describe('401 / 403 / 409 / 422 の表示（13 6節）', () => {
  it('401 はログインが必要であることを表示する', async () => {
    await renderAndSubmit(() => apiError(401, 'unauthenticated', 'owner session is missing or invalid'))
    expect(await screen.findByRole('alert')).toHaveTextContent('ログインが必要です')
  })

  it('403 は操作できないことを表示する（CSRF 失敗を含む）', async () => {
    await renderAndSubmit(() => apiError(403, 'forbidden', 'CSRF token is missing or invalid'))
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('この操作は実行できません')
    expect(alert).toHaveTextContent('CSRF')
  })

  it('409 は状態が変わったことを表示し、最新を読み直すボタンを出す', async () => {
    const onReload = vi.fn()
    await renderAndSubmit(() => apiError(409, 'conflict', 'task is already completed'), onReload)
    expect(await screen.findByRole('alert')).toHaveTextContent('状態が変わりました')
    await userEvent.click(screen.getByRole('button', { name: '最新を読み直す' }))
    expect(onReload).toHaveBeenCalledTimes(1)
  })

  it('422 は前提条件の不成立とサーバーの理由を表示し、details もフォーム項目ではなく一覧に出す', async () => {
    await renderAndSubmit(() =>
      apiError(422, 'precondition_failed', 'local repository path is not a Git repository', [
        { field: 'path', reason: 'not_git_repository' },
      ]),
    )
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('前提条件が満たされていません')
    expect(alert).toHaveTextContent('local repository path is not a Git repository')
    expect(alert).toHaveTextContent('path: Git リポジトリではありません')
    expect(screen.getByLabelText('件名')).not.toHaveAttribute('aria-invalid')
  })
})
