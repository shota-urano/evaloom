import { useState, type FormEvent } from 'react'

import { preconditionFieldErrors, type FieldErrors } from '@/api/errors'
import { registry, TIMEOUT_KEYS, type Project, type ProjectInput, type Runner, type TimeoutKey } from '@/api/registry'
import { useSession } from '@/auth/session'
import { ApiErrorAlert } from '@/components/api-error-alert'
import { SelectField, TextAreaField, TextField } from '@/components/form-field'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { useApiErrors } from '@/hooks/use-api-errors'
import { useResource } from '@/hooks/use-resource'
import { Link } from '@/lib/link'
import { navigate } from '@/lib/router'
import { Loading, PageTitle } from '@/components/page'
import { PROJECT_LABELS as L, REPOSITORY_CHECK_FIELDS, REPOSITORY_CHECK_LABELS, TIMEOUT_LABELS } from './labels'

type FormValues = {
  id: string
  name: string
  localRepoPath: string
  gitRemoteUrl: string
  runnerId: string
  baseBranch: string
  defaultRole: string
  testCommand: string
  testReportFormat: string
  testReportPath: string
  setupCommand: string
  copyAllowlist: string
  allowedOperations: string
  approvalPolicy: string
} & Record<`timeouts.${TimeoutKey}`, string>

// サーバーの details[].field と同じ名前。400 の details をこの名前の項目へ出す（13 6節）。
const FIELDS = [
  'id',
  'name',
  'localRepoPath',
  'gitRemoteUrl',
  'runnerId',
  'baseBranch',
  'defaultRole',
  'testCommand',
  'testReportFormat',
  'testReportPath',
  'setupCommand',
  'copyAllowlist',
  'allowedOperations',
  'approvalPolicy',
  'timeouts',
  ...TIMEOUT_KEYS.map((key) => `timeouts.${key}` as const),
] as const

const REQUIRED = ['name', 'localRepoPath', 'gitRemoteUrl', 'runnerId', 'baseBranch', 'defaultRole'] as const

const EMPTY: FormValues = {
  id: '',
  name: '',
  localRepoPath: '',
  gitRemoteUrl: '',
  runnerId: '',
  baseBranch: 'main',
  defaultRole: 'developer',
  testCommand: '',
  testReportFormat: '',
  testReportPath: '',
  setupCommand: '',
  copyAllowlist: '',
  allowedOperations: '{}',
  approvalPolicy: '{}',
  'timeouts.setupSec': '',
  'timeouts.harnessSec': '',
  'timeouts.testSec': '',
}

function toValues(project: Project): FormValues {
  const timeout = (key: TimeoutKey) => (project.timeouts[key] === undefined ? '' : String(project.timeouts[key]))
  return {
    id: project.id,
    name: project.name,
    localRepoPath: project.localRepoPath,
    gitRemoteUrl: project.gitRemoteUrl,
    runnerId: project.runnerId,
    baseBranch: project.baseBranch,
    defaultRole: project.defaultRole,
    testCommand: project.testCommand ?? '',
    testReportFormat: project.testReportFormat ?? '',
    testReportPath: project.testReportPath ?? '',
    setupCommand: project.setupCommand ?? '',
    copyAllowlist: project.copyAllowlist.join('\n'),
    allowedOperations: JSON.stringify(project.allowedOperations, null, 2),
    approvalPolicy: JSON.stringify(project.approvalPolicy, null, 2),
    'timeouts.setupSec': timeout('setupSec'),
    'timeouts.harnessSec': timeout('harnessSec'),
    'timeouts.testSec': timeout('testSec'),
  }
}

const lines = (value: string) =>
  value
    .split('\n')
    .map((line) => line.trim())
    .filter((line) => line !== '')
const optional = (value: string) => (value.trim() === '' ? null : value.trim())

function parseObject(value: string): Record<string, unknown> | undefined {
  try {
    const parsed: unknown = JSON.parse(value.trim() === '' ? '{}' : value)
    if (typeof parsed === 'object' && parsed !== null && !Array.isArray(parsed)) return parsed as Record<string, unknown>
  } catch {
    // 下で項目のエラーにする。
  }
  return undefined
}

