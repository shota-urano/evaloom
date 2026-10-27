import { useState, type FormEvent, type ReactNode } from 'react'

import { preconditionFieldErrors } from '@/api/errors'
import { operationStatusLabel, registry, shortSha, TIMEOUT_KEYS, type Project } from '@/api/registry'
import { useSession } from '@/auth/session'
import { ApiErrorAlert } from '@/components/api-error-alert'
import { SelectField, TextField } from '@/components/form-field'
import { Loading, PageTitle } from '@/components/page'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { useApiErrors } from '@/hooks/use-api-errors'
import { useResource } from '@/hooks/use-resource'
import { Link } from '@/lib/link'
import type { NavigationState } from '@/lib/router'
import { PROJECT_LABELS as L, TIMEOUT_LABELS } from './labels'

function Row({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="grid gap-1 py-2 sm:grid-cols-[14rem_1fr] sm:gap-4">
      <dt className="text-muted-foreground text-sm">{label}</dt>
      <dd className="text-sm break-all">{children}</dd>
    </div>
  )
}

const unset = <span className="text-muted-foreground">未設定</span>
const mono = (value: string | null) => (value === null || value === '' ? unset : <span className="font-mono">{value}</span>)
const list = (values: readonly string[]) =>
  values.length === 0 ? (
    <span className="text-muted-foreground">なし</span>
  ) : (
    <ul className="grid gap-0.5 font-mono">
      {values.map((value) => (
        <li key={value}>{value}</li>
      ))}
    </ul>
  )
const json = (value: Record<string, unknown>) => <pre className="bg-muted/50 rounded-md p-2 font-mono text-xs">{JSON.stringify(value, null, 2)}</pre>

function SettingsCard({ project }: { project: Project }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>設定</CardTitle>
      </CardHeader>
      <CardContent>
        <dl className="divide-y">
          <Row label={L.localRepoPath}>{mono(project.localRepoPath)}</Row>
          <Row label={L.gitRemoteUrl}>{mono(project.gitRemoteUrl)}</Row>
          <Row label={L.runnerId}>{mono(project.runnerId)}</Row>
          <Row label={L.baseBranch}>{mono(project.baseBranch)}</Row>
          <Row label={L.defaultRole}>{mono(project.defaultRole)}</Row>
          <Row label={L.testCommand}>{mono(project.testCommand)}</Row>
          <Row label={L.testReportFormat}>{mono(project.testReportFormat)}</Row>
          <Row label={L.testReportPath}>{mono(project.testReportPath)}</Row>
          <Row label={L.setupCommand}>{mono(project.setupCommand)}</Row>
          <Row label={L.copyAllowlist}>{list(project.copyAllowlist)}</Row>
          <Row label={L.testFilePatterns}>{list(project.testFilePatterns)}</Row>
          {TIMEOUT_KEYS.map((key) => (
            <Row key={key} label={TIMEOUT_LABELS[key]}>
              {project.timeouts[key] === undefined ? <span className="text-muted-foreground">全体の既定値</span> : project.timeouts[key]}
            </Row>
          ))}
          <Row label={L.allowedOperations}>{json(project.allowedOperations)}</Row>
          <Row label={L.approvalPolicy}>{json(project.approvalPolicy)}</Row>
        </dl>
      </CardContent>
    </Card>
  )
}

const ASSIGN_FIELDS = ['role', 'harnessId', 'version'] as const
const ASSIGN_LABELS = { role: '役割', harnessId: 'Harness', version: '版' }

