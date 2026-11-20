import { useState } from 'react'

import { decisions, type Decision } from '@/api/decisions'
import { registry } from '@/api/registry'
import { tasks, type ExecutionOutput, type TaskFilter, type TaskSummary } from '@/api/tasks'
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
import { DESTINATION_KIND_LABELS, decodeDestination, destinationPath, encodeDestination, taskDestination } from './destination'
import { useLiveTaskList } from './live'
import { decisionDetailPath } from '@/pages/decisions/labels'
import { heldCause, intakeBlockCauseLabel, routeDecisionOf } from './intake'
import { TASK_NEW_PATH, TASK_STATUS_FILTERS, taskDetailPath, taskStatusLabel } from './labels'
import { LinearIssueLink } from './linear-issue-link'

// 状態は色だけでなく文字でも示す（13 3.5）。色は補助。
const STATUS_TONES: Record<string, string> = {
  routing: 'border-violet-300 bg-violet-50 text-violet-900',
  queued: 'border-sky-300 bg-sky-50 text-sky-900',
  running: 'border-blue-300 bg-blue-50 text-blue-900',
  waiting_approval: 'border-amber-300 bg-amber-50 text-amber-900',
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

type ListFilter = Required<TaskFilter>
const NO_FILTER: ListFilter = { status: '', projectId: '', harnessId: '' }

/** 絞り込みは URL の ?status=&projectId=（または &harnessId=）にも残し、再読み込みしても保つ。振り分け先はどちらか一方だけ。 */
function readFilter(): ListFilter {
  const params = new URLSearchParams(window.location.search)
  const projectId = params.get('projectId') ?? ''
  // 02 5.1 に無い状態（廃止済みの verifying など）はサーバーが 400 にするので、絞り込みに使わない。
  const status = params.get('status') ?? ''
  const known = (TASK_STATUS_FILTERS as readonly string[]).includes(status)
  return { status: known ? status : '', projectId, harnessId: projectId ? '' : (params.get('harnessId') ?? '') }
}

function writeFilter(filter: ListFilter) {
  const params = new URLSearchParams()
  if (filter.status) params.set('status', filter.status)
  if (filter.projectId) params.set('projectId', filter.projectId)
  if (filter.harnessId) params.set('harnessId', filter.harnessId)
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

/** 振り分け先（Project かスペシャルハーネス）。種類も文字で出す。 */
function Destination({ task, names }: { task: TaskSummary; names: Record<'project' | 'harness', Map<string, string>> }) {
  const ref = taskDestination(task)
  if (!ref) return <span className="text-muted-foreground">なし</span>
  return (
    <div>
      <div className="text-muted-foreground text-xs">{DESTINATION_KIND_LABELS[ref.kind]}</div>
      <Link to={destinationPath(ref)} className="underline-offset-4 hover:underline">
        {names[ref.kind].get(ref.id) ?? ref.id}
      </Link>
    </div>
  )
}

/** 外に出したもの（最新の Execution の outputs、13 3.2）。label と url はサーバーの値をそのまま出す（13 3.1）。 */
function Outputs({ outputs }: { outputs: ExecutionOutput[] | undefined }) {
  if (!outputs || outputs.length === 0) return <span className="text-muted-foreground">なし</span>
  return (
    <ul className="grid gap-1" aria-label="外に出したもの">
      {outputs.map((output, index) => (
        <li key={`${index}:${output.url}`}>
          {/^https?:\/\//i.test(output.url) ? (
            <a href={output.url} target="_blank" rel="noopener noreferrer" className="underline underline-offset-4">
              {output.label || output.url}
            </a>
          ) : (
            // http(s) 以外の URL はリンクにしない（javascript: などを開かせない）。値はそのまま出す。
            <span>
              {output.label} <span className="text-muted-foreground font-mono text-xs">{output.url}</span>
            </span>
          )}
        </li>
      ))}
    </ul>
  )
}

/** 取り込みで保留しているタスクの理由（15-linear-intake 3.6）。状態は色でなく文字で示す。 */
function HeldReason({ task }: { task: TaskSummary }) {
  const cause = heldCause(task)
  if (!cause) return null
  return (
    <div className="mt-1 max-w-72 text-xs" data-testid="held-reason">
      保留の理由: {intakeBlockCauseLabel(cause)}
    </div>
  )
}

/** 振り分けを待つタスク（15-linear-intake 3.6.4）。開いている振り分けの判断待ちがあればリンクを出す。 */
function RoutingWait({ task, routeDecisions }: { task: TaskSummary; routeDecisions: readonly Decision[] | undefined }) {
  if (task.status !== 'routing') return null
  const decision = routeDecisionOf(routeDecisions, task.id)
  return (
    <div className="mt-1 max-w-72 text-xs" data-testid="routing-wait">
      振り分けを待っています
      {decision ? (
        <div>
          <Link to={decisionDetailPath(decision.id)} className="underline underline-offset-4">
            判断待ちを開く
          </Link>
        </div>
      ) : routeDecisions !== undefined ? (
        <div className="text-muted-foreground">振り分けの提案を作っています</div>
      ) : null}
    </div>
  )
}

/** Tasks 一覧（13 3.2）。タスク、状態、振り分け先、最新 Execution、外に出したもの、Linear の識別子。状態と振り分け先で絞り込む。 */
export function TaskListPage({ state }: { state?: NavigationState }) {
  const { api } = useSession()
  const [filter, setFilter] = useState(readFilter)
  const list = useResource((signal) => tasks.list(api, filter, signal), `tasks:${filter.status}:${filter.projectId}:${filter.harnessId}`)
  const projects = useResource((signal) => registry.listProjects(api, signal), 'projects')
  const harnesses = useResource((signal) => registry.listHarnesses(api, signal), 'harnesses')
  useLiveTaskList(api, list.data, list.reload)
  // 振り分けを待つタスクがあるときだけ、開いている振り分けの判断待ち（kind=route）を読んでリンクにする。
  const routingIds = (list.data ?? []).filter((task) => task.status === 'routing').map((task) => task.id)
  const routeDecisions = useResource(
    (signal) => (routingIds.length > 0 ? decisions.list(api, 'open', 'route', signal) : Promise.resolve([])),
    `route-decisions:${routingIds.join(',')}`,
  )
  const names = {
    project: new Map((projects.data ?? []).map((p) => [p.id, p.name])),
    harness: new Map((harnesses.data ?? []).map((h) => [h.id, h.name])),
  }
  const filtered = filter.status !== '' || filter.projectId !== '' || filter.harnessId !== ''
  const destinationValue = filter.projectId
    ? encodeDestination({ kind: 'project', id: filter.projectId })
    : filter.harnessId
      ? encodeDestination({ kind: 'harness', id: filter.harnessId })
      : ''

  const apply = (next: ListFilter) => {
    writeFilter(next)
    setFilter(next)
  }
  const changeStatus = (event: { target: { value: string } }) => apply({ ...filter, status: event.target.value })
  const changeDestination = (event: { target: { value: string } }) => {
    const ref = decodeDestination(event.target.value)
    apply({ ...filter, projectId: ref?.kind === 'project' ? ref.id : '', harnessId: ref?.kind === 'harness' ? ref.id : '' })
  }
  const reset = () => apply(NO_FILTER)
  const error = list.error ?? projects.error ?? harnesses.error
  const reload = () => {
    list.reload()
    projects.reload()
    harnesses.reload()
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
          <NativeSelect id="task-filter-status" className="w-48" value={filter.status} onChange={changeStatus}>
            <option value="">すべて</option>
            {TASK_STATUS_FILTERS.map((status) => (
              <option key={status} value={status}>
                {taskStatusLabel(status)}
              </option>
            ))}
          </NativeSelect>
        </div>
        <div className="grid gap-2">
          <Label htmlFor="task-filter-destination">振り分け先</Label>
          <NativeSelect id="task-filter-destination" className="w-60" value={destinationValue} onChange={changeDestination}>
            <option value="">すべて</option>
            <optgroup label="Project">
              {(projects.data ?? []).map((project) => (
                <option key={project.id} value={encodeDestination({ kind: 'project', id: project.id })}>
                  {project.name}
                </option>
              ))}
              {filter.projectId && !names.project.has(filter.projectId) ? <option value={destinationValue}>{filter.projectId}</option> : null}
            </optgroup>
            <optgroup label="スペシャルハーネス">
              {(harnesses.data ?? []).map((harness) => (
                <option key={harness.id} value={encodeDestination({ kind: 'harness', id: harness.id })}>
                  {harness.name}
                </option>
              ))}
              {filter.harnessId && !names.harness.has(filter.harnessId) ? <option value={destinationValue}>{filter.harnessId}</option> : null}
            </optgroup>
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
                  振り分け先
                </th>
                <th scope="col" className="px-4 py-2 font-medium whitespace-nowrap">
                  最新 Execution
                </th>
                <th scope="col" className="px-4 py-2 font-medium whitespace-nowrap">
                  外に出したもの
                </th>
              </tr>
            </thead>
            <tbody>
              {list.data.map((task) => (
                <tr key={task.id} className="border-t align-top">
                  <td className="px-4 py-3">
                    <Link to={taskDetailPath(task.id)} className="font-medium underline-offset-4 hover:underline">
                      {task.title}
                    </Link>
                    <div className="text-muted-foreground font-mono text-xs">
                      {task.id}
                      {task.shortName ? ` · ${task.shortName}` : null}
                    </div>
                    {task.source === 'linear' && task.linear ? (
                      <div className="text-xs">
                        <span className="text-muted-foreground">Linear </span>
                        <LinearIssueLink task={task} className="font-mono underline underline-offset-4" />
                      </div>
                    ) : null}
                  </td>
                  <td className="px-4 py-3">
                    <TaskStatusBadge status={task.status} />
                    <HeldReason task={task} />
                    <RoutingWait task={task} routeDecisions={routeDecisions.data} />
                  </td>
                  <td className="px-4 py-3 whitespace-nowrap">
                    <Destination task={task} names={names} />
                  </td>
                  <td className="px-4 py-3">
                    <LatestExecution task={task} />
                  </td>
                  <td className="px-4 py-3">
                    <Outputs outputs={task.latestExecution?.outputs} />
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
