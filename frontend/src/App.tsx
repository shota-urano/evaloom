import { SessionProvider, useSession } from '@/auth/session'
import { ApiErrorAlert } from '@/components/api-error-alert'
import { Button } from '@/components/ui/button'
import type { ApiClient } from '@/api/client'
import { cn } from '@/lib/utils'
import { Link } from '@/lib/link'
import { matchPath, useLocation } from '@/lib/router'
import { ApprovalDetailPage } from '@/pages/approvals/approval-detail-page'
import { ApprovalListPage } from '@/pages/approvals/approval-list-page'
import { APPROVALS_PATH } from '@/pages/approvals/labels'
import { DashboardPage } from '@/pages/dashboard/dashboard-page'
import { LoginPage } from '@/pages/login-page'
import { HarnessDetailPage } from '@/pages/harnesses/harness-detail-page'
import { HarnessEditPage, HarnessNewPage } from '@/pages/harnesses/harness-form-page'
import { HarnessListPage } from '@/pages/harnesses/harness-list-page'
import { HARNESS_NEW_PATH } from '@/pages/harnesses/labels'
import { ProjectDetailPage } from '@/pages/projects/project-detail-page'
import { ProjectEditPage, ProjectNewPage } from '@/pages/projects/project-form-page'
import { ProjectListPage } from '@/pages/projects/project-list-page'
import { PROJECT_NEW_PATH } from '@/pages/projects/labels'
import { SETTINGS_PATH } from '@/pages/settings/labels'
import { SettingsPage } from '@/pages/settings/settings-page'
import { TASK_NEW_PATH, TASKS_PATH } from '@/pages/tasks/labels'
import { TaskNewPage } from '@/pages/tasks/task-form-page'
import { TaskDetailPage } from '@/pages/tasks/task-detail-page'
import { TaskListPage } from '@/pages/tasks/task-list-page'

const NAV = [
  { to: '/', label: 'Dashboard' },
  { to: '/projects', label: 'Projects' },
  { to: '/harnesses', label: 'Harnesses' },
  { to: TASKS_PATH, label: 'Tasks' },
  { to: APPROVALS_PATH, label: 'Approvals' },
  { to: SETTINGS_PATH, label: 'Settings' },
]

function Routes({ path, state }: ReturnType<typeof useLocation>) {
  if (path === '/projects') return <ProjectListPage />
  if (path === PROJECT_NEW_PATH) return <ProjectNewPage />
  const edit = matchPath('/projects/:id/edit', path)
  if (edit) return <ProjectEditPage key={edit.id} id={edit.id} />
  const detail = matchPath('/projects/:id', path)
  if (detail) return <ProjectDetailPage key={detail.id} id={detail.id} state={state} />
  if (path === '/harnesses') return <HarnessListPage />
  if (path === HARNESS_NEW_PATH) return <HarnessNewPage />
  const harnessEdit = matchPath('/harnesses/:id/edit', path)
  if (harnessEdit) return <HarnessEditPage key={harnessEdit.id} id={harnessEdit.id} />
  const harness = matchPath('/harnesses/:id', path)
  if (harness) return <HarnessDetailPage key={harness.id} id={harness.id} />
  if (path === TASKS_PATH) return <TaskListPage state={state} />
  if (path === TASK_NEW_PATH) return <TaskNewPage />
  const task = matchPath('/tasks/:id', path)
  if (task) return <TaskDetailPage key={task.id} id={task.id} />
  if (path === APPROVALS_PATH) return <ApprovalListPage />
  const approval = matchPath('/approvals/:id', path)
  if (approval) return <ApprovalDetailPage key={approval.id} id={approval.id} />
  if (path === SETTINGS_PATH) return <SettingsPage />
  if (path === '/') return <DashboardPage />
  return (
    <div>
      <h1 className="text-lg font-semibold">ページが見つかりません</h1>
      <p className="text-muted-foreground mt-1 text-sm">
        <Link to="/" className="underline underline-offset-4">
          ホームへ戻る
        </Link>
      </p>
    </div>
  )
}

function AppShell() {
  const location = useLocation()
  const current = (to: string) => (to === '/' ? location.path === '/' : location.path === to || location.path.startsWith(`${to}/`))
  return (
    <div className="min-h-svh">
      <header className="border-b">
        <div className="mx-auto flex h-14 max-w-6xl items-center gap-6 px-6">
          <div className="flex items-baseline">
            <span className="font-semibold">Evaloom</span>
            <span className="text-muted-foreground ml-3 text-sm">管理画面</span>
          </div>
          <nav aria-label="画面" className="flex gap-1">
            {NAV.map((item) => (
              <Link
                key={item.to}
                to={item.to}
                aria-current={current(item.to) ? 'page' : undefined}
                className={cn(
                  'rounded-md px-3 py-1.5 text-sm',
                  current(item.to) ? 'bg-secondary font-medium' : 'text-muted-foreground hover:text-foreground',
                )}
              >
                {item.label}
              </Link>
            ))}
          </nav>
        </div>
      </header>
      <main className="mx-auto max-w-6xl p-6">
        <Routes {...location} />
      </main>
    </div>
  )
}

function SessionGate() {
  const { status, error, retry } = useSession()
  if (status === 'loading') {
    return (
      <p role="status" className="text-muted-foreground p-6 text-sm">
        読み込んでいます…
      </p>
    )
  }
  if (status === 'error') {
    return (
      <main className="mx-auto grid max-w-md gap-3 p-6">
        <ApiErrorAlert error={error} />
        <Button type="button" variant="outline" onClick={retry}>
          再試行
        </Button>
      </main>
    )
  }
  if (status === 'unauthenticated') return <LoginPage />
  return <AppShell />
}

export default function App({ api }: { api?: ApiClient }) {
  return (
    <SessionProvider api={api}>
      <SessionGate />
    </SessionProvider>
  )
}
