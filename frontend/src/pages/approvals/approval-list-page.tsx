import { useState } from 'react'

import { APPROVAL_STATUSES, approvals, type Approval, type ApprovalStatus } from '@/api/approvals'
import { useSession } from '@/auth/session'
import { ApiErrorAlert } from '@/components/api-error-alert'
import { Loading, PageTitle } from '@/components/page'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Label } from '@/components/ui/label'
import { NativeSelect } from '@/components/ui/select'
import { useResource } from '@/hooks/use-resource'
import { Link } from '@/lib/link'
import { cn } from '@/lib/utils'
import { taskDetailPath } from '@/pages/tasks/labels'
import { approvalDetailPath, approvalTargetLabel, formatTime } from './labels'

// 状態は色だけでなく文字でも示す（13 3.5）。色は補助。値は 17 3.9 の approvals.status のまま出す。
const STATUS_TONES: Record<string, string> = {
  pending: 'border-sky-300 bg-sky-50 text-sky-900',
  approved: 'border-blue-300 bg-blue-50 text-blue-900',
  consumed: 'border-emerald-300 bg-emerald-50 text-emerald-900',
  rejected: 'border-red-300 bg-red-50 text-red-900',
  invalidated: 'border-amber-300 bg-amber-50 text-amber-900',
  expired: 'border-amber-300 bg-amber-50 text-amber-900',
}

export function ApprovalStatusBadge({ status }: { status: string }) {
  return (
    <span
      data-status={status}
      className={cn(
        'inline-flex rounded-md border px-2 py-0.5 font-mono text-xs font-medium whitespace-nowrap',
        STATUS_TONES[status] ?? 'bg-muted text-foreground',
      )}
    >
      {status}
    </span>
  )
}