/** 画面での検証（正本はサーバー、13 3.1）。通れば送信内容を返す。 */
function validate(values: FormValues, create: boolean): { input?: ProjectInput; errors: FieldErrors } {
  const errors: FieldErrors = {}
  const fields = create ? (['id', ...REQUIRED] as const) : REQUIRED
  for (const field of fields) {
    if (values[field].trim() === '') errors[field] = ['入力してください']
  }
  const allowedOperations = parseObject(values.allowedOperations)
  const approvalPolicy = parseObject(values.approvalPolicy)
  if (!allowedOperations) errors.allowedOperations = ['JSON のオブジェクト（{ … }）で入力してください']
  if (!approvalPolicy) errors.approvalPolicy = ['JSON のオブジェクト（{ … }）で入力してください']
  const timeouts: ProjectInput['timeouts'] = {}
  for (const key of TIMEOUT_KEYS) {
    const raw = values[`timeouts.${key}`].trim()
    if (raw === '') continue
    if (!/^\d+$/.test(raw) || Number(raw) <= 0) {
      errors[`timeouts.${key}`] = ['1 以上の整数で入力してください']
    } else {
      timeouts[key] = Number(raw)
    }
  }
  if (Object.keys(errors).length > 0 || !allowedOperations || !approvalPolicy) return { errors }
  return {
    errors,
    input: {
      name: values.name.trim(),
      localRepoPath: values.localRepoPath.trim(),
      gitRemoteUrl: values.gitRemoteUrl.trim(),
      runnerId: values.runnerId,
      baseBranch: values.baseBranch.trim(),
      defaultRole: values.defaultRole.trim(),
      testCommand: optional(values.testCommand),
      testReportFormat: optional(values.testReportFormat),
      testReportPath: optional(values.testReportPath),
      setupCommand: optional(values.setupCommand),
      copyAllowlist: lines(values.copyAllowlist),
      allowedOperations,
      approvalPolicy,
      timeouts,
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
  const repositoryErrors = preconditionFieldErrors(error, REPOSITORY_CHECK_FIELDS)

  const errorsFor = (field: string) => {
    const messages = [...(localErrors[field] ?? []), ...(fieldErrors[field] ?? []), ...(repositoryErrors[field] ?? [])]
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
      navigate(`/projects/${encodeURIComponent(saved.id)}`, { warnings: saved.warnings })
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
      <ApiErrorAlert error={error} unmatchedDetails={unmatched} onReload={reload} fieldLabels={REPOSITORY_CHECK_LABELS} />

      <Card>
        <CardHeader>
          <CardTitle>リポジトリ</CardTitle>
          <CardDescription>
            登録時に、パスが Git リポジトリのルートであること、origin が Git リポジトリ URL と一致すること、Base Branch が
            remote にあることを確かめます。
          </CardDescription>
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
          <TextField
            label={L.localRepoPath}
            placeholder="/Users/you/workspace/app"
            autoComplete="off"
            spellCheck={false}
            hint="Runner のマシン上の絶対パス。"
            {...bind('localRepoPath')}
          />
          <TextField
            label={L.gitRemoteUrl}
            placeholder="git@github.com:you/app.git"
            autoComplete="off"
            spellCheck={false}
            hint="git remote get-url origin と同じ値。"
            {...bind('gitRemoteUrl')}
          />
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
          <TextField label={L.baseBranch} autoComplete="off" spellCheck={false} {...bind('baseBranch')} />
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
          <CardTitle>テストとセットアップ</CardTitle>
          <CardDescription>テストコマンドの正本はここです。ハーネスからは変更できません。</CardDescription>
        </CardHeader>
        <CardContent className="grid gap-4 md:grid-cols-2">
          <TextField label={L.testCommand} placeholder="make verify" autoComplete="off" spellCheck={false} {...bind('testCommand')} />
          <TextField label={L.setupCommand} placeholder="npm ci" autoComplete="off" spellCheck={false} {...bind('setupCommand')} />
          <TextField
            label={L.testReportFormat}
            placeholder="junit_xml"
            autoComplete="off"
            spellCheck={false}
            {...bind('testReportFormat')}
          />
          <TextField
            label={L.testReportPath}
            placeholder="test-results/junit.xml"
            autoComplete="off"
            spellCheck={false}
            hint="worktree 内のパス。"
            {...bind('testReportPath')}
          />
          <TextAreaField
            label={L.copyAllowlist}
            rows={3}
            spellCheck={false}
            hint="worktree へコピーしてよいファイルを、リポジトリルートからの相対パスで1行に1つ。.. ・絶対パス・ワイルドカードは使えません。秘密情報は載せず、値が必要ならテスト専用のダミー値を使ってください。"
            {...bind('copyAllowlist')}
          />
          {TIMEOUT_KEYS.map((key) => (
            <TextField
              key={key}
              label={TIMEOUT_LABELS[key]}
              inputMode="numeric"
              autoComplete="off"
              hint="空欄なら全体の既定値を使います。"
              {...bind(`timeouts.${key}`)}
            />
          ))}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>権限設定</CardTitle>
          <CardDescription>
            Phase 1 は保存と表示のみです。承認が必要な操作は、この設定にかかわらず Runner が実行しません。
          </CardDescription>
        </CardHeader>
        <CardContent className="grid gap-4 md:grid-cols-2">
          <TextAreaField label={L.allowedOperations} rows={4} spellCheck={false} className="font-mono" {...bind('allowedOperations')} />
          <TextAreaField label={L.approvalPolicy} rows={4} spellCheck={false} className="font-mono" {...bind('approvalPolicy')} />
        </CardContent>
      </Card>

      <div className="flex gap-3">
        <Button type="submit" disabled={submitting}>
          {submitting ? '確かめています…' : create ? '登録' : '保存'}
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
