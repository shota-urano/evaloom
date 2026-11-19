import { registry } from '@/api/registry'
import { useSession } from '@/auth/session'
import { ApiErrorAlert } from '@/components/api-error-alert'
import { Loading, PageTitle } from '@/components/page'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { useResource } from '@/hooks/use-resource'
import { Link } from '@/lib/link'
import { PROJECT_LABELS as L } from './labels'

/** Project 詳細（13 3.2）。6項目の表示と、その Project のタスクの一覧へのリンク。 */
export function ProjectDetailPage({ id }: { id: string }) {
  const { api } = useSession()
  const project = useResource((signal) => registry.getProject(api, id, signal), `project:${id}`)
  const data = project.data
  const pinned = data?.harnesses?.find((h) => h.profileId === data.profileId)
  return (
    <div className="grid gap-6">
      <PageTitle
        title={data?.name ?? 'Project'}
        subtitle={id}
        actions={
          <>
            <Button asChild variant="outline">
              <Link to="/projects">一覧へ戻る</Link>
            </Button>
            {data ? (
              <Button asChild variant="outline">
                <Link to={`/tasks?projectId=${encodeURIComponent(id)}`}>この Project のタスク</Link>
              </Button>
            ) : null}
            {data ? (
              <Button asChild>
                <Link to={`/projects/${encodeURIComponent(id)}/edit`}>編集</Link>
              </Button>
            ) : null}
          </>
        }
      />
      <ApiErrorAlert error={project.error} onReload={project.reload} />
      {data ? (
        <Card>
          <CardHeader>
            <CardTitle>設定</CardTitle>
          </CardHeader>
          <CardContent>
            <dl className="grid gap-x-6 gap-y-3 text-sm md:grid-cols-[12rem_1fr]">
              <dt className="text-muted-foreground">{L.name}</dt>
              <dd>{data.name}</dd>
              <dt className="text-muted-foreground">{L.routingLabel}</dt>
              <dd className="font-mono" data-testid="routing-label">
                {data.routingLabel ?? <span className="text-muted-foreground font-sans">ラベルなし</span>}
              </dd>
              <dt className="text-muted-foreground">{L.harnessId}</dt>
              <dd>
                <span className="font-mono">{data.harnessId}</span>
                {pinned ? <span className="font-mono"> v{pinned.version}</span> : null}
                <div className="text-muted-foreground font-mono text-xs">{data.profileId}</div>
              </dd>
              <dt className="text-muted-foreground">渡す値</dt>
              <dd>
                <pre className="bg-muted overflow-auto rounded p-2">{JSON.stringify(data.params, null, 2)}</pre>
              </dd>
              <dt className="text-muted-foreground">{L.approvalWaivers}</dt>
              <dd className="font-mono">{data.approvalWaivers.join(', ') || <span className="font-sans">なし</span>}</dd>
              <dt className="text-muted-foreground">{L.runnerId}</dt>
              <dd className="font-mono">{data.runnerId}</dd>
            </dl>
          </CardContent>
        </Card>
      ) : project.error ? null : (
        <Loading />
      )}
    </div>
  )
}
