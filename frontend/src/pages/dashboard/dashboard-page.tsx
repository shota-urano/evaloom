import { useEffect, useRef, useState, type ReactNode } from 'react'

import type { ApiClient } from '@/api/client'

import { approvals, type Approval } from '@/api/approvals'
import { linearStatus } from '@/api/linear'
import { registry } from '@/api/registry'
import {
  notificationFailureKey,
  notificationFailureReasonLabel,
  notificationFailures,
  type NotificationFailure,
} from '@/api/notification-failures'
import { tasks, type TaskSummary } from '@/api/tasks'
import { useSession } from '@/auth/session'
import { ApiErrorAlert } from '@/components/api-error-alert'
import { Loading, PageTitle } from '@/components/page'
import { RunnerTable } from '@/components/runner-table'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { useResource } from '@/hooks/use-resource'
import { Link } from '@/lib/link'
import { cn } from '@/lib/utils'
import { APPROVALS_PATH, approvalDetailPath, approvalTargetLabel } from '@/pages/approvals/labels'
import { useLiveTaskList } from '@/pages/tasks/live'
import { taskDetailPath } from '@/pages/tasks/labels'
import { TaskStatusBadge } from '@/pages/tasks/task-list-page'
import { LinearPollFailures, LinearStatusSection } from './linear-status'
import { summarize, TASK_GROUP_LABELS, TASK_GROUPS, type TaskGroup } from './summary'

function Stat({ testId, label, value, more, tone }: { testId: string; label: string; value: number; more?: boolean; tone?: string }) {
  return (
    <div data-testid={testId} className={cn('rounded-xl border px-4 py-3', value > 0 ? tone : undefined)}>
      <dt className="text-muted-foreground text-sm">{label}</dt>
      <dd className="mt-1 text-2xl font-semibold tabular-nums">
        {value}
        <span className="text-muted-foreground ml-1 text-sm font-normal">件{more ? '以上' : ''}</span>
      </dd>
    </div>
  )
}

const GROUP_TONES: Record<TaskGroup, string> = {
  running: 'border-blue-300 bg-blue-50',
  waiting: 'border-sky-300 bg-sky-50',
  blocked: 'border-amber-300 bg-amber-50',
  error: 'border-red-300 bg-red-50',
}

const TITLES: Record<TaskGroup, string> = {
  running: '実行中のタスク',
  waiting: '待機中のタスク',
  blocked: 'Blocked のタスク',
  error: 'エラー状況',
}

const EMPTY: Record<TaskGroup, string> = {
  running: '実行中のタスクはありません。',
  waiting: '待機中のタスクはありません。',
  blocked: 'Blocked のタスクはありません。',
  error: '失敗したタスクはありません。',
}

type NotificationState = {
  items: NotificationFailure[] | undefined
  /** 続きのページがあるか（nextCursor が null でない）。 */
  hasMore: boolean
  error: unknown
  loading: boolean
  loadingMore: boolean
  loadMore: () => void
  reload: () => void
}

/**
 * 通知失敗の一覧（12 5、Q41）を1ページ目から読み、loadMore で nextCursor の続きを足していく。
 * 再読み込みで1ページ目からやり直す（1ページ目が読み直されたら、足した続きは捨てる）。
 */
function useNotificationFailures(api: ApiClient): NotificationState {
  const first = useResource((signal) => notificationFailures.list(api, null, signal), 'notification-failures')
  const [extra, setExtra] = useState<{ base: unknown; items: NotificationFailure[]; nextCursor: string | null }>()
  const [more, setMore] = useState<{ base: unknown; error: unknown; loading: boolean }>()
  const controller = useRef<AbortController | null>(null)
  useEffect(() => () => controller.current?.abort(), [])

  const base = first.data
  const current = base === undefined ? undefined : extra?.base === base ? extra : { items: base.items, nextCursor: base.nextCursor }
  const moreState = more?.base === base ? more : undefined

  const loadMore = () => {
    if (!current?.nextCursor || moreState?.loading) return
    controller.current?.abort()
    const request = new AbortController()
    controller.current = request
    setMore({ base, error: null, loading: true })
    notificationFailures.list(api, current.nextCursor, request.signal).then(
      (page) => {
        if (request.signal.aborted) return
        setExtra({ base, items: [...current.items, ...page.items], nextCursor: page.nextCursor })
        setMore({ base, error: null, loading: false })
      },
      (error: unknown) => {
        if (!request.signal.aborted) setMore({ base, error, loading: false })
      },
    )
  }
  const reload = () => {
    controller.current?.abort()
    setMore(undefined)
    first.reload()
  }
  return {
    items: current?.items,
    hasMore: Boolean(current?.nextCursor),
    error: first.error ?? moreState?.error ?? null,
    loading: first.loading,
    loadingMore: moreState?.loading ?? false,
    loadMore,
    reload,
  }
}