/** 役割に Harness と Profile の版を割り当てる（POST /api/projects/{id}/harnesses、03 3.6）。 */
function AssignForm({ project, onAssigned }: { project: Project; onAssigned: (message: string) => void }) {
  const { api } = useSession()
  const [role, setRole] = useState(project.defaultRole)
  const [harnessId, setHarnessId] = useState('')
  const [version, setVersion] = useState('')
  const [localErrors, setLocalErrors] = useState<Record<string, string[]>>({})
  const [submitting, setSubmitting] = useState(false)
  const { error, setError, clear, fieldErrors, unmatched } = useApiErrors(ASSIGN_FIELDS)
  const preconditionErrors = preconditionFieldErrors(error, { role: 'role', harnessId: 'harnessId', version: 'version' })
  const harnesses = useResource((signal) => registry.listHarnesses(api, signal), 'harnesses')
  const profiles = useResource(
    (signal) => (harnessId === '' ? Promise.resolve([]) : registry.listHarnessProfiles(api, harnessId, signal)),
    `profiles:${harnessId}`,
  )
  const errorsFor = (field: string) => {
    const messages = [...(localErrors[field] ?? []), ...(fieldErrors[field] ?? []), ...(preconditionErrors[field] ?? [])]
    return messages.length > 0 ? messages : undefined
  }

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (submitting) return
    clear()
    const errors: Record<string, string[]> = {}
    if (role.trim() === '') errors.role = ['入力してください']
    if (harnessId === '') errors.harnessId = ['選択してください']
    if (version === '') errors.version = ['選択してください']
    setLocalErrors(errors)
    if (Object.keys(errors).length > 0) return
    setSubmitting(true)
    try {
      const assignment = await registry.assignHarness(api, project.id, { role: role.trim(), harnessId, version })
      onAssigned(`${assignment.role} に ${assignment.harnessId} の版 ${version} を割り当てました。`)
    } catch (cause) {
      setError(cause)
    } finally {
      setSubmitting(false)
    }
  }

  const harnessList = harnesses.data ?? []
  const profileList = profiles.data ?? []
  return (
    <form className="grid gap-4" noValidate onSubmit={handleSubmit} aria-label="割り当ての追加・差し替え">
      <ApiErrorAlert error={error ?? harnesses.error ?? profiles.error} unmatchedDetails={unmatched} fieldLabels={ASSIGN_LABELS} />
      <div className="grid gap-4 md:grid-cols-3">
        <TextField
          name="role"
          label={ASSIGN_LABELS.role}
          value={role}
          autoComplete="off"
          spellCheck={false}
          onChange={(event) => setRole(event.target.value)}
          errors={errorsFor('role')}
          hint="同じ役割に割り当て済みなら版を差し替えます。"
        />
        <SelectField
          name="harnessId"
          label={ASSIGN_LABELS.harnessId}
          value={harnessId}
          onChange={(event) => {
            setHarnessId(event.target.value)
            setVersion('')
          }}
          errors={errorsFor('harnessId')}
          hint={harnesses.data && harnessList.length === 0 ? 'Harness が登録されていません。' : undefined}
        >
          <option value="">選択してください</option>
          {harnessList.map((harness) => (
            <option key={harness.id} value={harness.id}>
              {harness.name === harness.id ? harness.id : `${harness.name}（${harness.id}）`}
            </option>
          ))}
        </SelectField>
        <SelectField
          name="version"
          label={ASSIGN_LABELS.version}
          value={version}
          disabled={harnessId === '' || profiles.loading}
          onChange={(event) => setVersion(event.target.value)}
          errors={errorsFor('version')}
          hint={harnessId !== '' && !profiles.loading && profileList.length === 0 ? 'この Harness に登録済みの版はありません。' : undefined}
        >
          <option value="">選択してください</option>
          {profileList.map((profile) => (
            <option key={profile.id} value={profile.version}>
              {`${profile.version}（${shortSha(profile.commitSha)}・${operationStatusLabel(profile.operationStatus)}）`}
            </option>
          ))}
        </SelectField>
      </div>
      <div>
        <Button type="submit" disabled={submitting}>
          {submitting ? '割り当てています…' : '割り当てる'}
        </Button>
      </div>
    </form>
  )
}

