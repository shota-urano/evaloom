import { useState, type FormEvent } from 'react'

import { conflictFieldErrors, preconditionFieldErrors, type FieldErrors } from '@/api/errors'
import { registry, type Project, type ProjectInput, type Runner } from '@/api/registry'
import { useSession } from '@/auth/session'
import { ApiErrorAlert } from '@/components/api-error-alert'
import { SelectField, TextField, TextAreaField } from '@/components/form-field'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { useApiErrors } from '@/hooks/use-api-errors'
import { useResource } from '@/hooks/use-resource'
import { Link } from '@/lib/link'
import { navigate } from '@/lib/router'
import { Loading, PageTitle } from '@/components/page'
import { ERROR_FIELD_LABELS, LINEAR_CONFLICT_FIELDS, PRECONDITION_FIELDS, PROJECT_LABELS as L } from './labels'

type FormValues = { id: string; name: string; runnerId: string; harnessId: string; profileId: string; params: string; approvalWaivers: string }
const FIELDS = ['id', 'name', 'runnerId', 'harnessId', 'profileId', 'params', 'approvalWaivers'] as const
const REQUIRED = ['name', 'runnerId', 'harnessId', 'profileId'] as const
const EMPTY: FormValues = { id: '', name: '', runnerId: '', harnessId: '', profileId: '', params: '{}', approvalWaivers: '' }
function toValues(project: Project): FormValues {
  return { id: project.id, name: project.name, runnerId: project.runnerId, harnessId: project.harnessId, profileId: project.profileId,
    params: JSON.stringify(project.params, null, 2), approvalWaivers: project.approvalWaivers.join('\n') }
}
function validate(values: FormValues, create: boolean): { input?: ProjectInput; errors: FieldErrors } {
  const errors: FieldErrors = {}
  for (const field of create ? ['id', ...REQUIRED] as const : REQUIRED) {
    if (values[field].trim() === '') errors[field] = ['入力してください']
  }
  let params: unknown
  try { params = JSON.parse(values.params) } catch { /* reported below */ }
  if (!params || typeof params !== 'object' || Array.isArray(params)) errors.params = ['JSON のオブジェクトを入力してください']
  if (Object.keys(errors).length) return { errors }
  return { errors, input: { name: values.name.trim(), runnerId: values.runnerId, harnessId: values.harnessId.trim(), profileId: values.profileId.trim(),
    params: params as Record<string, unknown>, approvalWaivers: values.approvalWaivers.split('\n').map((value) => value.trim()).filter(Boolean) } }
}

function runnerLabel(runner: Runner): string {
  const status = runner.status === 'online' ? 'オンライン' : 'オフライン'
  return runner.name === runner.id ? `${runner.id}（${status}）` : `${runner.name}（${runner.id}・${status}）`
}

type FormProps = {
  /** 編集時の元の値。無ければ新規登録。 */
  project?: Project
  runners: Runner[]
  onReload?: () => void
}

