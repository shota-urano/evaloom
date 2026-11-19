import { useState, type ReactNode } from 'react'

import { ApiError } from '@/api/client'
import {
  canContinue,
  decisions,
  decisionTitle,
  detailFindings,
  detailOptions,
  detailText,
  operationDescription,
  targetView,
  withdrawnReasonLabel,
  type DecisionAnswer,
  type DecisionDetail,
  type Finding,
} from '@/api/decisions'
import { tasks } from '@/api/tasks'
import { useSession } from '@/auth/session'
import { ApiErrorAlert } from '@/components/api-error-alert'
import { Loading, PageTitle } from '@/components/page'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Textarea } from '@/components/ui/textarea'
import { useResource } from '@/hooks/use-resource'
import { Link } from '@/lib/link'
import { formatTime } from '@/pages/approvals/labels'
import { LinearIssueLink } from '@/pages/tasks/linear-issue-link'
import { taskDetailPath } from '@/pages/tasks/labels'
import { DecisionStatusBadge } from './decision-list-page'
import { DECISIONS_PATH } from './labels'

type Submit = (decisionId: string, answer: DecisionAnswer) => Promise<void>

/** 項目名と値を並べる。375px では縦に積む。 */
function Fields({ children }: { children: ReactNode }) {
  return <dl className="grid gap-x-4 gap-y-1 text-sm sm:grid-cols-[10rem_1fr]">{children}</dl>
}

function Field({ label, children, testId }: { label: string; children: ReactNode; testId?: string }) {
  return (
    <>
      <dt className="text-muted-foreground mt-1 sm:mt-0">{label}</dt>
      <dd className="min-w-0 break-words" data-testid={testId}>
        {children}
      </dd>
    </>
  )
}

function Section({ title, description, children }: { title: string; description?: ReactNode; children: ReactNode }) {
  return (
    <Card className="gap-4 py-4">
      <CardHeader className="px-4">
        <CardTitle>
          <h2>{title}</h2>
        </CardTitle>
        {description ? <CardDescription>{description}</CardDescription> : null}
      </CardHeader>
      <CardContent className="grid gap-3 px-4">{children}</CardContent>
    </Card>
  )
}

function Findings({ findings }: { findings: Finding[] }) {
  if (findings.length === 0) return <span className="text-muted-foreground">finding はありません</span>
  return (
    <ul aria-label="finding" className="grid gap-1">
      {findings.map((finding, i) => (
        <li key={i} className="rounded-md border px-2 py-1">
          <span className="font-mono text-xs">
            [{finding.severity}] {finding.rule}
          </span>
          <p>{finding.message}</p>
          {finding.location ? (
            <span className="text-muted-foreground font-mono text-xs break-all">
              {finding.location.path}:{finding.location.line}
            </span>
          ) : null}
        </li>
      ))}
    </ul>
  )
}

/** 確かめた結果（summary と finding）。detail に無い項目は「-」。 */
function CheckResult({ detail }: { detail: Record<string, unknown> | null }) {
  return (
    <div className="grid gap-2" data-testid="check-result">
      <p>{detailText(detail, 'summary') ?? '-'}</p>
      <Findings findings={detailFindings(detail)} />
    </div>
  )
}

function Actions({ children }: { children: ReactNode }) {
  return <div className="flex flex-wrap gap-2 [&>button]:min-h-10">{children}</div>
}

