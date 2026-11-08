import { useState } from 'react'

import type { ApiClient } from '@/api/client'
import { ApiError } from '@/api/client'
import { approvals, isExpired, type ApprovalArtifact, type ApprovalDecision, type ApprovalDetail } from '@/api/approvals'
import { tasks, type GateResult } from '@/api/tasks'
import { useSession } from '@/auth/session'
import { ApiErrorAlert } from '@/components/api-error-alert'
import { Loading, PageTitle } from '@/components/page'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { useResource } from '@/hooks/use-resource'
import { Link } from '@/lib/link'
import { navigate } from '@/lib/router'
import { taskDetailPath } from '@/pages/tasks/labels'
import { ApprovalStatusBadge } from './approval-list-page'
import { APPROVALS_PATH, approvalDetailPath, approvalTargetLabel, formatTime } from './labels'

function GateResults({ gates }: { gates: GateResult[] }) {
  if (gates.length === 0) return <p className="text-muted-foreground text-sm">Gate の結果はありません。</p>
  return (
    <ul className="grid gap-3" aria-label="Gate の結果">
      {gates.map((gate) => (
        <li key={gate.id} className="grid gap-1 text-sm" data-gate={gate.gate}>
          <div>
            <span className="font-medium">Gate {gate.gate}</span> <span className="font-mono text-xs">{gate.result}</span>
            <span className="text-muted-foreground font-mono text-xs"> · 対象 {gate.targetSha}</span>
          </div>
          {gate.findings.length === 0 ? (
            <p className="text-muted-foreground text-xs">Finding はありません。</p>
          ) : (
            <ul className="grid gap-1 pl-4 text-xs">
              {gate.findings.map((finding, index) => (
                <li key={index}>
                  <span className="font-mono">
                    {finding.path || '(ファイルなし)'}
                    {finding.line > 0 ? `:${finding.line}` : ''}
                  </span>{' '}
                  <span className="text-muted-foreground font-mono">[{finding.rule}]</span> {finding.message}
                </li>
              ))}
            </ul>
          )}
        </li>
      ))}
    </ul>
  )
}

function DiffText({ api, executionId, artifact }: { api: ApiClient; executionId: string; artifact: ApprovalArtifact }) {
  const text = useResource((signal) => tasks.getArtifactText(api, executionId, artifact.id, signal), `diff:${executionId}:${artifact.id}`)
  if (text.error) return <ApiErrorAlert error={text.error} />
  if (text.data === undefined) return <p className="text-muted-foreground text-sm">差分を読み込んでいます…</p>
  return (
    <pre aria-label="差分" className="bg-muted max-h-96 overflow-auto rounded-md p-3 font-mono text-xs whitespace-pre">
      {text.data}
    </pre>
  )
}

function Diffs({ api, approval }: { api: ApiClient; approval: ApprovalDetail }) {
  if (approval.diffArtifacts.length === 0) return <p className="text-muted-foreground text-sm">差分はありません。</p>
  return (
    <div className="grid gap-2">
      {approval.diffArtifacts.map((artifact) => (
        <DiffText key={artifact.id} api={api} executionId={approval.executionId} artifact={artifact} />
      ))}
    </div>
  )
}

const DECIDED_NOTICE: Record<ApprovalDecision, string> = {
  approve: '承認を受け付けました。',
  reject: '却下を受け付けました。',
}