function AssignmentsCard({ project, onChanged }: { project: Project; onChanged: () => void }) {
  const [message, setMessage] = useState<string | null>(null)
  const harnesses = project.harnesses ?? []
  return (
    <Card>
      <CardHeader>
        <CardTitle>Harness Profile の割り当て</CardTitle>
        <CardDescription>
          役割ごとに Harness と Profile の版を固定します。新しい版を登録しても、ここでの割り当ては自動では変わりません。
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-6">
        {harnesses.length === 0 ? (
          <p className="text-muted-foreground text-sm">まだ割り当てがありません。既定の役割（{project.defaultRole}）に割り当てるまで、タスクは実行できません。</p>
        ) : (
          <div className="overflow-x-auto rounded-lg border">
            <table className="w-full text-sm" aria-label="割り当て中の Harness Profile">
              <thead className="bg-muted/50 text-left">
                <tr>
                  <th scope="col" className="px-3 py-2 font-medium">
                    役割
                  </th>
                  <th scope="col" className="px-3 py-2 font-medium">
                    Harness
                  </th>
                  <th scope="col" className="px-3 py-2 font-medium">
                    版
                  </th>
                  <th scope="col" className="px-3 py-2 font-medium">
                    commit SHA
                  </th>
                  <th scope="col" className="px-3 py-2 font-medium">
                    割り当て日時
                  </th>
                </tr>
              </thead>
              <tbody>
                {harnesses.map((h) => (
                  <tr key={h.role} className="border-t">
                    <td className="px-3 py-2 font-medium">
                      {h.role}
                      {h.role === project.defaultRole ? <span className="text-muted-foreground ml-2 text-xs">既定</span> : null}
                    </td>
                    <td className="px-3 py-2 font-mono">{h.harnessId}</td>
                    <td className="px-3 py-2 font-mono">{h.version}</td>
                    <td className="px-3 py-2 font-mono" title={h.commitSha}>
                      {shortSha(h.commitSha)}
                    </td>
                    <td className="px-3 py-2">{new Date(h.assignedAt).toLocaleString('ja-JP')}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        {message ? (
          <p role="status" className="text-sm">
            {message}
          </p>
        ) : null}
        <AssignForm
          project={project}
          onAssigned={(text) => {
            setMessage(text)
            onChanged()
          }}
        />
      </CardContent>
    </Card>
  )
}

/** Project 詳細（13 3.2）。設定、役割ごとの Harness Profile 割り当て、Project Context の説明。 */
export function ProjectDetailPage({ id, state }: { id: string; state: NavigationState }) {
  const { api } = useSession()
  const project = useResource((signal) => registry.getProject(api, id, signal), `project:${id}`)
  const warnings = state?.warnings ?? []
  const data = project.data
  return (
    <div className="grid gap-6">
      <PageTitle
        title={data?.name ?? 'プロジェクト'}
        subtitle={id}
        actions={
          <>
            <Button asChild variant="outline">
              <Link to="/projects">一覧へ戻る</Link>
            </Button>
            {data ? (
              <Button asChild>
                <Link to={`/projects/${encodeURIComponent(id)}/edit`}>編集</Link>
              </Button>
            ) : null}
          </>
        }
      />
      {warnings.length > 0 ? (
        <Alert variant="warning">
          <AlertTitle>コピー許可リストに秘密情報を含むおそれのあるファイルがあります</AlertTitle>
          <AlertDescription>
            <p>許可リストには秘密情報を含まないファイルだけを載せ、値が必要ならテスト専用のダミー値を使ってください。</p>
            <ul className="list-disc pl-5 font-mono">
              {warnings.map((warning) => (
                <li key={warning}>{warning}</li>
              ))}
            </ul>
          </AlertDescription>
        </Alert>
      ) : null}
      <ApiErrorAlert error={project.error} onReload={project.reload} />
      {data ? (
        <>
          <AssignmentsCard project={data} onChanged={project.reload} />
          <SettingsCard project={data} />
          <Card>
            <CardHeader>
              <CardTitle>Project Context</CardTitle>
            </CardHeader>
            <CardContent className="grid gap-1 text-sm">
              <p>
                固定対象はリポジトリ内の <span className="font-mono">{data.contextLocation}</span> です。Phase 1 は{' '}
                <span className="font-mono">AGENTS.md</span> が正本のため、この画面からは編集できません。
              </p>
            </CardContent>
          </Card>
        </>
      ) : project.error ? null : (
        <Loading />
      )}
    </div>
  )
}