/** 判断待ちの詳細と回答（13 3.4）。`/decisions/<id>` で直接開ける。 */
export function DecisionDetailPage({ id }: { id: string }) {
  const { api } = useSession()
  const detail = useResource((signal) => decisions.get(api, id, signal), `decision:${id}`)
  const decision = detail.data
  const taskId = decision?.taskId ?? ''
  const task = useResource((signal) => (taskId ? tasks.get(api, taskId, signal) : Promise.resolve(undefined)), `decision-task:${taskId}`)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>(null)
  const [stale, setStale] = useState(false)
  const [conflict, setConflict] = useState(false)
  const [notice, setNotice] = useState<string | null>(null)
  const [version, setVersion] = useState(0)

  const reload = () => {
    setStale(false)
    setConflict(false)
    setError(null)
    setNotice(null)
    detail.reload()
    setVersion((n) => n + 1)
  }

  const submit: Submit = async (decisionId, answer) => {
    if (busy) return
    setBusy(true)
    setError(null)
    setStale(false)
    setConflict(false)
    setNotice(null)
    try {
      await decisions.answer(api, decisionId, answer)
      setNotice('回答を受け付けました。')
      detail.reload()
      setVersion((n) => n + 1)
    } catch (cause) {
      if (cause instanceof ApiError && cause.status === 422) {
        // basis の不一致など。内容が変わったので読み直させる（13 3.4・6章）。
        setStale(true)
        setError(cause)
      } else if (cause instanceof ApiError && cause.status === 409) {
        // 判断済み・取り下げ済み。最新の状態を読み直して表示する（13 3.4）。
        setConflict(true)
        detail.reload()
        setVersion((n) => n + 1)
      } else {
        setError(cause)
      }
    } finally {
      setBusy(false)
    }
  }

  const open = decision?.status === 'open'
  const disabled = busy || stale

  return (
    <div className="grid gap-4">
      <PageTitle
        title={decision ? decisionTitle(decision) : '本人の判断待ち'}
        subtitle={id}
        actions={
          <Button asChild variant="outline">
            <Link to={DECISIONS_PATH}>一覧へ戻る</Link>
          </Button>
        }
      />
      {notice ? (
        <p role="status" className="rounded-md border border-emerald-300 bg-emerald-50 px-4 py-2 text-sm text-emerald-900">
          {notice}
        </p>
      ) : null}
      {stale ? (
        <Alert variant="warning" data-testid="decision-stale">
          <AlertTitle>内容が変わりました</AlertTitle>
          <AlertDescription>
            <p>表示していた内容が、判断待ちに記録されている内容と一致しませんでした。画面を読み直して、内容を確認し直してください。</p>
            <Button type="button" variant="outline" size="sm" className="mt-2" onClick={reload}>
              画面を読み直す
            </Button>
          </AlertDescription>
        </Alert>
      ) : null}
      {conflict ? (
        <Alert variant="warning" data-testid="decision-conflict">
          <AlertTitle>状態が変わりました</AlertTitle>
          <AlertDescription>
            <p>この判断待ちは、すでに判断済みか取り下げ済みでした。最新の状態を表示しています。</p>
          </AlertDescription>
        </Alert>
      ) : null}
      {stale ? null : <ApiErrorAlert error={error ?? detail.error} onReload={reload} />}
      {decision === undefined ? (
        detail.error ? null : <Loading />
      ) : (
        <>
          <Section title="状態">
            <Fields>
              <Field label="状態">
                <DecisionStatusBadge status={decision.status} />
              </Field>
              {decision.status === 'withdrawn' ? (
                <Field label="取り下げの理由" testId="withdrawn-reason">
                  {withdrawnReasonLabel(decision)}
                </Field>
              ) : null}
              {decision.status === 'answered' ? <Field label="判断した時刻">{formatTime(decision.answeredAt)}</Field> : null}
              <Field label="作成">{formatTime(decision.createdAt)}</Field>
              <Field label="タスク">
                <Link to={taskDetailPath(decision.taskId)} className="font-mono text-xs underline underline-offset-4">
                  {decision.taskId}
                </Link>
                {task.data ? <span className="ml-2">{task.data.title}</span> : null}
              </Field>
            </Fields>
          </Section>
          {decision.kind === 'route' ? (
            <RoutePanel key={version} decision={decision} task={task.data} open={open} disabled={disabled} submit={submit} />
          ) : decision.kind === 'approval' ? (
            <ApprovalPanel key={version} decision={decision} open={open} disabled={disabled} submit={submit} version={version} />
          ) : decision.kind === 'question' ? (
            <QuestionPanel key={version} decision={decision} open={open} disabled={disabled} submit={submit} />
          ) : (
            <BlockedPanel decision={decision} open={open} disabled={disabled} submit={submit} />
          )}
        </>
      )}
    </div>
  )
}

