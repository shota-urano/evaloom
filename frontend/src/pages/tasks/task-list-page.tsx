import { useState } from 'react'

import { registry } from '@/api/registry'
import { tasks, type TaskFilter, type TaskSummary } from '@/api/tasks'
import { useSession } from '@/auth/session'
import { ApiErrorAlert } from '@/components/api-error-alert'
import { Loading, PageTitle } from '@/components/page'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Label } from '@/components/ui/label'
import { NativeSelect } from '@/components/ui/select'
import { useResource } from '@/hooks/use-resource'
import { Link } from '@/lib/link'
import type { NavigationState } from '@/lib/router'
import { cn } from '@/lib/utils'
import { useLiveTaskList } from './live'
import { TASK_NEW_PATH, TASK_STATUS_FILTERS, taskStatusLabel } from './labels'

// 状態は色だけでなく文字でも示す（13 3.5）。色は補助。
const STATUS_TONES: Record<string, string> = {
  queued: 'border-sky-300 bg-sky-50 text-sky-900',
  running: 'border-blue-300 bg-blue-50 text-blue-900',
  verifying: 'border-indigo-300 bg-indigo-50 text-indigo-900',
  blocked: 'border-amber-300 bg-amber-50 text-amber-900',
  completed: 'border-emerald-300 bg-emerald-50 text-emerald-900',
  failed: 'border-red-300 bg-red-50 text-red-900',
  timed_out: 'border-red-300 bg-red-50 text-red-900',
}

export function TaskStatusBadge({ status }: { status: string }) {
  return (
    <span
      data-status={status}
      className={cn('inline-flex rounded-md border px-2 py-0.5 text-xs font-medium whitespace-nowrap', STATUS_TONES[status] ?? 'bg-muted text-foreground')}
    >
      {taskStatusLabel(status)}
    </span>
  )
}

/** 絞り込みは URL の ?status=&projectId= にも残し、再読み込みしても保つ。 */
function readFilter(): Required<TaskFilter> {
  const params = new URLSearchParams(window.location.search)
  return { status: params.get('status') ?? '', projectId: params.get('projectId') ?? '' }
}

function writeFilter(filter: Required<TaskFilter>) {
  const params = new URLSearchParams()
  if (filter.status) params.set('status', filter.status)
  if (filter.projectId) params.set('projectId', filter.projectId)
  const query = params.toString()
  window.history.replaceState(window.history.state, '', `${window.location.pathname}${query ? `?${query}` : ''}`)
}

function LatestExecution({ task }: { task: TaskSummary }) {
  const execution = task.latestExecution
  if (!execution) return <span className="text-muted-foreground">なし</span>
  return (
    <div>
      <span className="font-mono text-xs">#{execution.seq}</span> <span className="font-mono text-xs">{execution.status}</span>
      {execution.errorClass ? <div className="text-destructive font-mono text-xs">{execution.errorClass}</div> : null}
    </div>
  )
}

