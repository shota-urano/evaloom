import { useState, type FormEvent } from 'react'

import { conflictFieldErrors, preconditionFieldErrors, type FieldErrors } from '@/api/errors'
import { registry, type Project, type ProjectInput, type Runner } from '@/api/registry'
import { useSession } from '@/auth/session'
import { ApiErrorAlert } from '@/components/api-error-alert'
import { SelectField, TextField } from '@/components/form-field'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { useApiErrors } from '@/hooks/use-api-errors'
import { useResource } from '@/hooks/use-resource'
import { Link } from '@/lib/link'
import { navigate } from '@/lib/router'
import { Loading, PageTitle } from '@/components/page'
import { ERROR_FIELD_LABELS, LINEAR_CONFLICT_FIELDS, PRECONDITION_FIELDS, PROJECT_LABELS as L } from './labels'

type FormValues = {
  id: string
  name: string
  runnerId: string
  defaultRole: string
  linearTeamId: string
  linearRoutingLabel: string
}

// サーバーの details[].field と同じ名前。400 の details をこの名前の項目へ出す（13 6節）。
const FIELDS = [
  'id',
  'name',
  'runnerId',
  'defaultRole',
  'linearTeamId',
  'linearRoutingLabel',
] as const

const REQUIRED = ['name', 'runnerId', 'defaultRole'] as const

const EMPTY: FormValues = {
  id: '',
  name: '',
  runnerId: '',
  defaultRole: 'developer',
  linearTeamId: '',
  linearRoutingLabel: '',
}

function toValues(project: Project): FormValues {
  return {
    id: project.id,
    name: project.name,
    runnerId: project.runnerId,
    defaultRole: project.defaultRole,
    linearTeamId: project.linearTeamId ?? '',
    linearRoutingLabel: project.linearRoutingLabel ?? '',
  }
}

const optional = (value: string) => (value.trim() === '' ? null : value.trim())

/** 画面での検証（正本はサーバー、13 3.1）。通れば送信内容を返す。 */
function validate(values: FormValues, create: boolean): { input?: ProjectInput; errors: FieldErrors } {
  const errors: FieldErrors = {}
  const fields = create ? (['id', ...REQUIRED] as const) : REQUIRED
  for (const field of fields) {
    if (values[field].trim() === '') errors[field] = ['入力してください']
  }
  if (Object.keys(errors).length > 0) return { errors }
  return {
    errors,
    input: {
      name: values.name.trim(),
      runnerId: values.runnerId,
      defaultRole: values.defaultRole.trim(),
      // 空欄は null で送って解除する（省略しても null 扱いなので、編集時も毎回送る。backend/internal/api/projects.md）。
      linearTeamId: optional(values.linearTeamId),
      linearRoutingLabel: optional(values.linearRoutingLabel),
    },
  }
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
          <TextField
            label={L.defaultRole}
            autoComplete="off"
            spellCheck={false}
            hint="タスクの実行に使う役割。プロジェクト詳細で、この役割に Harness と版を割り当てます。"
            {...bind('defaultRole')}
          />
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Linear 連携</CardTitle>
          <CardDescription>
            両方を設定すると、この Team の Issue のうち、この実行先ラベルが付いたものをこのプロジェクトで実行します。どちらかが空欄なら
            Linear からは実行しません。
          </CardDescription>
        </CardHeader>
        <CardContent className="grid gap-4 md:grid-cols-2">
          <TextField label={L.linearTeamId} autoComplete="off" spellCheck={false} {...bind('linearTeamId')} />
          <TextField
            label={L.linearRoutingLabel}
            placeholder="repo:customer-system"
            autoComplete="off"
            spellCheck={false}
            {...bind('linearRoutingLabel')}
          />
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
