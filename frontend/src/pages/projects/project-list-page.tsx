import { registry, type ProjectHarness } from '@/api/registry'
import { useSession } from '@/auth/session'
import { ApiErrorAlert } from '@/components/api-error-alert'
import { Loading, PageTitle } from '@/components/page'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { useResource } from '@/hooks/use-resource'
import { Link } from '@/lib/link'

/** 「役割: Harness 版」の一覧。割り当てが無ければその旨を出す。 */
export function AssignmentSummary({ harnesses }: { harnesses: readonly ProjectHarness[] }) {
  if (harnesses.length === 0) return <span className="text-muted-foreground">未割り当て</span>
  return (
    <ul className="grid gap-0.5">
      {harnesses.map((h) => (
        <li key={h.role}>
          <span className="font-medium">{h.role}</span>: {h.harnessId} <span className="font-mono">v{h.version}</span>
        </li>
      ))}
    </ul>
  )
}

/** Projects 一覧（13 3.2）。登録済みプロジェクトと、割り当て中の Harness と版。 */
export function ProjectListPage() {
  const { api } = useSession()
  const projects = useResource((signal) => registry.listProjects(api, signal), 'projects')
  return (
    <div className="grid gap-6">
      <PageTitle
        title="Projects"
        actions={
          <Button asChild>
            <Link to="/projects/new">プロジェクトを登録</Link>
          </Button>
        }
      />
      <ApiErrorAlert error={projects.error} onReload={projects.reload} />
      {projects.data === undefined ? (
        projects.error ? null : <Loading />
      ) : projects.data.length === 0 ? (
        <Card>
          <CardContent className="text-muted-foreground text-sm">
            登録済みのプロジェクトはありません。「プロジェクトを登録」から追加してください。
          </CardContent>
        </Card>
      ) : (
        <div className="overflow-x-auto rounded-xl border">
          <table className="w-full text-sm">
            <caption className="sr-only">登録済みプロジェクト</caption>
            <thead className="bg-muted/50 text-left">
              <tr>
                <th scope="col" className="px-4 py-2 font-medium whitespace-nowrap">
                  プロジェクト
                </th>
                <th scope="col" className="px-4 py-2 font-medium whitespace-nowrap">
                  ローカルリポジトリのパス
                </th>
                <th scope="col" className="px-4 py-2 font-medium whitespace-nowrap">
                  Runner
                </th>
                <th scope="col" className="px-4 py-2 font-medium whitespace-nowrap">
                  Base Branch
                </th>
                <th scope="col" className="px-4 py-2 font-medium whitespace-nowrap">
                  割り当て中の Harness と版
                </th>
              </tr>
            </thead>
            <tbody>
              {projects.data.map((project) => (
                <tr key={project.id} className="border-t align-top">
                  <td className="px-4 py-3 whitespace-nowrap">
                    <Link to={`/projects/${encodeURIComponent(project.id)}`} className="font-medium underline-offset-4 hover:underline">
                      {project.name}
                    </Link>
                    <div className="text-muted-foreground font-mono text-xs">{project.id}</div>
                  </td>
                  <td className="px-4 py-3 font-mono text-xs break-all">{project.localRepoPath}</td>
                  <td className="px-4 py-3 font-mono text-xs">{project.runnerId}</td>
                  <td className="px-4 py-3 font-mono text-xs whitespace-nowrap">{project.baseBranch}</td>
                  <td className="px-4 py-3">
                    <AssignmentSummary harnesses={project.harnesses ?? []} />
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
