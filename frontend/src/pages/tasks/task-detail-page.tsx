import { useEffect, useRef, useState, type ReactNode } from 'react'

import { ApiError } from '@/api/client'
import { decisionTitle, targetView, type Decision, type Finding } from '@/api/decisions'
import { registry, shortSha } from '@/api/registry'
import {
  artifactPath,
  FINAL_EXECUTION_STATUSES,
  outputLinks,
  tasks,
  type ExecutionDetail,
  type ExecutionHistoryItem,
  type RetryMode,
  type TaskApproval,
  type TaskDetail,
} from '@/api/tasks'
import { useSession } from '@/auth/session'
import { ApiErrorAlert } from '@/components/api-error-alert'
import { ConfirmDialog } from '@/components/confirm-dialog'
import { Loading, PageTitle } from '@/components/page'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { useExecutionStatusEvents, type StatusChanged } from '@/hooks/use-execution-events'
import { useResource } from '@/hooks/use-resource'
import { Link } from '@/lib/link'
import { cn } from '@/lib/utils'
import { decisionDetailPath } from '@/pages/decisions/labels'
import { DESTINATION_KIND_LABELS, destinationPath, taskDestination } from './destination'
import { ExecutionLog } from './execution-log'
import { HeldReview } from './held-review'
import { heldCause } from './intake'
import { CANCELLABLE_TASK_STATUSES, RETRY_MODE_LABELS, RETRYABLE_TASK_STATUSES, TASKS_PATH } from './labels'
import { LinearIssueLink } from './linear-issue-link'
import { RoutingReview } from './routing-review'
import { TaskStatusBadge } from './task-list-page'

function Row({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="grid gap-1 py-2 sm:grid-cols-[10rem_1fr] sm:gap-4">
      <dt className="text-muted-foreground text-sm">{label}</dt>
      <dd className="text-sm break-all">{children}</dd>
    </div>
  )
}

const none = <span className="text-muted-foreground">なし</span>
const time = (value: string | null) => (value ? <time dateTime={value}>{new Date(value).toLocaleString('ja-JP')}</time> : none)

function Section({ title, description, children }: { title: string; description?: string; children: ReactNode }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>
          <h2>{title}</h2>
        </CardTitle>
        {description ? <CardDescription>{description}</CardDescription> : null}
      </CardHeader>
      <CardContent>{children}</CardContent>
    </Card>
  )
}