function ProjectForm({ project, runners, onReload }: FormProps) {
  const { api } = useSession()
  const create = project === undefined
  const [values, setValues] = useState<FormValues>(() => (project ? toValues(project) : EMPTY))
  const [localErrors, setLocalErrors] = useState<FieldErrors>({})
  const [submitting, setSubmitting] = useState(false)
  const { error, setError, clear, fieldErrors, unmatched } = useApiErrors(FIELDS)
  const preconditionErrors = preconditionFieldErrors(error, PRECONDITION_FIELDS)
  const conflictErrors = conflictFieldErrors(error, LINEAR_CONFLICT_FIELDS)
  // 409 に項目の details があるのは Linear Team ID と実行先ラベルの組の重複。読み直しても解消しないので、ボタンは出さない。
  const linearConflict = Object.keys(conflictErrors).length > 0

  const errorsFor = (field: string) => {
    const messages = [
      ...(localErrors[field] ?? []),
      ...(fieldErrors[field] ?? []),
      ...(preconditionErrors[field] ?? []),
      ...(conflictErrors[field] ?? []),
    ]
    return messages.length > 0 ? messages : undefined
  }

  const bind = (field: keyof FormValues) => ({
    name: field,
    value: values[field],
    errors: errorsFor(field),
    onChange: (event: { target: { value: string } }) => setValues((previous) => ({ ...previous, [field]: event.target.value })),
  })

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (submitting) return
    clear()
    const { input, errors } = validate(values, create)
    setLocalErrors(errors)
    if (!input) return
    setSubmitting(true)
    try {
      const id = create ? values.id.trim() : project.id
      const saved = create ? await registry.createProject(api, id, input) : await registry.updateProject(api, id, input)
      navigate(`/projects/${encodeURIComponent(saved.id)}`)
    } catch (cause) {
      setError(cause)
      setSubmitting(false)
    }
  }

  const reload =
    onReload &&
    (() => {
      clear()
      onReload()
    })

  return (
    <form className="grid gap-6" noValidate onSubmit={handleSubmit} aria-label={create ? 'プロジェクトの登録' : 'プロジェクトの編集'}>
      <ApiErrorAlert
        error={error}
        unmatchedDetails={unmatched}
        onReload={linearConflict ? undefined : reload}
        fieldLabels={ERROR_FIELD_LABELS}
        title={linearConflict ? 'Linear の実行先が重複しています' : undefined}
        description={
          linearConflict
            ? '同じ Linear Team ID と実行先ラベルの組は、1つのプロジェクトにしか登録できません。どちらかを変えてから保存してください。'
            : undefined
        }
      />

      <Card>
        <CardHeader>
          <CardTitle>基本情報</CardTitle>
        </CardHeader>
        <CardContent className="grid gap-4 md:grid-cols-2">
          <TextField
            label={L.id}
            autoComplete="off"
            spellCheck={false}
            disabled={!create}
            hint={create ? '英小文字・数字・ハイフン。登録後は変更できません。' : '登録後は変更できません。'}
            {...bind('id')}
          />
          <TextField label={L.name} autoComplete="off" {...bind('name')} />
          <SelectField
            label={L.runnerId}
            hint={runners.length === 0 ? 'Runner が登録されていません。evaloomd の runner register で登録してください。' : undefined}
            {...bind('runnerId')}
          >
            <option value="">選択してください</option>
            {runners.map((runner) => (
              <option key={runner.id} value={runner.id}>
                {runnerLabel(runner)}
              </option>
            ))}
          </SelectField>
          <TextField label={L.harnessId} autoComplete="off" {...bind('harnessId')} />
          <TextField label={L.profileId} autoComplete="off" {...bind('profileId')} />
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>スペシャルハーネスへの入力</CardTitle>
          <CardDescription>鍵やパスワードを渡す値に入れないでください。</CardDescription>
        </CardHeader>
        <CardContent className="grid gap-4">
          <TextAreaField label={L.params} rows={5} spellCheck={false} {...bind('params')} />
          <TextAreaField label={L.approvalWaivers} rows={3} hint="操作の名前を1行に1つ。Profile が省略を認めた操作だけを指定できます。" {...bind('approvalWaivers')} />
        </CardContent>
      </Card>

      <div className="flex gap-3">
        <Button type="submit" disabled={submitting}>
          {submitting ? '保存しています…' : create ? '登録' : '保存'}
        </Button>
        <Button asChild variant="outline">
          <Link to={create ? '/projects' : `/projects/${encodeURIComponent(project.id)}`}>キャンセル</Link>
        </Button>
      </div>
    </form>
  )
}

function useRunners() {
  const { api } = useSession()
  return useResource((signal) => registry.listRunners(api, signal), 'runners')
}

export function ProjectNewPage() {
  const runners = useRunners()
  return (
    <div className="grid gap-6">
      <PageTitle title="プロジェクトの登録" />
      {runners.error ? <ApiErrorAlert error={runners.error} /> : null}
      {runners.data ? <ProjectForm runners={runners.data} /> : <Loading />}
    </div>
  )
}

export function ProjectEditPage({ id }: { id: string }) {
  const { api } = useSession()
  const runners = useRunners()
  const project = useResource((signal) => registry.getProject(api, id, signal), `project:${id}`)
  const error = project.error ?? runners.error
  return (
    <div className="grid gap-6">
      <PageTitle title="プロジェクトの編集" subtitle={id} />
      {error ? <ApiErrorAlert error={error} /> : null}
      {project.data && runners.data && !project.loading ? (
        // 読み直したら入力欄を最新の値で作り直す。
        <ProjectForm key={project.data.updatedAt} project={project.data} runners={runners.data} onReload={project.reload} />
      ) : error ? null : (
        <Loading />
      )}
    </div>
  )
}