/**
 * エラー状況のうち通知失敗（12 3.4・5・6節）。全 Execution の notification_failed と、Execution の無い契機の
 * Linear コメントの送信の失敗（linear_outbox の行、16 3.4.2、Q82）を、応答の順（記録した時刻の新しい順）のまま出す。
 * linear_outbox の行は outboxId で見分け、Execution の ID を出さない（13 3.2）。
 */
function NotificationFailures({ state }: { state: NotificationState }) {
  return (
    <div className="grid gap-2" data-testid="notification-failures" aria-busy={state.loading || state.loadingMore || undefined}>
      <h3 className="text-sm font-semibold">通知失敗</h3>
      <p className="text-muted-foreground text-xs">
        すべての Execution の notification_failed と、Execution の無い契機の Linear コメントの送信の失敗を、記録した時刻の新しい順に表示します。
      </p>
      <ApiErrorAlert error={state.error} />
      {state.items === undefined ? (
        state.error ? null : <Loading />
      ) : state.items.length === 0 ? (
        <p className="text-muted-foreground text-sm">通知失敗はありません。</p>
      ) : (
        <ul className="divide-y" aria-label="通知失敗">
          {state.items.map((failure) => {
            const fromOutbox = failure.outboxId != null
            return (
              <li key={notificationFailureKey(failure)} className="py-2" data-source={fromOutbox ? 'linear_outbox' : 'execution_event'}>
                <Link to={taskDetailPath(failure.taskId)} className="font-medium underline-offset-4 hover:underline">
                  {failure.taskTitle || failure.taskId}
                </Link>
                <div className="text-sm">
                  {notificationFailureReasonLabel(failure) ?? '通知に失敗'}
                  <span className="text-muted-foreground font-mono text-xs">
                    {' '}
                    （reason: {failure.reason || '不明'} · kind: {failure.kind || '不明'} · channel: {failure.channel || '不明'}）
                  </span>
                </div>
                <div className="text-muted-foreground font-mono text-xs">
                  <time dateTime={failure.createdAt}>{new Date(failure.createdAt).toLocaleString('ja-JP')}</time>
                  {!fromOutbox && failure.executionId ? <span data-testid="notification-failure-execution"> · {failure.executionId}</span> : null}
                </div>
              </li>
            )
          })}
        </ul>
      )}
      {state.hasMore ? (
        <div>
          <Button type="button" variant="outline" size="sm" onClick={state.loadMore} disabled={state.loadingMore}>
            さらに表示
          </Button>
        </div>
      ) : null}
    </div>
  )
}

/** 承認待ち（17 3.3・3.4、13 3.10 の追加）。件数と、詳細へのリンク。 */
function PendingApprovals({ items, error, onReload }: { items: Approval[] | undefined; error: unknown; onReload: () => void }) {
  return (
    <section aria-labelledby="dashboard-approvals" className="grid gap-3">
      <h2 id="dashboard-approvals" className="font-semibold">
        承認
      </h2>
      <ApiErrorAlert error={error} onReload={onReload} />
      {items === undefined ? (
        error ? null : <Loading />
      ) : (
        <div className="grid gap-3 sm:grid-cols-[minmax(0,12rem)_1fr]">
          <dl>
            <Stat testId="approval-count-pending" label="承認待ち" value={items.length} tone="border-sky-300 bg-sky-50" />
          </dl>
          <Card>
            <CardContent className="grid gap-2">
              {items.length === 0 ? (
                <p className="text-muted-foreground text-sm">承認待ちはありません。</p>
              ) : (
                <ul className="divide-y" aria-label="承認待ち">
                  {items.map((approval) => (
                    <li key={approval.id} className="py-2">
                      <Link to={approvalDetailPath(approval.id)} className="font-medium underline-offset-4 hover:underline">
                        {approvalTargetLabel(approval)}
                      </Link>
                      <div className="text-muted-foreground font-mono text-xs">
                        {approval.contentVersion.slice(0, 12)} · {approval.taskId}
                      </div>
                    </li>
                  ))}
                </ul>
              )}
              <div>
                <Link to={APPROVALS_PATH} className="text-sm underline underline-offset-4">
                  Approvals 画面を開く
                </Link>
              </div>
            </CardContent>
          </Card>
        </div>
      )}
    </section>
  )
}

