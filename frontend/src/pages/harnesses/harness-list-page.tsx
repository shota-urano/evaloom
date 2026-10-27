import { registry } from '@/api/registry'
import { useSession } from '@/auth/session'
import { ApiErrorAlert } from '@/components/api-error-alert'
import { Loading, PageTitle } from '@/components/page'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { useResource } from '@/hooks/use-resource'
import { Link } from '@/lib/link'
import { HARNESS_NEW_PATH, harnessPath, usagesOf, type HarnessUsage } from './labels'

/** 一覧で使う、Harness を使っているプロジェクトの名前。同じ Project の複数の役割は1つにまとめる。 */
function UsageSummary({ usages }: { usages: readonly HarnessUsage[] }) {
  if (usages.length === 0) return <span className="text-muted-foreground">なし</span>
  const projects = [...new Map(usages.map((u) => [u.project.id, u.project])).values()]
  return (
    <ul className="grid gap-0.5">
      {projects.map((project) => (
        <li key={project.id}>
          <Link to={`/projects/${encodeURIComponent(project.id)}`} className="underline-offset-4 hover:underline">
            {project.name}
          </Link>
        </li>
      ))}
    </ul>
  )
}

/** Harnesses 一覧（13 3.2）。Harness、Adapter、使用中プロジェクト。 */
export function HarnessListPage() {
  const { api } = useSession()
  const harnesses = useResource((signal) => registry.listHarnesses(api, signal), 'harnesses')
  const projects = useResource((signal) => registry.listProjects(api, signal), 'projects')
  const error = harnesses.error ?? projects.error
  const reload = () => {
    harnesses.reload()
    projects.reload()
  }
  return (
    <div className="grid gap-6">
      <PageTitle
        title="Harnesses"
        actions={
          <Button asChild>
            <Link to={HARNESS_NEW_PATH}>Harness を登録</Link>
          </Button>
        }
      />
      <ApiErrorAlert error={error} onReload={reload} />
      {harnesses.data === undefined || projects.data === undefined ? (
        error ? null : <Loading />
      ) : harnesses.data.length === 0 ? (
        <Card>
          <CardContent className="text-muted-foreground text-sm">
            登録済みの Harness はありません。「Harness を登録」から追加してください。
          </CardContent>
        </Card>
      ) : (
        <div className="overflow-x-auto rounded-xl border">
          <table className="w-full text-sm">
            <caption className="sr-only">登録済み Harness</caption>
            <thead className="bg-muted/50 text-left">
              <tr>
                <th scope="col" className="px-4 py-2 font-medium whitespace-nowrap">
                  Harness
                </th>
                <th scope="col" className="px-4 py-2 font-medium whitespace-nowrap">
                  Adapter
                </th>
                <th scope="col" className="px-4 py-2 font-medium whitespace-nowrap">
                  ハーネスリポジトリのパス
                </th>
                <th scope="col" className="px-4 py-2 font-medium whitespace-nowrap">
                  使用中プロジェクト
                </th>
              </tr>
            </thead>
            <tbody>
              {harnesses.data.map((harness) => (
                <tr key={harness.id} className="border-t align-top">
                  <td className="px-4 py-3 whitespace-nowrap">
                    <Link to={harnessPath(harness.id)} className="font-medium underline-offset-4 hover:underline">
                      {harness.name}
                    </Link>
                    <div className="text-muted-foreground font-mono text-xs">{harness.id}</div>
                  </td>
                  <td className="px-4 py-3 font-mono text-xs">{harness.adapter}</td>
                  <td className="px-4 py-3 font-mono text-xs break-all">{harness.repoPath}</td>
                  <td className="px-4 py-3">
                    <UsageSummary usages={usagesOf(projects.data ?? [], harness.id)} />
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