function ApprovalTable({ items, caption, timeLabel }: { items: Approval[]; caption: string; timeLabel: 'created' | 'decided' }) {
  return (
    <div className="overflow-x-auto rounded-xl border">
      <table className="w-full text-sm" aria-label={caption}>
        <caption className="sr-only">{caption}</caption>
        <thead className="bg-muted/50 text-left">
          <tr>
            <th scope="col" className="px-4 py-2 font-medium whitespace-nowrap">
              対象
            </th>
            <th scope="col" className="px-4 py-2 font-medium whitespace-nowrap">
              状態
            </th>
            <th scope="col" className="px-4 py-2 font-medium whitespace-nowrap">
              head SHA
            </th>
            <th scope="col" className="px-4 py-2 font-medium whitespace-nowrap">
              タスク
            </th>
            <th scope="col" className="px-4 py-2 font-medium whitespace-nowrap">
              {timeLabel === 'created' ? '作成' : '判断'}
            </th>
          </tr>
        </thead>
        <tbody>
          {items.map((approval) => (
            <tr key={approval.id} className="border-t align-top" data-approval-id={approval.id}>
              <td className="px-4 py-3">
                <Link to={approvalDetailPath(approval.id)} className="font-medium underline-offset-4 hover:underline">
                  {approvalTargetLabel(approval)}
                </Link>
                <div className="text-muted-foreground font-mono text-xs">
                  {approval.operation}
                  {approval.target?.baseBranch ? ` · ${approval.target.headBranch ?? ''} → ${approval.target.baseBranch}` : ''}
                </div>
              </td>
              <td className="px-4 py-3">
                <ApprovalStatusBadge status={approval.status} />
                {approval.invalidatedReason ? (
                  <div className="text-muted-foreground mt-1 font-mono text-xs">{approval.invalidatedReason}</div>
                ) : null}
              </td>
              <td className="px-4 py-3 font-mono text-xs">{approval.contentVersion.slice(0, 12)}</td>
              <td className="px-4 py-3">
                <Link to={taskDetailPath(approval.taskId)} className="font-mono text-xs underline underline-offset-4">
                  {approval.taskId}
                </Link>
              </td>
              <td className="px-4 py-3 font-mono text-xs whitespace-nowrap">
                {formatTime(timeLabel === 'created' ? approval.createdAt : (approval.decidedAt ?? approval.createdAt))}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

const HISTORY_STATUSES = APPROVAL_STATUSES.filter((s) => s !== 'pending')

/** 履歴の絞り込みは URL の ?status= にも残す。 */
function readHistoryStatus(): ApprovalStatus | '' {
  const value = new URLSearchParams(window.location.search).get('status') ?? ''
  return (HISTORY_STATUSES as readonly string[]).includes(value) ? (value as ApprovalStatus) : ''
}

function writeHistoryStatus(status: string) {
  const query = status ? `?status=${encodeURIComponent(status)}` : ''
  window.history.replaceState(window.history.state, '', `${window.location.pathname}${query}`)
}

/** Approvals 画面（17 3.4、13 3.10 の追加）。承認待ちの一覧と、判断済み・無効・期限切れの履歴。 */
export function ApprovalListPage() {
  const { api } = useSession()
  const [historyStatus, setHistoryStatus] = useState(readHistoryStatus)
  const pending = useResource((signal) => approvals.list(api, 'pending', signal), 'approvals:pending')
  const history = useResource(
    (signal) => approvals.list(api, historyStatus || undefined, signal),
    `approvals:history:${historyStatus}`,
  )
  const historyItems = history.data?.filter((a) => a.status !== 'pending')
  const reload = () => {
    pending.reload()
    history.reload()
  }

  return (
    <div className="grid gap-6">
      <PageTitle
        title="Approvals"
        actions={
          <Button type="button" variant="outline" onClick={reload}>
            再読み込み
          </Button>
        }
      />

      <Card>
        <CardHeader>
          <CardTitle>
            <h2>承認待ち</h2>
          </CardTitle>
          <CardDescription>対象の PR、head SHA、Gate の結果、差分を詳細で確認し、承認か却下を選んでください。</CardDescription>
        </CardHeader>
        <CardContent className="grid gap-3" aria-busy={pending.loading || undefined}>
          <ApiErrorAlert error={pending.error} onReload={pending.reload} />
          {pending.data === undefined ? (
            pending.error ? null : <Loading />
          ) : pending.data.length === 0 ? (
            <p className="text-muted-foreground text-sm">承認待ちはありません。</p>
          ) : (
            <ApprovalTable items={pending.data} caption="承認待ち" timeLabel="created" />
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>
            <h2>履歴</h2>
          </CardTitle>
        </CardHeader>
        <CardContent className="grid gap-3" aria-busy={history.loading || undefined}>
          <form role="search" aria-label="履歴の絞り込み" className="flex flex-wrap items-end gap-4" onSubmit={(e) => e.preventDefault()}>
            <div className="grid gap-2">
              <Label htmlFor="approval-history-status">状態</Label>
              <NativeSelect
                id="approval-history-status"
                className="w-48"
                value={historyStatus}
                onChange={(event) => {
                  const next = event.target.value as ApprovalStatus | ''
                  writeHistoryStatus(next)
                  setHistoryStatus(next)
                }}
              >
                <option value="">すべて</option>
                {HISTORY_STATUSES.map((status) => (
                  <option key={status} value={status}>
                    {status}
                  </option>
                ))}
              </NativeSelect>
            </div>
          </form>
          <ApiErrorAlert error={history.error} onReload={history.reload} />
          {historyItems === undefined ? (
            history.error ? null : <Loading />
          ) : historyItems.length === 0 ? (
            <p className="text-muted-foreground text-sm">{historyStatus ? '条件に合う承認はありません。' : '履歴はありません。'}</p>
          ) : (
            <ApprovalTable items={historyItems} caption="履歴" timeLabel="decided" />
          )}
        </CardContent>
      </Card>
    </div>
  )
}