type PanelProps = { decision: DecisionDetail; open: boolean; disabled: boolean; submit: Submit }

function RoutePanel({ decision, task, open, disabled, submit }: PanelProps & { task: Awaited<ReturnType<typeof tasks.get>> | undefined }) {
  const { api } = useSession()
  const routes = useResource((signal) => decisions.routes(api, signal), 'routes')
  const proposal = decision.routeProposal
  const failed = proposal?.status === 'failed'
  const initial = proposal?.proposedLabel ?? ''
  const [label, setLabel] = useState(initial)
  const descriptionOf = (value: string) => proposal?.candidates.find((c) => c.label === value)?.description ?? null
  const selectable = routes.data?.some((r) => r.label === label) ?? false

  return (
    <>
      <Section title="タスク">
        <Fields>
          <Field label="件名">{task?.title ?? '-'}</Field>
          <Field label="本文">
            <p className="whitespace-pre-wrap">{task?.body || '-'}</p>
          </Field>
          <Field label="Issue">{task?.source === 'linear' && task.linear ? <LinearIssueLink task={task} /> : '-'}</Field>
          <Field label="判定したときのラベル" testId="seen-labels">
            {proposal && proposal.seenLabels.length > 0 ? proposal.seenLabels.join(', ') : '-'}
          </Field>
        </Fields>
      </Section>
      <Section title="振り分けの提案">
        {proposal === undefined ? (
          <p className="text-sm">提案の記録がありません。</p>
        ) : failed ? (
          <p className="text-sm" data-testid="proposal-failed">
            提案に失敗しました{proposal.errorClass ? `（${proposal.errorClass}）` : ''}。候補から選んでください。
          </p>
        ) : (
          <Fields>
            <Field label="提案したラベル" testId="proposed-label">
              <span className="font-mono">{proposal.proposedLabel ?? '-'}</span>
            </Field>
            <Field label="理由" testId="proposal-reason">
              <p className="whitespace-pre-wrap">{proposal.reason ?? '-'}</p>
            </Field>
          </Fields>
        )}
      </Section>
      <Section title="振り分け先の候補" description="現在の振り分けの表のラベルです。">
        <ApiErrorAlert error={routes.error} onReload={routes.reload} />
        {routes.data === undefined ? (
          routes.error ? null : <Loading />
        ) : routes.data.length === 0 ? (
          <p className="text-sm">振り分けの表にラベルがありません。</p>
        ) : (
          <fieldset className="grid gap-2" disabled={!open || disabled}>
            <legend className="sr-only">振り分け先の候補</legend>
            {routes.data.map((route) => (
              <label key={route.label} className="flex items-start gap-2 rounded-md border p-2 text-sm">
                <input
                  type="radio"
                  name="route-label"
                  value={route.label}
                  checked={label === route.label}
                  onChange={() => setLabel(route.label)}
                  className="mt-1"
                />
                <span className="min-w-0 break-words">
                  <span className="font-mono">{route.label}</span>
                  {route.label === proposal?.proposedLabel ? <span className="ml-2 text-xs">（提案）</span> : null}
                  <span className="text-muted-foreground block text-xs">
                    {route.projectId ? `Project: ${route.projectId}` : `スペシャルハーネス: ${route.harnessId ?? '-'}`}
                  </span>
                  {descriptionOf(route.label) ? <span className="block text-xs">{descriptionOf(route.label)}</span> : null}
                </span>
              </label>
            ))}
          </fieldset>
        )}
        {open ? (
          <Actions>
            <Button
              type="button"
              disabled={disabled || !selectable || proposal === undefined}
              onClick={() => proposal && submit(decision.id, { action: 'approve', label, basis: proposal.id })}
            >
              選んだラベルで承認する
            </Button>
            <Button type="button" variant="destructive" disabled={disabled} onClick={() => submit(decision.id, { action: 'reject' })}>
              却下する
            </Button>
          </Actions>
        ) : null}
      </Section>
    </>
  )
}

