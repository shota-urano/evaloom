import { useEffect, useRef, useState, type ReactNode } from 'react'

import { ApiError } from '@/api/client'
import { registry, shortSha } from '@/api/registry'
import {
  artifactPath,
  FINAL_EXECUTION_STATUSES,
  tasks,
  type ExecutionDetail,
  type ExecutionHistoryItem,
  type RetryMode,
  type TaskDetail,
} from '@/api/tasks'
import { useSession } from '@/auth/session'
import { ApiErrorAlert } from '@/components/api-error-alert'
import { Loading, PageTitle } from '@/components/page'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { useExecutionStatusEvents, type StatusChanged } from '@/hooks/use-execution-events'
import { useResource } from '@/hooks/use-resource'
import { Link } from '@/lib/link'
import { cn } from '@/lib/utils'
import { ExecutionLog } from './execution-log'
import { CANCELLABLE_TASK_STATUSES, RETRY_MODE_LABELS, RETRYABLE_TASK_STATUSES, TASKS_PATH } from './labels'
import { TaskStatusBadge } from './task-list-page'

/** 成果物の一覧。成果物（artifacts）の一覧を返す本人用 API は仕様（01 5.1）に無いので、取得元を差し替えられるようにしておく。 */
export type ArtifactRef = { id: string; kind: string }
export type ArtifactSource = (execution: ExecutionDetail) => readonly ArtifactRef[] | null
const NO_ARTIFACT_LIST: ArtifactSource = () => null

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