/** Tasks 一覧（13 3.2）。タスク、状態、プロジェクト、最新 Execution、PR へのリンク。状態とプロジェクトで絞り込む。 */
export function TaskListPage({ state }: { state?: NavigationState }) {
  const { api } = useSession()
  const [filter, setFilter] = useState(readFilter)
  const list = useResource((signal) => tasks.list(api, filter, signal), `tasks:${filter.status}:${filter.projectId}`)
  const projects = useResource((signal) => registry.listProjects(api, signal), 'projects')
  useLiveTaskList(api, list.data, list.reload)
  const projectName = new Map((projects.data ?? []).map((p) => [p.id, p.name]))
  const filtered = filter.status !== '' || filter.projectId !== ''

  const change = (key: keyof TaskFilter) => (event: { target: { value: string } }) => {
    const next = { ...filter, [key]: event.target.value }
    writeFilter(next)
    setFilter(next)
  }
  const reset = () => {
    const next = { status: '', projectId: '' }
    writeFilter(next)
    setFilter(next)
  }
  const error = list.error ?? projects.error
  const reload = () => {
    list.reload()
    projects.reload()
  }

  return (
    <div className="grid gap-6">
      <PageTitle
        title="Tasks"
        actions={
          <Button asChild>
            <Link to={TASK_NEW_PATH}>タスクを作成</Link>
          </Button>
        }
      />
      {state?.notice ? (
        <p role="status" className="rounded-md border border-emerald-300 bg-emerald-50 px-4 py-2 text-sm text-emerald-900">
          {state.notice}
        </p>
      ) : null}

      <form role="search" aria-label="タスクの絞り込み" className="flex flex-wrap items-end gap-4" onSubmit={(e) => e.preventDefault()}>
        <div className="grid gap-2">
          <Label htmlFor="task-filter-status">状態</Label>
          <NativeSelect id="task-filter-status" className="w-48" value={filter.status} onChange={change('status')}>
            <option value="">すべて</option>
            {TASK_STATUS_FILTERS.map((status) => (
              <option key={status} value={status}>
                {taskStatusLabel(status)}
              </option>
            ))}
            {filter.status && !TASK_STATUS_FILTERS.includes(filter.status as never) ? (
              <option value={filter.status}>{taskStatusLabel(filter.status)}</option>
            ) : null}
          </NativeSelect>
        </div>
        <div className="grid gap-2">
          <Label htmlFor="task-filter-project">プロジェクト</Label>
          <NativeSelect id="task-filter-project" className="w-60" value={filter.projectId} onChange={change('projectId')}>
            <option value="">すべて</option>
            {(projects.data ?? []).map((project) => (
              <option key={project.id} value={project.id}>
                {project.name}
              </option>
            ))}
            {filter.projectId && !projectName.has(filter.projectId) ? <option value={filter.projectId}>{filter.projectId}</option> : null}
          </NativeSelect>
        </div>
        {filtered ? (
          <Button type="button" variant="outline" onClick={reset}>
            絞り込みを解除
          </Button>
        ) : null}
      </form>

      <ApiErrorAlert error={error} onReload={reload} />
      {list.data === undefined ? (
        list.error ? null : <Loading />
      ) : list.data.length === 0 ? (
        <Card>
          <CardContent className="text-muted-foreground text-sm">
            {filtered ? '条件に合うタスクはありません。' : 'タスクはありません。「タスクを作成」から追加してください。'}
          </CardContent>
        </Card>
      ) : (
        <div className="overflow-x-auto rounded-xl border" aria-busy={list.loading || undefined}>
          <table className="w-full text-sm">
            <caption className="sr-only">タスク</caption>
            <thead className="bg-muted/50 text-left">
              <tr>
                <th scope="col" className="px-4 py-2 font-medium whitespace-nowrap">
                  タスク
                </th>
                <th scope="col" className="px-4 py-2 font-medium whitespace-nowrap">
                  状態
                </th>
                <th scope="col" className="px-4 py-2 font-medium whitespace-nowrap">
                  プロジェクト
                </th>
                <th scope="col" className="px-4 py-2 font-medium whitespace-nowrap">
                  最新 Execution
                </th>
                <th scope="col" className="px-4 py-2 font-medium whitespace-nowrap">
                  PR
                </th>
              </tr>
            </thead>
            <tbody>
              {list.data.map((task) => (
                <tr key={task.id} className="border-t align-top">
                  <td className="px-4 py-3">
                    <div className="font-medium">{task.title}</div>
                    <div className="text-muted-foreground font-mono text-xs">
                      {task.id} · {task.shortName}
                    </div>
                  </td>
                  <td className="px-4 py-3">
                    <TaskStatusBadge status={task.status} />
                  </td>
                  <td className="px-4 py-3 whitespace-nowrap">
                    <Link to={`/projects/${encodeURIComponent(task.projectId)}`} className="underline-offset-4 hover:underline">
                      {projectName.get(task.projectId) ?? task.projectId}
                    </Link>
                  </td>
                  <td className="px-4 py-3">
                    <LatestExecution task={task} />
                  </td>
                  <td className="px-4 py-3">
                    {task.prUrl ? (
                      <a href={task.prUrl} target="_blank" rel="noreferrer" className="underline underline-offset-4">
                        PR を開く
                      </a>
                    ) : (
                      <span className="text-muted-foreground">なし</span>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}