/** 同じ Execution の承認待ちは1つの画面に並べる（Q155）。開いていなければこの1件だけ。 */
function ApprovalPanel({ decision, open, disabled, submit, version }: PanelProps & { version: number }) {
  const { api } = useSession()
  const executionId = decision.executionId ?? ''
  const siblings = useResource(
    async (signal) => {
      if (!open || !executionId) return [decision.id]
      const list = await decisions.list(api, 'open', 'approval', signal)
      const ids = list.filter((d) => d.executionId === executionId).map((d) => d.id)
      return ids.includes(decision.id) ? ids : [decision.id, ...ids]
    },
    `approval-siblings:${decision.id}:${open}:${version}`,
  )
  const profile = useResource(
    async (signal) => {
      if (!executionId) return null
      const profileId = await decisions.executionProfileId(api, executionId, signal)
      return profileId ? (await decisions.profile(api, profileId, signal)).definition : null
    },
    `approval-profile:${executionId}`,
  )
  const ids = siblings.data ?? [decision.id]
  return (
    <>
      {ids.length > 1 ? (
        <Alert variant="warning" data-testid="approval-siblings">
          <AlertTitle>同じ実行で承認待ちが {ids.length} 件あります</AlertTitle>
          <AlertDescription>
            <p>操作ごとに承認・却下を選んでください。1つでも却下すると、この実行は失敗になります。</p>
          </AlertDescription>
        </Alert>
      ) : null}
      {ids.map((itemId) =>
        itemId === decision.id ? (
          <ApprovalItem key={itemId} decision={decision} open={open} disabled={disabled} submit={submit} definition={profile.data} />
        ) : (
          <SiblingApproval key={itemId} id={itemId} disabled={disabled} submit={submit} definition={profile.data} />
        ),
      )}
    </>
  )
}

function SiblingApproval({ id, disabled, submit, definition }: { id: string; disabled: boolean; submit: Submit; definition: ApprovalItemProps['definition'] }) {
  const { api } = useSession()
  const detail = useResource((signal) => decisions.get(api, id, signal), `decision:${id}`)
  if (detail.error) return <ApiErrorAlert error={detail.error} onReload={detail.reload} />
  if (!detail.data) return <Loading />
  return <ApprovalItem decision={detail.data} open={detail.data.status === 'open'} disabled={disabled} submit={submit} definition={definition} />
}

type ApprovalItemProps = PanelProps & { definition: Awaited<ReturnType<typeof decisions.profile>>['definition'] | null | undefined }