function Table({ caption, headers, children }: { caption: string; headers: readonly string[]; children: ReactNode }) {
  return (
    <div className="overflow-x-auto rounded-md border">
      <table className="w-full text-sm">
        <caption className="sr-only">{caption}</caption>
        <thead className="bg-muted/50 text-left">
          <tr>
            {headers.map((label, index) => (
              <th key={index} scope="col" className="px-3 py-2 font-medium whitespace-nowrap">
                {label}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>{children}</tbody>
      </table>
    </div>
  )
}

const cell = 'px-3 py-2'
const mono = 'px-3 py-2 font-mono text-xs'

function OutputLinks({ execution }: { execution: ExecutionHistoryItem | undefined }) {
  const links = outputLinks(execution?.outputs)
  if (links.length === 0) return none
  return (
    <ul className="grid gap-1" aria-label="外に出したもの">
      {links.map((output, index) => (
        <li key={`${output.url}#${index}`}>
          <a href={output.url} target="_blank" rel="noreferrer" className="underline underline-offset-4">
            {output.label}
          </a>{' '}
          <span className="text-muted-foreground text-xs break-all">{output.url}</span>
        </li>
      ))}
    </ul>
  )
}

function OpenDecisions({ items, error }: { items: readonly Decision[] | undefined; error: unknown }) {
  if (items === undefined) return error ? <span className="text-muted-foreground">判断待ちを読めませんでした。</span> : <span className="text-muted-foreground">読み込み中…</span>
  if (items.length === 0) return none
  return (
    <ul className="grid gap-1" aria-label="開いている判断待ち">
      {items.map((decision) => (
        <li key={decision.id}>
          <Link to={decisionDetailPath(decision.id)} className="underline underline-offset-4">
            {decisionTitle(decision)}
          </Link>{' '}
          <span className="text-muted-foreground font-mono text-xs">{decision.id}</span>
        </li>
      ))}
    </ul>
  )
}

function OverviewCard({
  task,
  destination,
  latest,
  openDecisions,
}: {
  task: TaskDetail
  destination: ReactNode
  latest: ExecutionHistoryItem | undefined
  openDecisions: { data: readonly Decision[] | undefined; error: unknown }
}) {
  return (
    <Section title="概要">
      <dl className="divide-y">
        <Row label="状態">
          <TaskStatusBadge status={task.status} />
        </Row>
        <Row label="振り分け先">{destination}</Row>
        {task.source === 'linear' && task.linear ? (
          <Row label="Linear Issue">
            <LinearIssueLink task={task} className="font-mono underline underline-offset-4" />{' '}
            <span className="text-muted-foreground text-xs">{task.linear.url}</span>
          </Row>
        ) : null}
        <Row label="外に出したもの">
          <OutputLinks execution={latest} />
        </Row>
        <Row label="判断待ち">
          <OpenDecisions items={openDecisions.data} error={openDecisions.error} />
        </Row>
        <Row label="本文">
          <div className="whitespace-pre-wrap">{task.body}</div>
        </Row>
        <Row label="受け入れ条件">
          <div className="whitespace-pre-wrap">{task.acceptanceCriteria}</div>
        </Row>
      </dl>
    </Section>
  )
}

function HistoryTable({ executions, selected, onSelect }: { executions: readonly ExecutionHistoryItem[]; selected: string | undefined; onSelect: (id: string) => void }) {
  if (executions.length === 0) return <p className="text-muted-foreground text-sm">Execution はまだありません。</p>
  // 新しい順に並べる。
  const rows = [...executions].sort((a, b) => b.seq - a.seq)
  return (
    <Table caption="Execution 履歴" headers={['seq', 'mode', '状態', 'スペシャルハーネスの版', '開始', '終了', 'エラー分類', '']}>
      {rows.map((execution) => {
        const current = execution.id === selected
        return (
          <tr key={execution.id} aria-current={current ? 'true' : undefined} className={cn('border-t align-top', current && 'bg-secondary/60')}>
            <td className="px-3 py-2 font-mono">#{execution.seq}</td>
            <td className={cell}>{RETRY_MODE_LABELS[execution.mode] ?? execution.mode}</td>
            <td className={mono}>{execution.status}</td>
            <td className={mono}>
              {execution.profileVersion ? (
                <>
                  {execution.profileVersion}
                  {execution.profileCommitSha ? <span className="text-muted-foreground" title={execution.profileCommitSha}> · {shortSha(execution.profileCommitSha)}</span> : null}
                </>
              ) : (
                none
              )}
            </td>
            <td className="px-3 py-2 text-xs whitespace-nowrap">{time(execution.startedAt)}</td>
            <td className="px-3 py-2 text-xs whitespace-nowrap">{time(execution.finishedAt)}</td>
            <td className={mono}>{execution.errorClass ?? none}</td>
            <td className="px-3 py-2 text-right">
              <Button type="button" size="sm" variant={current ? 'secondary' : 'outline'} disabled={current} onClick={() => onSelect(execution.id)} aria-label={`#${execution.seq} を表示`}>
                {current ? '表示中' : '表示'}
              </Button>
            </td>
          </tr>
        )
      })}
    </Table>
  )
}

/** 選んだ Execution の、履歴の表に出していない項目（再開した段、契約の版、渡した値、動かした Runner）。 */
function ExecutionFacts({ execution }: { execution: ExecutionDetail }) {
  const params = execution.params === null || execution.params === undefined ? null : JSON.stringify(execution.params, null, 2)
  return (
    <dl className="divide-y">
      <Row label="再開した段">{execution.resumeStage ? <span className="font-mono">{execution.resumeStage}</span> : none}</Row>
      <Row label="スペシャルハーネスの版">
        {execution.profileVersion ? (
          <span className="font-mono">
            {execution.profileVersion}
            {execution.profileCommitSha ? <span className="text-muted-foreground"> · {execution.profileCommitSha}</span> : null}
          </span>
        ) : (
          none
        )}
      </Row>
      <Row label="契約の版">{execution.contractVersion ? <span className="font-mono">{execution.contractVersion}</span> : none}</Row>
      <Row label="渡した値">{params === null ? none : <pre className="bg-muted/50 overflow-x-auto rounded-md border p-2 font-mono text-xs" data-testid="execution-params">{params}</pre>}</Row>
      <Row label="Runner">{execution.runnerId ? <span className="font-mono">{execution.runnerId}</span> : none}</Row>
    </dl>
  )
}

function StageRuns({ execution }: { execution: ExecutionDetail }) {
  if (execution.stageRuns.length === 0) return <p className="text-muted-foreground text-sm">段はまだ始まっていません。</p>
  return (
    <Table caption="段の結果" headers={['段', 'attempt', '状態', 'エラー分類', '開始', '終了', 'message', 'summary']}>
      {execution.stageRuns.map((run) => (
        <tr key={run.id} className="border-t align-top">
          <td className={mono}>{run.stage}</td>
          <td className={mono}>{run.attempt}</td>
          <td className={mono}>{run.status}</td>
          <td className={mono}>{run.errorClass ?? none}</td>
          <td className="px-3 py-2 text-xs whitespace-nowrap">{time(run.startedAt)}</td>
          <td className="px-3 py-2 text-xs whitespace-nowrap">{time(run.finishedAt)}</td>
          <td className="px-3 py-2 text-xs whitespace-pre-wrap">{run.message ?? none}</td>
          <td className="px-3 py-2 text-xs whitespace-pre-wrap">{run.summary ?? none}</td>
        </tr>
      ))}
    </Table>
  )
}

function findingsOf(value: unknown): Finding[] {
  return Array.isArray(value) ? (value as Finding[]) : []
}

function CheckResults({ execution }: { execution: ExecutionDetail }) {
  if (execution.checkResults.length === 0) return <p className="text-muted-foreground text-sm">確かめた結果はまだありません。</p>
  return (
    <div className="grid gap-4">
      {execution.checkResults.map((check) => {
        const findings = findingsOf(check.findings)
        return (
          <article key={check.id} className="grid gap-2 rounded-md border p-3" aria-label={`確かめた結果 ${check.id}`}>
            <dl className="divide-y">
              <Row label="result">
                <span className="font-mono">{check.result}</span>
              </Row>
              <Row label="中身の識別子">{check.contentId ? <span className="font-mono">{check.contentId}</span> : none}</Row>
              <Row label="summary">{check.summary ? <span className="whitespace-pre-wrap">{check.summary}</span> : none}</Row>
            </dl>
            {findings.length === 0 ? (
              <p className="text-muted-foreground text-sm">finding はありません。</p>
            ) : (
              <Table caption="finding" headers={['rule', 'severity', 'message', 'location']}>
                {findings.map((finding, index) => (
                  <tr key={index} className="border-t align-top">
                    <td className={mono}>{finding.rule}</td>
                    <td className={mono}>{finding.severity}</td>
                    <td className="px-3 py-2 text-xs whitespace-pre-wrap">{finding.message}</td>
                    <td className={mono}>{finding.location ? `${finding.location.path}:${finding.location.line}` : none}</td>
                  </tr>
                ))}
              </Table>
            )}
          </article>
        )
      })}
    </div>
  )
}

function Approvals({ items, error, reload }: { items: readonly TaskApproval[] | undefined; error: unknown; reload: () => void }) {
  if (items === undefined) return error ? <ApiErrorAlert error={error} onReload={reload} /> : <Loading />
  if (items.length === 0) return <p className="text-muted-foreground text-sm">承認の記録はありません。</p>
  return (
    <Table caption="承認" headers={['操作', '対象', '中身の識別子', '状態', '無効の理由', '判断', '使用']}>
      {items.map((approval) => {
        const target = targetView(approval.target)
        return (
          <tr key={approval.id} className="border-t align-top">
            <td className={mono}>{approval.operation}</td>
            <td className="px-3 py-2 text-xs">
              {target.url ? (
                <a href={target.url} target="_blank" rel="noreferrer" className="underline underline-offset-4">
                  {target.label ?? target.url}
                </a>
              ) : (
                (target.label ?? none)
              )}
            </td>
            <td className={cn(mono, 'break-all')}>{approval.contentId}</td>
            <td className={mono}>{approval.status}</td>
            <td className={mono}>{approval.invalidatedReason ?? none}</td>
            <td className="px-3 py-2 text-xs whitespace-nowrap">{time(approval.decidedAt)}</td>
            <td className="px-3 py-2 text-xs whitespace-nowrap">{time(approval.consumedAt)}</td>
          </tr>
        )
      })}
    </Table>
  )
}

const LAYER_LABELS: Record<string, string> = { reported: 'reported（記録のみ）' }

function ContextTable({ execution }: { execution: ExecutionDetail }) {
  if (execution.contextRevisions.length === 0) return <p className="text-muted-foreground text-sm">固定した文脈はまだありません。</p>
  return (
    <Table caption="固定した文脈" headers={['階層', 'パス', '版', 'ハッシュ']}>
      {execution.contextRevisions.map((revision) => (
        <tr key={revision.id} className="border-t align-top">
          <td className={mono}>{LAYER_LABELS[revision.layer] ?? revision.layer}</td>
          <td className={cn(mono, 'break-all')}>
            {revision.sourcePath}
            <div className="text-muted-foreground">{revision.kind}</div>
          </td>
          <td className={mono} title={revision.sourceCommitSha}>
            {revision.sourceCommitSha ? shortSha(revision.sourceCommitSha) : none}
          </td>
          <td className={cn(mono, 'break-all')}>{revision.contentHash}</td>
        </tr>
      ))}
    </Table>
  )
}

/** artifacts.kind の表示名（13 3.3「成果物」）。 */
const ARTIFACT_KIND_LABELS: Record<string, string> = {
  out_file: '段の out のファイル',
  work_summary: '作業の概要',
  question: '質問',
  request_json: '要求 JSON',
  result_json: '結果 JSON',
  log: 'ログ',
}

function Artifacts({ execution }: { execution: ExecutionDetail }) {
  if (execution.artifacts.length === 0) return <p className="text-muted-foreground text-sm">成果物はありません。</p>
  return (
    <Table caption="成果物" headers={['種類', '段', 'ファイル']}>
      {execution.artifacts.map((artifact) => (
        <tr key={artifact.id} className="border-t align-top">
          <td className={cell}>{ARTIFACT_KIND_LABELS[artifact.kind] ?? artifact.kind}</td>
          <td className={mono}>{artifact.stage}</td>
          <td className={cn(mono, 'break-all')}>
            <a href={artifactPath(execution.id, artifact.id)} className="underline underline-offset-4" download={artifact.name.split('/').pop()}>
              {artifact.name}
            </a>
          </td>
        </tr>
      ))}
    </Table>
  )
}

function FreshConfirm({ busy, onConfirm, onCancel }: { busy: boolean; onConfirm: () => void; onCancel: () => void }) {
  return (
    <ConfirmDialog title="最初から再実行しますか" confirmLabel="最初から再実行する" busy={busy} onConfirm={onConfirm} onCancel={onCancel}>
      <p>前回の作業ツリーを引き継がず、開始点から新しい Execution で作業し直します。前回の Execution の記録はそのまま残ります。</p>
    </ConfirmDialog>
  )
}

/** タスク詳細（13 3.3）。概要、Execution 履歴、段の結果、ログ、確かめた結果、承認、固定した文脈、成果物、中止・再実行。 */
export function TaskDetailPage({ id }: { id: string }) {
  const { api } = useSession()
  const task = useResource((signal) => tasks.get(api, id, signal), `task:${id}`)
  const executions = task.data?.executions ?? []
  const latest = executions.reduce<ExecutionHistoryItem | undefined>((a, b) => (a && a.seq > b.seq ? a : b), undefined)
  // 選んでいなければ最新の Execution を表示する（再実行で増えたら新しいものへ移る）。
  const [chosen, setChosen] = useState<string | null>(null)
  const selected = executions.find((e) => e.id === chosen) ?? latest
  const execution = useResource(
    (signal) => (selected ? tasks.getExecution(api, selected.id, signal) : Promise.resolve(undefined)),
    `execution:${selected?.id ?? ''}`,
  )
  const openDecisions = useResource((signal) => tasks.openDecisions(api, id, signal), `decisions:${id}`)
  const approvals = useResource((signal) => tasks.approvals(api, id, signal), `approvals:${id}`)
  const destinationRef = task.data ? taskDestination(task.data) : null
  const destinationName = useResource(
    async (signal) => {
      if (!destinationRef) return undefined
      const found = destinationRef.kind === 'project' ? await registry.getProject(api, destinationRef.id, signal) : await registry.getHarness(api, destinationRef.id, signal)
      return found.name
    },
    `destination:${destinationRef?.kind ?? ''}:${destinationRef?.id ?? ''}`,
  )

  // 状態が進みうる Execution の status_changed で読み直す（13 3.5）。続けて届いた遷移はまとめて1回にする。
  const shown = useRef(executions)
  const reloadAll = useRef(() => {})
  useEffect(() => {
    shown.current = executions
    reloadAll.current = () => {
      task.reload()
      execution.reload()
      openDecisions.reload()
      approvals.reload()
    }
  })
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)
  useEffect(() => () => clearTimeout(timer.current), [])
  const watched = executions.filter((e) => !FINAL_EXECUTION_STATUSES.has(e.status)).map((e) => e.id)
  useExecutionStatusEvents(api, watched, (event: StatusChanged) => {
    // 購読を始めると過去のイベントも届く。表示中の状態と同じ遷移先なら読み直さない。
    const current = shown.current.find((e) => e.id === event.executionId)
    if (current && (event.to === current.status || event.reason === 'task_created')) return
    clearTimeout(timer.current)
    timer.current = setTimeout(() => reloadAll.current(), 100)
  })

  const [busy, setBusy] = useState(false)
  const [actionError, setActionError] = useState<unknown>(null)
  const [notice, setNotice] = useState<string | null>(null)
  const [confirmFresh, setConfirmFresh] = useState(false)

  async function act(run: () => Promise<void>, message: string) {
    if (busy) return
    setBusy(true)
    setActionError(null)
    setNotice(null)
    try {
      await run()
      setNotice(message)
      setChosen(null)
      reloadAll.current()
    } catch (cause) {
      setActionError(cause)
      // 409 は状態が変わったことを示すので、最新を読み直す（13 6節）。
      if (cause instanceof ApiError && cause.status === 409) task.reload()
    } finally {
      setBusy(false)
      setConfirmFresh(false)
    }
  }
  const cancel = () => act(() => tasks.cancel(api, id), '中止を受け付けました。')
  const retry = (mode: RetryMode) => act(() => tasks.retry(api, id, mode), `再実行（${RETRY_MODE_LABELS[mode]}）を受け付けました。新しい Execution を履歴に追加しました。`)

  if (task.data === undefined) {
    return (
      <div className="grid gap-6">
        <PageTitle title="タスク詳細" subtitle={id} />
        {task.error ? <ApiErrorAlert error={task.error} onReload={task.reload} /> : <Loading />}
        <p className="text-sm">
          <Link to={TASKS_PATH} className="underline underline-offset-4">
            Tasks へ戻る
          </Link>
        </p>
      </div>
    )
  }

  const data = task.data
  // 取り込みで保留したタスクは続行（再実行を含む）できず、中止だけを保留の確認に出す（15 3.6.3）。
  const held = heldCause(data)
  const canCancel = !held && CANCELLABLE_TASK_STATUSES.has(data.status)
  // 保留のまま中止したタスクにも Execution が無く、再実行の元が無い（サーバーは 409 を返す）ので再実行を出さない。
  const intakeOnly = Boolean(data.intakeBlockCause) && executions.length === 0
  const canRetry = !held && !intakeOnly && RETRYABLE_TASK_STATUSES.has(data.status)
  const destination = destinationRef ? (
    <>
      <span className="text-muted-foreground text-xs">{DESTINATION_KIND_LABELS[destinationRef.kind]}</span>{' '}
      <Link to={destinationPath(destinationRef)} className="underline-offset-4 hover:underline">
        {destinationName.data ?? destinationRef.id}
      </Link>
    </>
  ) : (
    none
  )
  const detail = execution.data && selected && execution.data.id === selected.id ? execution.data : undefined

  return (
    <div className="grid gap-6">
      <PageTitle
        title={data.title}
        subtitle={data.shortName ? `${data.id} · ${data.shortName}` : data.id}
        actions={
          <>
            {canCancel ? (
              <Button type="button" variant="destructive" disabled={busy} onClick={cancel}>
                停止（中止）
              </Button>
            ) : null}
            {canRetry ? (
              <>
                <Button type="button" variant="outline" disabled={busy} onClick={() => retry('continue')}>
                  再実行（継続）
                </Button>
                <Button type="button" variant="outline" disabled={busy} onClick={() => setConfirmFresh(true)}>
                  再実行（最初から）
                </Button>
              </>
            ) : null}
          </>
        }
      />
      {notice ? (
        <p role="status" className="rounded-md border border-emerald-300 bg-emerald-50 px-4 py-2 text-sm text-emerald-900">
          {notice}
        </p>
      ) : null}
      <ApiErrorAlert error={actionError} onReload={task.reload} />
      {task.error ? <ApiErrorAlert error={task.error} onReload={task.reload} /> : null}

      {held ? <HeldReview cause={held} busy={busy} harnessPath={destinationRef?.kind === 'harness' ? destinationPath(destinationRef) : null} onCancel={cancel} /> : null}
      {data.status === 'routing' ? <RoutingReview taskId={data.id} decisions={openDecisions.data} error={openDecisions.error} /> : null}

      <OverviewCard task={data} destination={destination} latest={latest} openDecisions={openDecisions} />

      <Section title="Execution 履歴" description="1つのタスクに複数の Execution があります。再実行すると新しい Execution が増えます。">
        <HistoryTable executions={executions} selected={selected?.id} onSelect={setChosen} />
      </Section>

      {selected ? (
        <>
          {execution.error ? <ApiErrorAlert error={execution.error} onReload={execution.reload} /> : null}
          {detail ? (
            <>
              <Section title={`Execution の詳細（#${selected.seq}）`}>
                <ExecutionFacts execution={detail} />
              </Section>
              <Section title={`段の結果（#${selected.seq}）`}>
                <StageRuns execution={detail} />
              </Section>
            </>
          ) : execution.error ? null : (
            <Loading />
          )}
          <Section title={`ログ（#${selected.seq}）`} description="実行中は届いた順に追記します。終了後は保存済みのログを表示します。">
            <ExecutionLog key={selected.id} api={api} executionId={selected.id} status={selected.status} />
          </Section>
          {detail ? (
            <>
              <Section title={`確かめた結果（#${selected.seq}）`}>
                <CheckResults execution={detail} />
              </Section>
              <Section title={`固定した文脈（#${selected.seq}）`} description="reported はスペシャルハーネスが報告した文脈で、記録として表示するだけです。">
                <ContextTable execution={detail} />
              </Section>
              <Section title={`成果物（#${selected.seq}）`}>
                <Artifacts execution={detail} />
              </Section>
            </>
          ) : null}
        </>
      ) : null}

      <Section title="承認" description="このタスクの承認の記録です。承認は判断待ちの画面で受け付けます。">
        <Approvals items={approvals.data} error={approvals.error} reload={approvals.reload} />
      </Section>

      {confirmFresh ? <FreshConfirm busy={busy} onConfirm={() => retry('fresh')} onCancel={() => setConfirmFresh(false)} /> : null}
    </div>
  )
}
