import { useState } from 'react'

import {
  DECISION_KINDS,
  DECISION_KIND_LABELS,
  DECISION_STATUSES,
  DECISION_STATUS_LABELS,
  decisions,
  decisionTitle,
  withdrawnReasonLabel,
  type Decision,
  type DecisionKind,
  type DecisionStatus,
} from '@/api/decisions'
import { useSession } from '@/auth/session'
import { ApiErrorAlert } from '@/components/api-error-alert'
import { Loading, PageTitle } from '@/components/page'
import { Label } from '@/components/ui/label'
import { NativeSelect } from '@/components/ui/select'
import { useResource } from '@/hooks/use-resource'
import { Link } from '@/lib/link'
import { cn } from '@/lib/utils'
import { formatTime } from '@/pages/approvals/labels'
import { decisionDetailPath } from './labels'

/** 状態は色だけでなく文字でも示す（13 3.9）。 */
export function DecisionStatusBadge({ status }: { status: Decision['status'] }) {
  return (
    <span
      className={cn(
        'inline-block rounded-md border px-2 py-0.5 text-xs',
        status === 'open' && 'border-sky-300 bg-sky-50 text-sky-900',
        status === 'answered' && 'border-emerald-300 bg-emerald-50 text-emerald-900',
        status === 'withdrawn' && 'border-zinc-300 bg-zinc-50 text-zinc-700',
      )}
    >
      {DECISION_STATUS_LABELS[status] ?? status}
    </span>
  )
}

/** 本人の判断待ちの一覧（13 3.4）。open を新しい順に出し、種類で絞り込む。判断済みにも切り替えられる。 */
export function DecisionListPage() {
  const { api } = useSession()
  const [status, setStatus] = useState<DecisionStatus>('open')
  const [kind, setKind] = useState<DecisionKind | ''>('')
  const list = useResource((signal) => decisions.list(api, status, kind || undefined, signal), `decisions:${status}:${kind}`)
  const items = list.data

  return (
    <div className="grid gap-6">
      <PageTitle title="本人の判断待ち" />
      <div className="flex flex-wrap items-end gap-4">
        <div role="group" aria-label="状態" className="flex flex-wrap gap-1">
          {DECISION_STATUSES.map((value) => (
            <button
              key={value}
              type="button"
              aria-pressed={status === value}
              onClick={() => setStatus(value)}
              className={cn(
                'rounded-md border px-3 py-1.5 text-sm',
                status === value ? 'bg-secondary font-medium' : 'text-muted-foreground hover:text-foreground',
              )}
            >
              {DECISION_STATUS_LABELS[value]}
            </button>
          ))}
        </div>
        <div className="grid gap-1">
          <Label htmlFor="decision-kind">種類</Label>
          <NativeSelect id="decision-kind" value={kind} onChange={(event) => setKind(event.target.value as DecisionKind | '')}>
            <option value="">すべて</option>
            {DECISION_KINDS.map((value) => (
              <option key={value} value={value}>
                {DECISION_KIND_LABELS[value]}
              </option>
            ))}
          </NativeSelect>
        </div>
      </div>
      <ApiErrorAlert error={list.error} onReload={list.reload} />
      {items === undefined ? (
        list.error ? null : <Loading />
      ) : items.length === 0 ? (
        <p className="text-muted-foreground text-sm">該当する判断待ちはありません。</p>
      ) : (
        <ul aria-label="判断待ち" className="grid gap-2">
          {items.map((decision) => (
            <li key={decision.id} data-kind={decision.kind} className="rounded-lg border p-3 text-sm">
              <div className="flex flex-wrap items-center gap-2">
                <DecisionStatusBadge status={decision.status} />
                <Link to={decisionDetailPath(decision.id)} className="font-medium underline underline-offset-4">
                  {decisionTitle(decision)}
                </Link>
              </div>
              <div className="text-muted-foreground mt-1 flex flex-wrap gap-x-3 font-mono text-xs break-all">
                <span>{decision.taskId}</span>
                <span>{formatTime(decision.createdAt)}</span>
              </div>
              {decision.status === 'withdrawn' ? (
                <p className="mt-1">取り下げの理由: {withdrawnReasonLabel(decision)}</p>
              ) : null}
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}