function ApprovalItem({ decision, open, disabled, submit, definition }: ApprovalItemProps) {
  const approval = decision.approval
  const operation = approval?.operation ?? detailText(decision.detail, 'operation') ?? '-'
  const target = targetView(approval?.target ?? decision.detail?.target)
  // 画面に出している content_id を送る（02 5.6、13 3.4）。
  const contentId = approval?.contentId ?? detailText(decision.detail, 'contentId') ?? decision.basis ?? ''
  const [ttl, setTtl] = useState('')
  const ttlSec = ttl.trim() === '' ? undefined : Number(ttl)
  const ttlInvalid = ttlSec !== undefined && (!Number.isInteger(ttlSec) || ttlSec < 60)
  const ttlId = `ttl-${decision.id}`
  return (
    <section aria-label={`操作の承認 ${operation}`} data-testid="approval-item" className="grid">
      <Section title={`操作: ${operation}`}>
        <Fields>
          <Field label="操作" testId="approval-operation">
            <span className="font-mono">{operation}</span>
          </Field>
          <Field label="操作の説明">{operationDescription(definition, operation) ?? '-'}</Field>
          <Field label="対象" testId="approval-target">
            {target.url ? (
              <a href={target.url} target="_blank" rel="noreferrer" className="underline underline-offset-4 break-all">
                {target.label ?? target.url}
              </a>
            ) : (
              (target.label ?? '-')
            )}
          </Field>
          <Field label="概要">{approval?.summary ?? detailText(decision.detail, 'summary') ?? '-'}</Field>
          <Field label="中身の識別子" testId="approval-content-id">
            <span className="font-mono text-xs break-all">{contentId || '-'}</span>
          </Field>
          <Field label="承認を求めた段">{approval?.requestedStage ?? detailText(decision.detail, 'requestedStage') ?? '-'}</Field>
          <Field label="確かめた結果">
            {decision.detail && 'findings' in decision.detail ? <Findings findings={detailFindings(decision.detail)} /> : '記録がありません'}
          </Field>
          <Field label="承認の有効期限">{formatTime(approval?.expiresAt)}</Field>
          <Field label="タスク詳細">
            <Link to={taskDetailPath(decision.taskId)} className="underline underline-offset-4">
              成果物はタスク詳細で見る
            </Link>
          </Field>
        </Fields>
        {open ? (
          <>
            <div className="grid gap-1">
              <Label htmlFor={ttlId}>承認の有効時間（秒、空なら全体設定の既定値）</Label>
              <Input
                id={ttlId}
                inputMode="numeric"
                value={ttl}
                onChange={(event) => setTtl(event.target.value)}
                aria-invalid={ttlInvalid || undefined}
                className="max-w-48"
              />
              {ttlInvalid ? <p className="text-destructive text-xs">60 以上の整数を入れてください</p> : null}
            </div>
            <Actions>
              <Button
                type="button"
                disabled={disabled || ttlInvalid || !contentId}
                onClick={() => submit(decision.id, ttlSec === undefined ? { action: 'approve', contentId } : { action: 'approve', contentId, ttlSec })}
              >
                承認する
              </Button>
              <Button type="button" variant="destructive" disabled={disabled || !contentId} onClick={() => submit(decision.id, { action: 'reject', contentId })}>
                却下する
              </Button>
            </Actions>
          </>
        ) : (
          <p className="text-sm">
            <DecisionStatusBadge status={decision.status} />
          </p>
        )}
      </Section>
    </section>
  )
}