/** 承認の詳細と承認・却下（17 3.4・3.6）。 */
export function ApprovalDetailPage({ id }: { id: string }) {
  const { api } = useSession()
  const detail = useResource((signal) => approvals.get(api, id, signal), `approval:${id}`)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>(null)
  const [stale, setStale] = useState(false)
  const [notice, setNotice] = useState<string | null>(null)

  const approval = detail.data

  const reload = () => {
    setStale(false)
    setError(null)
    detail.reload()
  }

  async function decide(decision: ApprovalDecision) {
    if (busy || !approval) return
    setBusy(true)
    setError(null)
    setStale(false)
    setNotice(null)
    try {
      // 画面に表示している content_version（head SHA）を送る（17 3.4）。
      await approvals.decide(api, approval.id, { decision, contentVersion: approval.contentVersion })
      setNotice(DECIDED_NOTICE[decision])
      detail.reload()
    } catch (cause) {
      // content_version の不一致（422）は対象の内容が変わったことを示すので、読み直させる（17 6章）。
      if (cause instanceof ApiError && cause.status === 422) setStale(true)
      else setError(cause)
    } finally {
      setBusy(false)
    }
  }

  async function renew() {
    if (busy || !approval) return
    setBusy(true)
    setError(null)
    setNotice(null)
    try {
      const created = await approvals.renew(api, approval.id)
      navigate(approvalDetailPath(created.id))
    } catch (cause) {
      setError(cause)
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="grid gap-6">
      <PageTitle
        title="承認の詳細"
        subtitle={id}
        actions={
          <Button asChild variant="outline">
            <Link to={APPROVALS_PATH}>一覧へ戻る</Link>
          </Button>
        }
      />
      {notice ? (
        <p role="status" className="rounded-md border border-emerald-300 bg-emerald-50 px-4 py-2 text-sm text-emerald-900">
          {notice}
        </p>
      ) : null}
      {stale ? (
        <Alert variant="warning" data-testid="approval-stale">
          <AlertTitle>内容が変わりました</AlertTitle>
          <AlertDescription>
            <p>表示していた head SHA が、承認待ちに記録されている head SHA と一致しませんでした。画面を読み直して、対象を確認し直してください。</p>
            <Button type="button" variant="outline" size="sm" className="mt-2" onClick={reload}>
              画面を読み直す
            </Button>
          </AlertDescription>
        </Alert>
      ) : null}
      <ApiErrorAlert error={error ?? detail.error} onReload={reload} />
      {approval === undefined ? (
        detail.error ? null : <Loading />
      ) : (
        <>
          <Card>
            <CardHeader>
              <CardTitle>
                <h2>対象</h2>
              </CardTitle>
            </CardHeader>
            <CardContent>
              <dl className="grid gap-2 text-sm sm:grid-cols-[10rem_1fr]">
                <dt className="text-muted-foreground">状態</dt>
                <dd>
                  <ApprovalStatusBadge status={approval.status} />
                  {approval.invalidatedReason ? <span className="ml-2 font-mono text-xs">{approval.invalidatedReason}</span> : null}
                </dd>
                <dt className="text-muted-foreground">操作</dt>
                <dd className="font-mono text-xs">{approval.operation}</dd>
                <dt className="text-muted-foreground">PR</dt>
                <dd data-testid="approval-pr-url">
                  {approval.prUrl ? (
                    <a href={approval.prUrl} target="_blank" rel="noreferrer" className="break-all underline underline-offset-4">
                      {approval.prUrl}
                    </a>
                  ) : (
                    <span className="text-muted-foreground">{approvalTargetLabel(approval)}</span>
                  )}
                </dd>
                <dt className="text-muted-foreground">ブランチ</dt>
                <dd className="font-mono text-xs">
                  {approval.target?.headBranch ?? '-'} → {approval.target?.baseBranch ?? '-'}
                </dd>
                <dt className="text-muted-foreground">head SHA</dt>
                <dd className="font-mono text-xs break-all" data-testid="approval-content-version">
                  {approval.contentVersion}
                </dd>
                <dt className="text-muted-foreground">タスク</dt>
                <dd>
                  <Link to={taskDetailPath(approval.taskId)} className="font-mono text-xs underline underline-offset-4">
                    {approval.taskId}
                  </Link>
                </dd>
                <dt className="text-muted-foreground">作成</dt>
                <dd className="font-mono text-xs">{formatTime(approval.createdAt)}</dd>
                <dt className="text-muted-foreground">判断</dt>
                <dd className="font-mono text-xs">
                  {formatTime(approval.decidedAt)}
                  {approval.decidedBy ? ` · ${approval.decidedBy}` : ''}
                </dd>
                <dt className="text-muted-foreground">有効期限</dt>
                <dd className="font-mono text-xs">{formatTime(approval.expiresAt)}</dd>
                <dt className="text-muted-foreground">使用回数</dt>
                <dd className="font-mono text-xs">
                  {approval.useCount} / {approval.maxUses}
                </dd>
              </dl>
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle>
                <h2>Gate の結果</h2>
              </CardTitle>
            </CardHeader>
            <CardContent>
              <GateResults gates={approval.gateResults} />
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle>
                <h2>差分</h2>
              </CardTitle>
            </CardHeader>
            <CardContent>
              <Diffs api={api} approval={approval} />
            </CardContent>
          </Card>

          {approval.status === 'pending' ? (
            <Card className="border-sky-300">
              <CardHeader>
                <CardTitle>
                  <h2>承認・却下</h2>
                </CardTitle>
                <CardDescription>上の head SHA の内容に対して判断します。承認すると Evaloom がこの PR をマージします。</CardDescription>
              </CardHeader>
              <CardContent className="flex flex-wrap gap-2">
                <Button type="button" disabled={busy || stale} onClick={() => decide('approve')}>
                  承認する
                </Button>
                <Button type="button" variant="destructive" disabled={busy || stale} onClick={() => decide('reject')}>
                  却下する
                </Button>
              </CardContent>
            </Card>
          ) : isExpired(approval) ? (
            <Card className="border-amber-300">
              <CardHeader>
                <CardTitle>
                  <h2>再承認</h2>
                </CardTitle>
                <CardDescription>この承認は有効期限が切れています。新しい承認待ちを作って、もう一度判断してください。</CardDescription>
              </CardHeader>
              <CardContent>
                <Button type="button" variant="outline" disabled={busy} onClick={renew}>
                  再承認を求める
                </Button>
              </CardContent>
            </Card>
          ) : null}
        </>
      )}
    </div>
  )
}
