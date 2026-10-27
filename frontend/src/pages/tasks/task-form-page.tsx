import { useRef, useState, type FormEvent } from 'react'

import { preconditionFieldErrors, type FieldErrors } from '@/api/errors'
import { registry } from '@/api/registry'
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
import { SHORT_NAME_PATTERN, TASK_LABELS as L, TASKS_PATH } from './labels'

type FormValues = Required<TaskInput>

// サーバーの details[].field と同じ名前（04 3.1、13 6節）。
const FIELDS = ['projectId', 'title', 'body', 'acceptanceCriteria', 'shortName'] as const
const REQUIRED = ['projectId', 'title', 'body', 'acceptanceCriteria'] as const
// 422 precondition_failed（Default Harness の割り当てが無い）の details[].field → 項目。
const PRECONDITION_FIELDS: Record<string, string> = { projectId: 'projectId' }

const EMPTY: FormValues = { projectId: '', title: '', body: '', acceptanceCriteria: '', shortName: '' }

/** 画面での検証（正本はサーバー、13 3.1）。受け入れ条件は必須で、空白だけも不可（04 3.1、13 4節）。 */
function validateTask(values: FormValues): { input?: TaskInput; errors: FieldErrors } {
  const errors: FieldErrors = {}
  for (const field of REQUIRED) {
    if (values[field].trim() === '') errors[field] = [field === 'projectId' ? '選択してください' : '入力してください']
  }
  const shortName = values.shortName.trim()
  if (shortName !== '' && !SHORT_NAME_PATTERN.test(shortName)) {
    errors.shortName = ['英小文字・数字・ハイフンの1〜40文字で入力してください']
  }
  if (Object.keys(errors).length > 0) return { errors }
  // 本文と受け入れ条件はサーバーが加工せずに保存する（backend/internal/api/tasks.md）ので、そのまま送る。
  const input: TaskInput = {
    projectId: values.projectId,
    title: values.title,
    body: values.body,
    acceptanceCriteria: values.acceptanceCriteria,
  }
  if (shortName !== '') input.shortName = shortName
  return { errors, input }
}

function TaskForm({ projects }: { projects: { id: string; name: string }[] }) {
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

  const errorsFor = (field: string) => {
    const messages = [...(localErrors[field] ?? []), ...(fieldErrors[field] ?? []), ...(preconditionErrors[field] ?? [])]
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
    const { input, errors } = validateTask(values)
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
          <CardDescription>
            作成すると、プロジェクトの既定の役割に割り当てた Harness で実行待ち（Queued）になります。受け入れ条件は必須です。
          </CardDescription>
        </CardHeader>
        <CardContent className="grid gap-4">
          <div className="grid gap-4 md:grid-cols-2">
            <SelectField label={L.projectId} {...bind('projectId')}>
              <option value="">選択してください</option>
              {projects.map((project) => (
                <option key={project.id} value={project.id}>
                  {project.name}
                </option>
              ))}
            </SelectField>
            <TextField
              label={L.shortName}
              placeholder="task"
              autoComplete="off"
              spellCheck={false}
              hint="任意。英小文字・数字・ハイフンの1〜40文字。ブランチ名に入ります。省略すると task。"
              {...bind('shortName')}
            />
          </div>
          <TextField label={L.title} autoComplete="off" {...bind('title')} />
          <TextAreaField label={L.body} rows={6} {...bind('body')} />
          <TextAreaField
            label={L.acceptanceCriteria}
            rows={4}
            required
            hint="必須。何ができたら完了かを書きます。"
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

/** タスク作成（13 3.2）。プロジェクト、件名、本文、受け入れ条件（必須）、短い名前。 */
export function TaskNewPage() {
  const { api } = useSession()
  const projects = useResource((signal) => registry.listProjects(api, signal), 'projects')
  return (
    <div className="grid gap-6">
      <PageTitle title="タスクの作成" />
      <ApiErrorAlert error={projects.error} onReload={projects.reload} />
      {projects.data === undefined ? (
        projects.error ? null : <Loading />
      ) : projects.data.length === 0 ? (
        <Card>
          <CardContent className="text-muted-foreground text-sm">
            プロジェクトが登録されていません。先に{' '}
            <Link to="/projects" className="underline underline-offset-4">
              Projects
            </Link>{' '}
            からプロジェクトを登録してください。
          </CardContent>
        </Card>
      ) : (
        <TaskForm projects={projects.data} />
      )}
    </div>
  )
}