/** kind=blocked の4つの原因（02 5.6、13 3.4）。 */
function BlockedPanel({ decision, open, disabled, submit }: PanelProps) {
  const cause = decision.cause
  const message = detailText(decision.detail, 'message')
  if (cause === 'check_blocked') {
    const contentId = decision.basis ?? ''
    return (
      <Section title="確かめた結果">
        <Fields>
          <Field label="メッセージ">{message ?? '-'}</Field>
          <Field label="結果">
            <CheckResult detail={decision.detail} />
          </Field>
          <Field label="中身の識別子" testId="blocked-content-id">
            <span className="font-mono text-xs break-all">{contentId || '-'}</span>
          </Field>
        </Fields>
        {open ? (
          <Actions>
            <Button type="button" disabled={disabled || !contentId} onClick={() => submit(decision.id, { action: 'continue', contentId })}>
              続行する
            </Button>
            <Button type="button" variant="destructive" disabled={disabled || !contentId} onClick={() => submit(decision.id, { action: 'abort', contentId })}>
              中止する
            </Button>
          </Actions>
        ) : null}
      </Section>
    )
  }
  if (cause === 'publish_unknown') {
    const outputs = Array.isArray(decision.detail?.outputs) ? (decision.detail.outputs as { label?: string; url?: string }[]) : []
    return (
      <Section title="出す段の結果が不明です" description="続行すると、出す段を新しく始めます。">
        <Fields>
          <Field label="出す段の message">{message ?? '-'}</Field>
          <Field label="わかっている outputs">
            {outputs.length === 0 ? (
              '-'
            ) : (
              <ul>
                {outputs.map((output, i) => (
                  <li key={i}>
                    {output.url ? (
                      <a href={output.url} target="_blank" rel="noreferrer" className="underline underline-offset-4 break-all">
                        {output.label ?? output.url}
                      </a>
                    ) : (
                      output.label
                    )}
                  </li>
                ))}
              </ul>
            )}
          </Field>
        </Fields>
        {open ? (
          <Actions>
            <Button type="button" disabled={disabled} onClick={() => submit(decision.id, { action: 'continue' })}>
              続行する
            </Button>
            <Button type="button" variant="destructive" disabled={disabled} onClick={() => submit(decision.id, { action: 'abort' })}>
              中止する
            </Button>
          </Actions>
        ) : null}
      </Section>
    )
  }
  // context_conflict / workspace_missing: 中止だけ。続行のボタンは出さない（13 3.4）。
  const guide =
    cause === 'context_conflict'
      ? '中止したあと、矛盾を直してからタスク詳細で再実行してください。'
      : cause === 'workspace_missing'
        ? '中止したあと、タスク詳細で fresh（最初から）を選んで再実行してください。'
        : null
  return (
    <Section title="原因" description={guide ?? undefined}>
      <p className="text-sm whitespace-pre-wrap" data-testid="blocked-message">
        {message ?? '-'}
      </p>
      <p className="text-sm">
        <Link to={taskDetailPath(decision.taskId)} className="underline underline-offset-4">
          タスク詳細で再実行する
        </Link>
      </p>
      {open && !canContinue(cause) ? (
        <Actions>
          <Button type="button" variant="destructive" disabled={disabled} onClick={() => submit(decision.id, { action: 'abort' })}>
            中止する
          </Button>
        </Actions>
      ) : null}
    </Section>
  )
}

function QuestionPanel({ decision, open, disabled, submit }: PanelProps) {
  const question = detailText(decision.detail, 'question')
  const options = detailOptions(decision.detail)
  const [text, setText] = useState('')
  const [option, setOption] = useState<string | null>(null)
  const textId = `answer-${decision.id}`
  return (
    <Section title="作業中の AI の質問">
      <p className="text-sm whitespace-pre-wrap" data-testid="question-text">
        {question ?? '-'}
      </p>
      {options.length > 0 ? (
        <fieldset className="grid gap-2" disabled={!open || disabled}>
          <legend className="mb-1 text-sm font-medium">選択肢</legend>
          {options.map((value) => (
            <label key={value} className="flex items-start gap-2 rounded-md border p-2 text-sm">
              <input type="radio" name="question-option" value={value} checked={option === value} onChange={() => setOption(value)} className="mt-1" />
              <span className="min-w-0 break-words">{value}</span>
            </label>
          ))}
          <label className="flex items-start gap-2 rounded-md border p-2 text-sm">
            <input type="radio" name="question-option" value="" checked={option === null} onChange={() => setOption(null)} className="mt-1" />
            <span>選ばない</span>
          </label>
        </fieldset>
      ) : null}
      {open ? (
        <>
          <div className="grid gap-1">
            <Label htmlFor={textId}>回答</Label>
            <Textarea id={textId} value={text} onChange={(event) => setText(event.target.value)} rows={4} disabled={disabled} />
          </div>
          <Actions>
            <Button type="button" disabled={disabled} onClick={() => submit(decision.id, { action: 'answer', text, option })}>
              回答を送る
            </Button>
            <Button type="button" variant="destructive" disabled={disabled} onClick={() => submit(decision.id, { action: 'abort' })}>
              中止する
            </Button>
          </Actions>
        </>
      ) : null}
    </Section>
  )
}
