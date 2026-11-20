import { useRef, useState, type FormEvent } from 'react'

import { preconditionFieldErrors, type FieldErrors } from '@/api/errors'
import { registry, type Harness, type HarnessProfile, type Project } from '@/api/registry'
import { newIdempotencyKey, tasks, type TaskInput } from '@/api/tasks'
import { useSession } from '@/auth/session'
import { ApiErrorAlert } from '@/components/api-error-alert'
import { SelectField, TextAreaField, TextField } from '@/components/form-field'
import { Loading, PageTitle } from '@/components/page'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { useApiErrors } from '@/hooks/use-api-errors'
import { useResource } from '@/hooks/use-resource'
import { Link } from '@/lib/link'
import { navigate } from '@/lib/router'
import {
  acceptanceCriteriaRequired,
  decodeDestination,
  destinationInput,
  destinationProfileId,
  encodeDestination,
} from './destination'
import { TASK_LABELS as L, TASKS_PATH } from './labels'

type FormValues = { destination: string; title: string; body: string; acceptanceCriteria: string }

// サーバーの details[].field（04 3.1、backend/internal/api/tasks.md）。projectId・harnessId は「振り分け先」の欄に出す。
const FIELDS = ['projectId', 'harnessId', 'title', 'body', 'acceptanceCriteria'] as const
// 422 precondition_failed（未登録の振り分け先、既定の版の無いスペシャルハーネス）の details[].field → 項目。
const PRECONDITION_FIELDS: Record<string, string> = { projectId: 'destination', harnessId: 'destination' }

const EMPTY: FormValues = { destination: '', title: '', body: '', acceptanceCriteria: '' }

/** 画面での検証（正本はサーバー、13 3.1）。受け入れ条件は振り分け先の Profile が required のときだけ必須で、空白だけも不可（13 4節）。 */
function validateTask(values: FormValues, criteriaRequired: boolean): { input?: TaskInput; errors: FieldErrors } {
  const errors: FieldErrors = {}
  const destination = decodeDestination(values.destination)
  if (!destination) errors.destination = ['選択してください']
  if (values.title.trim() === '') errors.title = ['入力してください']
  if (values.body.trim() === '') errors.body = ['入力してください']
  if (criteriaRequired && values.acceptanceCriteria.trim() === '') errors.acceptanceCriteria = ['入力してください']
  if (!destination || Object.keys(errors).length > 0) return { errors }
  // 本文と受け入れ条件はサーバーが加工せずに保存する（backend/internal/api/tasks.md）ので、そのまま送る。
  const input: TaskInput = {
    ...destinationInput(destination),
    title: values.title,
    body: values.body,
    acceptanceCriteria: values.acceptanceCriteria,
  }
  return { errors, input }
}

type FormProps = { projects: Project[]; harnesses: Harness[]; profiles: HarnessProfile[] }

