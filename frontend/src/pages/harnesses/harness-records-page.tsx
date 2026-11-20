import { useState } from 'react'

import { ApiError } from '@/api/client'
import { registry, type HarnessRecord } from '@/api/registry'
import { artifactPath } from '@/api/tasks'
import { useSession } from '@/auth/session'
import { ApiErrorAlert } from '@/components/api-error-alert'
import { ConfirmDialog } from '@/components/confirm-dialog'
import { Loading, PageTitle } from '@/components/page'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { useResource } from '@/hooks/use-resource'
import { Link } from '@/lib/link'
import { harnessNotFound, harnessPath } from './labels'

const formatTime = (value: string) => new Date(value).toLocaleString('ja-JP')

type TableProps = {
  records: readonly HarnessRecord[]
  deleting: string | null
  onDelete: (record: HarnessRecord) => void
}

function RecordsTable({ records, deleting, onDelete }: TableProps) {
  if (records.length === 0) return <p className="text-muted-foreground text-sm">記録はまだありません。</p>
  return (
    <div className="overflow-x-auto rounded-lg border">
      <table className="w-full text-sm" aria-label="記録の一覧">
        <thead className="bg-muted/50 text-left">
          <tr>
            {['記録した日付', 'タスク', 'ファイル名', '成果物', '状態', '操作'].map((label) => (
              <th key={label} scope="col" className="px-3 py-2 font-medium whitespace-nowrap">
                {label}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {records.map((record) => {
            const deleted = record.deletedAt !== null
            return (
              <tr key={record.id} data-record={record.id} className={`border-t align-top ${deleted ? 'text-muted-foreground' : ''}`}>
                <td className="px-3 py-2 whitespace-nowrap">{formatTime(record.createdAt)}</td>
                <td className="px-3 py-2">
                  <Link to={`/tasks/${encodeURIComponent(record.taskId)}`} className="underline-offset-4 hover:underline">
                    {record.sourceIdentifier ?? <span className="font-mono">{record.taskId}</span>}
                  </Link>
                </td>
                <td className={`px-3 py-2 font-mono break-all ${deleted ? 'line-through' : ''}`}>{record.fileName}</td>
                <td className="px-3 py-2">
                  <a href={artifactPath(record.executionId, record.artifactId)} target="_blank" rel="noreferrer" className="underline underline-offset-4">
                    内容を開く
                  </a>
                </td>
                <td className="px-3 py-2 whitespace-nowrap">
                  {deleted ? (
                    <span className="rounded border px-1.5 py-0.5 text-xs" title={`消した時刻: ${formatTime(record.deletedAt ?? '')}`}>
                      削除済み
                    </span>
                  ) : (
                    '次の仕事に渡す対象'
                  )}
                </td>
                <td className="px-3 py-2">
                  {deleted ? null : (
                    <Button type="button" size="sm" variant="outline" disabled={deleting !== null} onClick={() => onDelete(record)}>
                      {deleting === record.id ? '消しています…' : '消す'}
                    </Button>
                  )}
                </td>
              </tr>
            )
          })}
        </tbody>
      </table>
    </div>
  )
}

/**
 * 記録の一覧（13 3.8、契約 9章）。スペシャルハーネスごとの記録を新しい順に出し、確認ダイアログを経て消す。
 * 並びはサーバーの応答の順のまま（GET /api/harnesses/{id}/records は created_at の新しい順、同時刻は ID の降順。
 * 秒の端数の桁が揃わない時刻もサーバーが正しく並べるので、画面で並べ替えない）。
 */
export function HarnessRecordsPage({ id }: { id: string }) {
  const { api } = useSession()
  const harness = useResource((signal) => registry.getHarness(api, id, signal), `harness:${id}`)
  const records = useResource((signal) => registry.listHarnessRecords(api, id, signal), `records:${id}`)
  const [confirming, setConfirming] = useState<HarnessRecord | null>(null)
  const [deleting, setDeleting] = useState<string | null>(null)
  const [deleteError, setDeleteError] = useState<unknown>(null)
  const [message, setMessage] = useState<string | null>(null)
  const notFound = harness.data === undefined && !harness.loading && !harness.error
  const error = harness.error ?? (notFound ? harnessNotFound() : null) ?? records.error

  async function remove(record: HarnessRecord) {
    if (deleting) return
    setDeleting(record.id)
    setDeleteError(null)
    setMessage(null)
    try {
      await registry.deleteHarnessRecord(api, record.id)
      setMessage(`記録 ${record.fileName} を消しました。この記録は次の仕事に渡しません。`)
      setConfirming(null)
      records.reload()
    } catch (cause) {
      setDeleteError(cause)
      setConfirming(null)
      // 409・404 は状態が変わったことを示すので、最新の一覧を読み直す（13 6節）。
      if (cause instanceof ApiError && (cause.status === 409 || cause.status === 404)) records.reload()
    } finally {
      setDeleting(null)
    }
  }

  return (
    <div className="grid gap-6">
      <PageTitle
        title={harness.data ? `${harness.data.name} の記録` : '記録の一覧'}
        subtitle={id}
        actions={
          <Button asChild variant="outline">
            <Link to={harnessPath(id)}>詳細へ戻る</Link>
          </Button>
        }
      />
      <ApiErrorAlert
        error={error}
        onReload={() => {
          harness.reload()
          records.reload()
        }}
      />
      {harness.data && records.data ? (
        <Card>
          <CardHeader>
            <CardTitle>記録</CardTitle>
            <CardDescription>
              出す段まで成功した仕事で残した記録です（契約 9章）。消していない記録は、新しいものから次の仕事の作業の段に渡します。消した記録は次の仕事に渡らず、一覧には「削除済み」として残ります。
            </CardDescription>
          </CardHeader>
          <CardContent className="grid gap-4">
            <ApiErrorAlert error={deleteError} title="記録を消せませんでした" />
            {message ? (
              <p role="status" className="text-sm">
                {message}
              </p>
            ) : null}
            <RecordsTable records={records.data} deleting={deleting} onDelete={setConfirming} />
          </CardContent>
        </Card>
      ) : error ? null : (
        <Loading />
      )}
      {confirming ? (
        <ConfirmDialog
          title="記録を消しますか"
          confirmLabel="消す"
          initialFocus="cancel"
          busy={deleting !== null}
          onConfirm={() => void remove(confirming)}
          onCancel={() => setConfirming(null)}
        >
          <p>
            <span className="font-mono">{confirming.fileName}</span>（{confirming.sourceIdentifier ?? confirming.taskId}、{formatTime(confirming.createdAt)}）を消します。
          </p>
          <p>消した記録は次の仕事に渡りません。一覧には「削除済み」として残ります。</p>
        </ConfirmDialog>
      ) : null}
    </div>
  )
}