function OverviewCard({ task, projectLink }: { task: TaskDetail; projectLink: ReactNode }) {
  return (
    <Section title="概要">
      <dl className="divide-y">
        <Row label="状態">
          <TaskStatusBadge status={task.status} />
        </Row>
        <Row label="プロジェクト">{projectLink}</Row>
        <Row label="PR">
          {task.prUrl ? (
            <a href={task.prUrl} target="_blank" rel="noreferrer" className="underline underline-offset-4">
              {task.prUrl}
            </a>
          ) : (
            none
          )}
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
    <div className="overflow-x-auto rounded-md border">
      <table className="w-full text-sm">
        <caption className="sr-only">Execution 履歴</caption>
        <thead className="bg-muted/50 text-left">
          <tr>
            {['seq', 'mode', '状態', 'Profile の版', '開始', '終了', 'エラー分類', ''].map((label, index) => (
              <th key={index} scope="col" className="px-3 py-2 font-medium whitespace-nowrap">
                {label}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((execution) => {
            const current = execution.id === selected
            return (
              <tr key={execution.id} aria-current={current ? 'true' : undefined} className={cn('border-t align-top', current && 'bg-secondary/60')}>
                <td className="px-3 py-2 font-mono">#{execution.seq}</td>
                <td className="px-3 py-2">{RETRY_MODE_LABELS[execution.mode] ?? execution.mode}</td>
                <td className="px-3 py-2 font-mono text-xs">{execution.status}</td>
                <td className="px-3 py-2 font-mono text-xs">
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
                <td className="px-3 py-2 font-mono text-xs">{execution.errorClass ?? none}</td>
                <td className="px-3 py-2 text-right">
                  <Button type="button" size="sm" variant={current ? 'secondary' : 'outline'} disabled={current} onClick={() => onSelect(execution.id)} aria-label={`#${execution.seq} を表示`}>
                    {current ? '表示中' : '表示'}
                  </Button>
                </td>
              </tr>
            )
          })}
        </tbody>
      </table>
    </div>
  )
}

function ContextTable({ execution }: { execution: ExecutionDetail }) {
  if (execution.contextRevisions.length === 0) return <p className="text-muted-foreground text-sm">固定した Context はまだありません。</p>
  return (
    <div className="overflow-x-auto rounded-md border">
      <table className="w-full text-sm">
        <caption className="sr-only">固定した Context</caption>
        <thead className="bg-muted/50 text-left">
          <tr>
            <th scope="col" className="px-3 py-2 font-medium">階層</th>
            <th scope="col" className="px-3 py-2 font-medium">パス</th>
            <th scope="col" className="px-3 py-2 font-medium">ハッシュ</th>
          </tr>
        </thead>
        <tbody>
          {execution.contextRevisions.map((revision) => (
            <tr key={revision.id} className="border-t align-top">
              <td className="px-3 py-2 font-mono text-xs">{revision.layer}</td>
              <td className="px-3 py-2 font-mono text-xs break-all">
                {revision.sourcePath}
                <div className="text-muted-foreground">{revision.kind}</div>
              </td>
              <td className="px-3 py-2 font-mono text-xs break-all">{revision.contentHash}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

const GATE_RESULT_TONES: Record<string, string> = {
  pass: 'border-emerald-300 bg-emerald-50 text-emerald-900',
  blocked: 'border-amber-300 bg-amber-50 text-amber-900',
  failed: 'border-red-300 bg-red-50 text-red-900',
}

function GateResults({ execution }: { execution: ExecutionDetail }) {
  if (execution.gateResults.length === 0) return <p className="text-muted-foreground text-sm">検証結果はまだありません。</p>
  return (
    <ul className="grid gap-3">
      {[...execution.gateResults]
        .sort((a, b) => a.gate - b.gate || a.createdAt.localeCompare(b.createdAt))
        .map((gate) => (
          <li key={gate.id} className="rounded-md border p-3" aria-label={`Gate ${gate.gate}`}>
            <div className="flex flex-wrap items-center gap-2">
              <span className="font-medium">Gate {gate.gate}</span>
              <span className={cn('rounded-md border px-2 py-0.5 font-mono text-xs', GATE_RESULT_TONES[gate.result] ?? 'bg-muted')}>{gate.result}</span>
              <span className="text-muted-foreground font-mono text-xs">
                {shortSha(gate.baseSha)} → {shortSha(gate.targetSha)}
              </span>
            </div>
            {gate.gate === 2 ? (
              <p className="text-muted-foreground mt-2 text-xs">実行数・skip 数・確認方法（ID による照合か件数による確認か）は、Execution 詳細の API が返さないため表示できません。</p>
            ) : null}
            {gate.findings.length > 0 ? (
              <ul className="mt-2 grid gap-1 text-sm">
                {gate.findings.map((finding, index) => (
                  <li key={index}>
                    <span className="font-mono text-xs">
                      {finding.path}
                      {finding.line > 0 ? `:${finding.line}` : ''}
                    </span>{' '}
                    <span className="text-muted-foreground font-mono text-xs">[{finding.rule}]</span> {finding.message}
                  </li>
                ))}
              </ul>
            ) : (
              <p className="text-muted-foreground mt-2 text-xs">Finding はありません。</p>
            )}
          </li>
        ))}
    </ul>
  )
}

function Artifacts({ execution, source }: { execution: ExecutionDetail; source: ArtifactSource }) {
  const artifacts = source(execution)
  if (artifacts === null) {
    return (
      <p className="text-muted-foreground text-sm">
        成果物（差分・テストレポートなど）の一覧を返す API が無いため、ここには一覧を出せません。保存済みのログは上のログ欄で表示します。
      </p>
    )
  }
  if (artifacts.length === 0) return <p className="text-muted-foreground text-sm">成果物はありません。</p>
  return (
    <ul className="grid gap-1 text-sm">
      {artifacts.map((artifact) => (
        <li key={artifact.id}>
          <a href={artifactPath(execution.id, artifact.id)} className="font-mono underline underline-offset-4" download>
            {artifact.kind}
          </a>{' '}
          <span className="text-muted-foreground font-mono text-xs">{artifact.id}</span>
        </li>
      ))}
    </ul>
  )
}

function FreshConfirm({ busy, onConfirm, onCancel }: { busy: boolean; onConfirm: () => void; onCancel: () => void }) {
  const confirm = useRef<HTMLButtonElement>(null)
  useEffect(() => confirm.current?.focus(), [])
  return (
    <div className="fixed inset-0 z-50 grid place-items-center bg-black/40 p-4">
      <div
        role="alertdialog"
        aria-modal="true"
        aria-labelledby="fresh-confirm-title"
        aria-describedby="fresh-confirm-description"
        className="bg-background grid w-full max-w-md gap-4 rounded-xl border p-6 shadow-lg"
        onKeyDown={(event) => {
          if (event.key === 'Escape') onCancel()
        }}
      >
        <h2 id="fresh-confirm-title" className="font-semibold">
          最初から再実行しますか
        </h2>
        <p id="fresh-confirm-description" className="text-muted-foreground text-sm">
          前回の作業ツリーを引き継がず、開始点から新しい Execution で作業し直します。前回の Execution の記録はそのまま残ります。
        </p>
        <div className="flex justify-end gap-2">
          <Button type="button" variant="outline" onClick={onCancel} disabled={busy}>
            やめる
          </Button>
          <Button ref={confirm} type="button" onClick={onConfirm} disabled={busy}>
            最初から再実行する
          </Button>
        </div>
      </div>
    </div>
  )
}

type Props = { id: string; artifactSource?: ArtifactSource }

/** タスク詳細（13 3.3）。概要、Execution 履歴、ログ、固定した Context、検証結果、成果物、中止・再実行。 */
export function TaskDetailPage({ id, artifactSource = NO_ARTIFACT_LIST }: Props) {
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
  const project = useResource((signal) => (task.data ? registry.getProject(api, task.data.projectId, signal) : Promise.resolve(undefined)), `project:${task.data?.projectId ?? ''}`)

  // 状態が進みうる Execution の status_changed で読み直す（13 3.5）。続けて届いた遷移はまとめて1回にする。
  const shown = useRef(executions)
  const reloadAll = useRef(() => {})
  useEffect(() => {
    shown.current = executions
    reloadAll.current = () => {
      task.reload()
      execution.reload()
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
      task.reload()
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
  const canCancel = CANCELLABLE_TASK_STATUSES.has(data.status)
  const canRetry = RETRYABLE_TASK_STATUSES.has(data.status)
  const projectLink = (
    <Link to={`/projects/${encodeURIComponent(data.projectId)}`} className="underline-offset-4 hover:underline">
      {project.data?.name ?? data.projectId}
    </Link>
  )

  return (
    <div className="grid gap-6">
      <PageTitle
        title={data.title}
        subtitle={`${data.id} · ${data.shortName}`}
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

      <OverviewCard task={data} projectLink={projectLink} />

      <Section title="Execution 履歴" description="1つのタスクに複数の Execution があります。再実行すると新しい Execution が増えます。">
        <HistoryTable executions={executions} selected={selected?.id} onSelect={setChosen} />
      </Section>

      {selected ? (
        <>
          <Section title={`ログ（#${selected.seq}）`} description="実行中は届いた順に追記します。終了後は保存済みのログを表示します。">
            <ExecutionLog key={selected.id} api={api} executionId={selected.id} status={selected.status} />
          </Section>
          {execution.error ? <ApiErrorAlert error={execution.error} onReload={execution.reload} /> : null}
          {execution.data && execution.data.id === selected.id ? (
            <>
              <Section title={`固定した Context（#${selected.seq}）`}>
                <ContextTable execution={execution.data} />
              </Section>
              <Section title={`検証結果（#${selected.seq}）`}>
                <GateResults execution={execution.data} />
              </Section>
              <Section title={`成果物（#${selected.seq}）`}>
                <Artifacts execution={execution.data} source={artifactSource} />
              </Section>
            </>
          ) : execution.error ? null : (
            <Loading />
          )}
        </>
      ) : null}

      {confirmFresh ? <FreshConfirm busy={busy} onConfirm={() => retry('fresh')} onCancel={() => setConfirmFresh(false)} /> : null}
    </div>
  )
}
