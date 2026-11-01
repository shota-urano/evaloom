import { useEffect, useRef, useState, type ReactNode } from 'react'

import type { ApiClient } from '@/api/client'

import { registry } from '@/api/registry'
import {
  NOTIFICATION_FAILURE_REASONS,
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
import { useLiveTaskList } from '@/pages/tasks/live'
import { taskDetailPath } from '@/pages/tasks/labels'
import { TaskStatusBadge } from '@/pages/tasks/task-list-page'
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

/** エラー状況のうち通知失敗（12 3.4・5・6節）。全 Execution の notification_failed を新しい順に出す。 */
function NotificationFailures({ state }: { state: NotificationState }) {
  return (
    <div className="grid gap-2" data-testid="notification-failures" aria-busy={state.loading || state.loadingMore || undefined}>
      <h3 className="text-sm font-semibold">通知失敗</h3>
      <p className="text-muted-foreground text-xs">すべての Execution の notification_failed を新しい順に表示します。</p>
      <ApiErrorAlert error={state.error} />
      {state.items === undefined ? (
        state.error ? null : <Loading />
      ) : state.items.length === 0 ? (
        <p className="text-muted-foreground text-sm">通知失敗はありません。</p>
      ) : (
        <ul className="divide-y" aria-label="通知失敗">
          {state.items.map((failure) => (
            <li key={failure.eventId} className="py-2">
              <Link to={taskDetailPath(failure.taskId)} className="font-medium underline-offset-4 hover:underline">
                {failure.taskTitle || failure.taskId}
              </Link>
              <div className="text-sm">
                {NOTIFICATION_FAILURE_REASONS[failure.reason] ?? '通知に失敗'}
                <span className="text-muted-foreground font-mono text-xs">
                  {' '}
                  （reason: {failure.reason || '不明'} · kind: {failure.kind || '不明'} · channel: {failure.channel || '不明'}）
                </span>
              </div>
              <div className="text-muted-foreground font-mono text-xs">
                <time dateTime={failure.createdAt}>{new Date(failure.createdAt).toLocaleString('ja-JP')}</time> · {failure.executionId}
              </div>
            </li>
          ))}
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
            Failed と Timed Out のタスク、および通知失敗。
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

/** Dashboard（13 3.2）。Runner 状態、実行中・待機中・Blocked のタスク、エラー状況。 */
export function DashboardPage() {
  const { api } = useSession()
  const runners = useResource((signal) => registry.listRunners(api, signal), 'runners')
  const list = useResource((signal) => tasks.list(api, {}, signal), 'tasks')
  const projects = useResource((signal) => registry.listProjects(api, signal), 'projects')
  useLiveTaskList(api, list.data, list.reload)
  const notifications = useNotificationFailures(api)

  const reload = () => {
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
                  {group === 'error' ? <NotificationFailures state={notifications} /> : null}
                </TaskGroupCard>
              ))}
            </div>
          </section>
        </>
      )}
    </div>
  )
}
