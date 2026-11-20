import { registry, type Project } from '@/api/registry'
import { useSession } from '@/auth/session'
import { ApiErrorAlert } from '@/components/api-error-alert'
import { Loading, PageTitle } from '@/components/page'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { useResource } from '@/hooks/use-resource'
import { Link } from '@/lib/link'
import { HARNESS_NEW_PATH, harnessPath, projectsUsing, versionOf } from './labels'

/** 一覧で使う、スペシャルハーネスを頼む Project の名前。 */
function UsageSummary({ projects }: { projects: readonly Project[] }) {
  if (projects.length === 0) return <span className="text-muted-foreground">なし</span>
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

/** スペシャルハーネス一覧（13 3.2）。ID と名前、既定の版、使っている Project。 */
export function HarnessListPage() {
  const { api } = useSession()
  const harnesses = useResource((signal) => registry.listHarnesses(api, signal), 'harnesses')
  const projects = useResource((signal) => registry.listProjects(api, signal), 'projects')
  const profiles = useResource((signal) => registry.listAllHarnessProfiles(api, signal), 'harness-profiles')
  const error = harnesses.error ?? projects.error ?? profiles.error
  const reload = () => {
    harnesses.reload()
    projects.reload()
    profiles.reload()
  }
  return (
    <div className="grid gap-6">
      <PageTitle
        title="スペシャルハーネス"
        actions={
          <Button asChild>
            <Link to={HARNESS_NEW_PATH}>スペシャルハーネスを登録</Link>
          </Button>
        }
      />
      <ApiErrorAlert error={error} onReload={reload} />
      {harnesses.data === undefined || projects.data === undefined || profiles.data === undefined ? (
        error ? null : <Loading />
      ) : harnesses.data.length === 0 ? (
        <Card>
          <CardContent className="text-muted-foreground text-sm">
            登録済みのスペシャルハーネスはありません。「スペシャルハーネスを登録」から追加してください。
          </CardContent>
        </Card>
      ) : (
        <div className="overflow-x-auto rounded-xl border">
          <table className="w-full text-sm">
            <caption className="sr-only">登録済みスペシャルハーネス</caption>
            <thead className="bg-muted/50 text-left">
              <tr>
                <th scope="col" className="px-4 py-2 font-medium whitespace-nowrap">
                  スペシャルハーネス
                </th>
                <th scope="col" className="px-4 py-2 font-medium whitespace-nowrap">
                  既定の版
                </th>
                <th scope="col" className="px-4 py-2 font-medium whitespace-nowrap">
                  ハーネスリポジトリのパス
                </th>
                <th scope="col" className="px-4 py-2 font-medium whitespace-nowrap">
                  使っている Project
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
                  <td className="px-4 py-3 font-mono text-xs">{versionOf(profiles.data ?? [], harness.defaultProfileId) ?? <span className="font-sans">未設定</span>}</td>
                  <td className="px-4 py-3 font-mono text-xs break-all">{harness.repoPath}</td>
                  <td className="px-4 py-3">
                    <UsageSummary projects={projectsUsing(projects.data ?? [], harness.id)} />
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