function TaskForm({ projects, harnesses, profiles }: FormProps) {
  const { api } = useSession()
  const [values, setValues] = useState<FormValues>(EMPTY)
  const [localErrors, setLocalErrors] = useState<FieldErrors>({})
  const [submitting, setSubmitting] = useState(false)
  const { error, setError, clear, fieldErrors, unmatched } = useApiErrors(FIELDS)
  const preconditionErrors = preconditionFieldErrors(error, PRECONDITION_FIELDS)
  // 二重送信の防止（13 3.1）:
  // - inFlight は描画を待たずに立つので、ボタンが無効になる前の2回目のクリックも送らない。
  // - Idempotency-Key は送信内容ごとに1つ作る。同じ内容の再送（通信失敗のあとなど）は同じキーを使うので、
  //   サーバーが最初のタスクを返し、2件目は作られない。内容を変えたら新しいキーにする。
  const inFlight = useRef(false)
  const attempt = useRef<{ key: string; payload: string } | null>(null)

  const selected = decodeDestination(values.destination)
  const criteriaRequired = selected ? acceptanceCriteriaRequired(destinationProfileId(selected, projects, harnesses), profiles) : false

  const errorsFor = (field: string) => {
    const server = field === 'destination' ? [...(fieldErrors.projectId ?? []), ...(fieldErrors.harnessId ?? [])] : (fieldErrors[field] ?? [])
    const messages = [...(localErrors[field] ?? []), ...server, ...(preconditionErrors[field] ?? [])]
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
    if (inFlight.current) return
    clear()
    const { input, errors } = validateTask(values, criteriaRequired)
    setLocalErrors(errors)
    if (!input) return
    const payload = JSON.stringify(input)
    if (attempt.current?.payload !== payload) attempt.current = { key: newIdempotencyKey(), payload }
    inFlight.current = true
    setSubmitting(true)
    try {
      const created = await tasks.create(api, input, attempt.current.key)
      navigate(TASKS_PATH, { notice: `タスク ${created.id}（${created.title}）を作成しました。` })
    } catch (cause) {
      setError(cause)
      inFlight.current = false
      setSubmitting(false)
    }
  }

  return (
    <form className="grid gap-6" noValidate onSubmit={handleSubmit} aria-label="タスクの作成">
      <ApiErrorAlert error={error} unmatchedDetails={unmatched} fieldLabels={L} />
      <Card>
        <CardHeader>
          <CardTitle>タスク</CardTitle>
          <CardDescription>振り分け先に Project かスペシャルハーネスのどちらか一方を選びます。作成すると実行待ち（Queued）になります。</CardDescription>
        </CardHeader>
        <CardContent className="grid gap-4">
          <SelectField
            label={L.destination}
            hint="スペシャルハーネスへ直接振り分けると、そのスペシャルハーネスの既定の版で実行します。"
            {...bind('destination')}
          >
            <option value="">選択してください</option>
            {projects.length > 0 ? (
              <optgroup label="Project">
                {projects.map((project) => (
                  <option key={project.id} value={encodeDestination({ kind: 'project', id: project.id })}>
                    {project.name}
                  </option>
                ))}
              </optgroup>
            ) : null}
            {harnesses.length > 0 ? (
              <optgroup label="スペシャルハーネス">
                {harnesses.map((harness) => (
                  <option key={harness.id} value={encodeDestination({ kind: 'harness', id: harness.id })} disabled={!harness.defaultProfileId}>
                    {harness.defaultProfileId ? harness.name : `${harness.name}（既定の版が無いため選べません）`}
                  </option>
                ))}
              </optgroup>
            ) : null}
          </SelectField>
          <TextField label={L.title} autoComplete="off" {...bind('title')} />
          <TextAreaField label={L.body} rows={6} {...bind('body')} />
          <TextAreaField
            label={L.acceptanceCriteria}
            rows={4}
            required={criteriaRequired}
            hint={
              selected === null
                ? '振り分け先の Profile が必須と宣言しているときは必須です。'
                : criteriaRequired
                  ? '必須（振り分け先の Profile が必須と宣言しています）。何ができたら完了かを書きます。'
                  : '任意（振り分け先の Profile は必須にしていません）。'
            }
            {...bind('acceptanceCriteria')}
          />
        </CardContent>
      </Card>
      <div className="flex gap-3">
        <Button type="submit" disabled={submitting}>
          {submitting ? '作成しています…' : '作成'}
        </Button>
        <Button asChild variant="outline">
          <Link to={TASKS_PATH}>キャンセル</Link>
        </Button>
      </div>
    </form>
  )
}

/** タスク作成（13 3.2）。振り分け先（Project かスペシャルハーネスの一方）、件名、本文、受け入れ条件。 */
export function TaskNewPage() {
  const { api } = useSession()
  const projects = useResource((signal) => registry.listProjects(api, signal), 'projects')
  const harnesses = useResource((signal) => registry.listHarnesses(api, signal), 'harnesses')
  const profiles = useResource((signal) => registry.listAllHarnessProfiles(api, signal), 'harness-profiles')
  const error = projects.error ?? harnesses.error ?? profiles.error
  const reload = () => {
    projects.reload()
    harnesses.reload()
    profiles.reload()
  }
  return (
    <div className="grid gap-6">
      <PageTitle title="タスクの作成" />
      <ApiErrorAlert error={error} onReload={reload} />
      {projects.data === undefined || harnesses.data === undefined || profiles.data === undefined ? (
        error ? null : <Loading />
      ) : projects.data.length === 0 && harnesses.data.length === 0 ? (
        <Card>
          <CardContent className="text-muted-foreground text-sm">
            振り分け先がありません。先に{' '}
            <Link to="/projects" className="underline underline-offset-4">
              Projects
            </Link>{' '}
            か{' '}
            <Link to="/harnesses" className="underline underline-offset-4">
              スペシャルハーネス
            </Link>{' '}
            を登録してください。
          </CardContent>
        </Card>
      ) : (
        <TaskForm projects={projects.data} harnesses={harnesses.data} profiles={profiles.data} />
      )}
    </div>
  )
}
