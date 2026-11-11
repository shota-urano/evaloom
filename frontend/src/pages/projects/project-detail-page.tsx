import { registry } from '@/api/registry'
import { useSession } from '@/auth/session'
import { ApiErrorAlert } from '@/components/api-error-alert'
import { Loading, PageTitle } from '@/components/page'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { useResource } from '@/hooks/use-resource'
import { Link } from '@/lib/link'

export function ProjectDetailPage({ id }: { id: string }) {
  const { api } = useSession()
  const project = useResource((signal) => registry.getProject(api, id, signal), `project:${id}`)
  const data = project.data
  return <div className="grid gap-6">
    <PageTitle title={data?.name ?? 'Project'} subtitle={id} actions={<>
      <Button asChild variant="outline"><Link to="/projects">一覧へ戻る</Link></Button>
      {data ? <Button asChild><Link to={`/projects/${encodeURIComponent(id)}/edit`}>編集</Link></Button> : null}
    </>} />
    <ApiErrorAlert error={project.error} onReload={project.reload} />
    {data ? <Card><CardHeader><CardTitle>設定</CardTitle></CardHeader><CardContent>
      <dl className="grid gap-2 text-sm">
        <dt>Runner</dt><dd className="font-mono">{data.runnerId}</dd>
        <dt>Harness ID</dt><dd className="font-mono">{data.harnessId}</dd>
        <dt>Profile ID</dt><dd className="font-mono">{data.profileId}</dd>
        <dt>渡す値</dt><dd><pre className="overflow-auto rounded bg-muted p-2">{JSON.stringify(data.params, null, 2)}</pre></dd>
        <dt>承認を省く操作</dt><dd>{data.approvalWaivers.join(', ') || 'なし'}</dd>
      </dl>
    </CardContent></Card> : project.error ? null : <Loading />}
  </div>
}