function TaskGroupCard({
  group,
  items,
  projectName,
  children,
}: {
  group: TaskGroup
  items: TaskSummary[]
  projectName: Map<string, string>
  children?: ReactNode
}) {
  const title = TITLES[group]
  const listLabel = group === 'error' ? '失敗したタスク' : title
  return (
    <Card data-group={group}>
      <CardHeader>
        <CardTitle>
          <h2>{title}</h2>
        </CardTitle>
        {group === 'error' ? (
          <CardDescription>
            Failed と Timed Out のタスク、通知失敗、および Linear の照会の失敗。
          </CardDescription>
        ) : null}
      </CardHeader>
      <CardContent className="grid gap-4">
        {group === 'error' ? <h3 className="text-sm font-semibold">失敗したタスク</h3> : null}
        {items.length === 0 ? (
          <p className="text-muted-foreground text-sm">{EMPTY[group]}</p>
        ) : (
          <ul className="divide-y" aria-label={listLabel}>
            {items.map((task) => (
              <li key={task.id} className="flex flex-wrap items-center justify-between gap-2 py-2">
                <div>
                  <Link to={taskDetailPath(task.id)} className="font-medium underline-offset-4 hover:underline">
                    {task.title}
                  </Link>
                  <div className="text-muted-foreground font-mono text-xs">
                    {task.id} · {projectName.get(task.projectId) ?? task.projectId}
                    {task.latestExecution?.errorClass ? ` · ${task.latestExecution.errorClass}` : ''}
                  </div>
                </div>
                <TaskStatusBadge status={task.status} />
              </li>
            ))}
          </ul>
        )}
        {children}
      </CardContent>
    </Card>
  )
}

/** Dashboard（13 3.2）。Runner 状態、実行中・待機中・Blocked のタスク、エラー状況、Linear の取り込みの状況（15 5章）。 */
export function DashboardPage() {
  const { api } = useSession()
  const runners = useResource((signal) => registry.listRunners(api, signal), 'runners')
  const list = useResource((signal) => tasks.list(api, {}, signal), 'tasks')
  const projects = useResource((signal) => registry.listProjects(api, signal), 'projects')
  useLiveTaskList(api, list.data, list.reload)
  const notifications = useNotificationFailures(api)
  const linear = useResource((signal) => linearStatus.get(api, signal), 'linear-status')
  const pendingApprovals = useResource((signal) => approvals.list(api, 'pending', signal), 'approvals:pending')

  const reload = () => {
    pendingApprovals.reload()
    linear.reload()
    runners.reload()
    list.reload()
    projects.reload()
    notifications.reload()
  }
  const error = runners.error ?? list.error ?? projects.error
  const projectName = new Map((projects.data ?? []).map((p) => [p.id, p.name]))
  const summary = runners.data && list.data ? summarize(runners.data, list.data) : undefined

  return (
    <div className="grid gap-6">
      <PageTitle
        title="Dashboard"
        actions={
          <Button type="button" variant="outline" onClick={reload}>
            再読み込み
          </Button>
        }
      />
      <ApiErrorAlert error={error} onReload={reload} />
      {summary === undefined ? (
        error ? null : <Loading />
      ) : (
        <>
          <section aria-labelledby="dashboard-runners" className="grid gap-3">
            <h2 id="dashboard-runners" className="font-semibold">
              Runner
            </h2>
            <dl className="grid grid-cols-2 gap-3 sm:grid-cols-4">
              <Stat testId="runner-count-online" label="online" value={summary.runners.online} tone="border-emerald-300 bg-emerald-50" />
              <Stat testId="runner-count-offline" label="offline" value={summary.runners.offline} />
            </dl>
            <RunnerTable runners={runners.data ?? []} />
          </section>

          <PendingApprovals items={pendingApprovals.data} error={pendingApprovals.error} onReload={pendingApprovals.reload} />

          <section aria-labelledby="dashboard-tasks" className="grid gap-3">
            <h2 id="dashboard-tasks" className="font-semibold">
              タスク
            </h2>
            <dl className="grid grid-cols-2 gap-3 sm:grid-cols-5">
              {TASK_GROUPS.map((group) => (
                <Stat
                  key={group}
                  testId={`task-count-${group}`}
                  label={TASK_GROUP_LABELS[group]}
                  value={summary.tasks[group].length}
                  tone={GROUP_TONES[group]}
                />
              ))}
              <Stat testId="notification-failure-count" label="通知失敗" value={notifications.items?.length ?? 0}
                more={notifications.hasMore}
                tone={GROUP_TONES.error} />
            </dl>
            <div className="grid gap-4 lg:grid-cols-2">
              {TASK_GROUPS.map((group) => (
                <TaskGroupCard key={group} group={group} items={summary.tasks[group]} projectName={projectName}>
                  {group === 'error' ? (
                    <>
                      <NotificationFailures state={notifications} />
                      <LinearPollFailures teams={linear.data?.teams} error={linear.error} />
                    </>
                  ) : null}
                </TaskGroupCard>
              ))}
            </div>
          </section>

          <LinearStatusSection status={linear.data} error={linear.error} onReload={linear.reload} />
        </>
      )}
    </div>
  )
}
