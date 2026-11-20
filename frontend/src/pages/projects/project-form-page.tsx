import { useState, type FormEvent } from 'react'

import { ApiError } from '@/api/client'
import { describeError, formatDetail, preconditionFieldErrors, type FieldErrors } from '@/api/errors'
import {
  operationStatusLabel,
  registry,
  shortSha,
  waivableOperations,
  type Harness,
  type HarnessProfile,
  type Project,
  type ProjectInput,
  type Runner,
} from '@/api/registry'
import { settingsApi } from '@/api/settings'
import { useSession } from '@/auth/session'
import { ApiErrorAlert } from '@/components/api-error-alert'
import { ConfirmDialog } from '@/components/confirm-dialog'
import { FieldError, SelectField, TextAreaField, TextField } from '@/components/form-field'
import { Loading, PageTitle } from '@/components/page'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { useApiErrors } from '@/hooks/use-api-errors'
import { useResource } from '@/hooks/use-resource'
import { Link } from '@/lib/link'
import { navigate } from '@/lib/router'
import { ERROR_FIELD_LABELS, PRECONDITION_FIELDS, PROJECT_LABELS as L } from './labels'

// Project の登録・編集（13 3.5）。入力欄は6項目（名前〔登録時は Project ID も〕、振り分けのラベル、
// 頼むスペシャルハーネスと版、渡す値、承認を省く設定、動かすマシン）だけ。

export type FormValues = { id: string; name: string; routingLabel: string; harnessId: string; profileId: string; params: string; runnerId: string }
const FIELDS = ['id', 'name', 'routingLabel', 'harnessId', 'profileId', 'params', 'approvalWaivers', 'runnerId'] as const
const REQUIRED = ['name', 'harnessId', 'profileId', 'runnerId'] as const
const EMPTY: FormValues = { id: '', name: '', routingLabel: '', harnessId: '', profileId: '', params: '{}', runnerId: '' }

function toValues(project: Project): FormValues {
  return {
    id: project.id,
    name: project.name,
    routingLabel: project.routingLabel ?? '',
    harnessId: project.harnessId,
    profileId: project.profileId,
    params: JSON.stringify(project.params, null, 2),
    runnerId: project.runnerId,
  }
}

/** 画面での検証（正本はサーバー側、13 3.1）。ラベルは、まだ無いときだけ検証する。 */
export function validateProject(
  values: FormValues,
  waivers: readonly string[],
  options: { create: boolean; labelEditable: boolean; prefix: string | undefined },
): { input?: ProjectInput; errors: FieldErrors } {
  const errors: FieldErrors = {}
  for (const field of options.create ? (['id', ...REQUIRED] as const) : REQUIRED) {
    if (values[field].trim() === '') errors[field] = ['入力してください']
  }
  const label = values.routingLabel.trim()
  if (options.labelEditable) {
    if (options.create && label === '') errors.routingLabel = ['入力してください']
    else if (label !== '' && options.prefix && (!label.startsWith(options.prefix) || label === options.prefix)) {
      errors.routingLabel = [`「${options.prefix}」で始めてください`]
    }
  }
  let params: unknown
  try {
    params = JSON.parse(values.params)
  } catch {
    /* reported below */
  }
  if (!params || typeof params !== 'object' || Array.isArray(params)) errors.params = ['JSON のオブジェクトを入力してください']
  if (Object.keys(errors).length) return { errors }
  return {
    errors,
    input: {
      name: values.name.trim(),
      harnessId: values.harnessId,
      profileId: values.profileId,
      params: params as Record<string, unknown>,
      approvalWaivers: [...waivers],
      runnerId: values.runnerId,
    },
  }
}

function runnerLabel(runner: Runner): string {
  const status = runner.status === 'online' ? 'オンライン' : 'オフライン'
  return runner.name === runner.id ? `${runner.id}（${status}）` : `${runner.name}（${runner.id}・${status}）`
}

function profileLabel(profile: HarnessProfile): string {
  return `v${profile.version}（${shortSha(profile.commitSha)}・${operationStatusLabel(profile.operationStatus)}）`
}

/** ラベルの登録だけが失敗したときの理由（13 3.5）。 */
export function labelFailureText(error: unknown): string {
  if (error instanceof ApiError && error.status === 409) {
    return '同じラベルか、この Project を振り分け先にしたラベルがすでに登録されています。'
  }
  if (error instanceof ApiError && error.details.length > 0) {
    return error.details.map((detail) => formatDetail(detail, ERROR_FIELD_LABELS)).join('、')
  }
  const display = describeError(error)
  return `${display.title}。${display.description}`
}

type FormProps = {
  /** 編集時の元の値。無ければ新規登録。 */
  project?: Project
  runners: Runner[]
  harnesses: Harness[]
  /** linear.routingLabelPrefix。読めなかったときは undefined（サーバーの検証に任せる）。 */
  prefix: string | undefined
  onReload?: () => void
}

function ProjectForm({ project, runners, harnesses, prefix, onReload }: FormProps) {
  const { api } = useSession()
  // 登録後にラベルの登録だけが失敗したら、以後は登録済みの Project の更新として送る。
  const [saved, setSaved] = useState<Project | undefined>(project)
  const create = saved === undefined
  const labelEditable = !saved?.routingLabel
  const [values, setValues] = useState<FormValues>(() => (project ? toValues(project) : EMPTY))
  const [waivers, setWaivers] = useState<string[]>(() => project?.approvalWaivers ?? [])
  const [pendingWaiver, setPendingWaiver] = useState<string | null>(null)
  const [localErrors, setLocalErrors] = useState<FieldErrors>({})
  const [labelError, setLabelError] = useState<unknown>(null)
  const [submitting, setSubmitting] = useState(false)
  const { error, setError, clear, fieldErrors, unmatched } = useApiErrors(FIELDS)
  const preconditionErrors = preconditionFieldErrors(error, PRECONDITION_FIELDS)

  const profiles = useResource(
    (signal) => (values.harnessId ? registry.listHarnessProfiles(api, values.harnessId, signal) : Promise.resolve([])),
    `profiles:${values.harnessId}`,
  )
  const profile = profiles.data?.find((p) => p.id === values.profileId)
  const waivable = profile ? waivableOperations(profile.definition) : []
  // 選んだ版で waivable でない操作が残っていれば、外すよう促す（13 3.5、17 3.2）。
  const leftover = profile ? waivers.filter((operation) => !waivable.some((w) => w.operation === operation)) : []
  const schema = profile?.definition?.params?.schema ?? null
  const pendingDescription = waivable.find((w) => w.operation === pendingWaiver)?.description

  const errorsFor = (field: string) => {
    const messages = [...(localErrors[field] ?? []), ...(fieldErrors[field] ?? []), ...(preconditionErrors[field] ?? [])]
    if (field === 'routingLabel' && labelError) messages.push(labelFailureText(labelError))
    return messages.length > 0 ? messages : undefined
  }

  const bind = (field: keyof FormValues) => ({
    name: field,
    value: values[field],
    errors: errorsFor(field),
    onChange: (event: { target: { value: string } }) => setValues((previous) => ({ ...previous, [field]: event.target.value })),
  })

  function changeHarness(harnessId: string) {
    const harness = harnesses.find((h) => h.id === harnessId)
    setValues((previous) => ({ ...previous, harnessId, profileId: harness?.defaultProfileId ?? '' }))
  }

  function toggleWaiver(operation: string, checked: boolean) {
    if (checked) setPendingWaiver(operation)
    else setWaivers((previous) => previous.filter((w) => w !== operation))
  }

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (submitting) return
    clear()
    const { input, errors } = validateProject(values, waivers, { create, labelEditable, prefix })
    setLocalErrors(errors)
    if (!input) return
    setSubmitting(true)
    let current: Project
    try {
      current = create ? await registry.createProject(api, values.id.trim(), input) : await registry.updateProject(api, saved.id, input)
    } catch (cause) {
      setError(cause)
      setSubmitting(false)
      return
    }
    // 振り分けのラベルは Project の登録・更新に成功した後に登録する（13 3.5）。
    const label = values.routingLabel.trim()
    if (labelEditable && label !== '') {
      try {
        await registry.createProjectRoute(api, label, current.id)
      } catch (cause) {
        setSaved({ ...current, routingLabel: null })
        setLabelError(cause)
        setSubmitting(false)
        return
      }
    }
    navigate(`/projects/${encodeURIComponent(current.id)}`)
  }

  const reload =
    onReload &&
    (() => {
      clear()
      onReload()
    })

  return (
    <form className="grid gap-6" noValidate onSubmit={handleSubmit} aria-label={create ? 'プロジェクトの登録' : 'プロジェクトの編集'}>
      <ApiErrorAlert error={error} unmatchedDetails={unmatched} onReload={reload} fieldLabels={ERROR_FIELD_LABELS} />
      {labelError && saved ? (
        <Alert variant="destructive" data-testid="label-missing">
          <AlertTitle>Project「{saved.name}」は登録しましたが、振り分けのラベルがありません</AlertTitle>
          <AlertDescription>
            <p>ラベルの登録に失敗しました: {labelFailureText(labelError)}</p>
            <p>ラベルを直して、もう一度保存してください。</p>
          </AlertDescription>
        </Alert>
      ) : null}

      <Card>
        <CardHeader>
          <CardTitle>Project</CardTitle>
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
            label={L.routingLabel}
            autoComplete="off"
            spellCheck={false}
            disabled={!labelEditable}
            hint={
              labelEditable
                ? prefix
                  ? `「${prefix}」で始まる文字列。Project ごとに1つ。`
                  : 'Project ごとに1つ。'
                : '登録済みのラベルの変更は、振り分けの表で行います。'
            }
            {...bind('routingLabel')}
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
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>頼むスペシャルハーネス</CardTitle>
        </CardHeader>
        <CardContent className="grid gap-4">
          <div className="grid gap-4 md:grid-cols-2">
            <SelectField {...bind('harnessId')} label={L.harnessId} onChange={(event) => changeHarness(event.target.value)}>
              <option value="">選択してください</option>
              {harnesses.map((harness) => (
                <option key={harness.id} value={harness.id}>
                  {harness.name === harness.id ? harness.id : `${harness.name}（${harness.id}）`}
                </option>
              ))}
            </SelectField>
            <SelectField {...bind('profileId')} label={L.profileId} disabled={values.harnessId === ''}>
              <option value="">選択してください</option>
              {(profiles.data ?? []).map((p) => (
                <option key={p.id} value={p.id}>
                  {profileLabel(p)}
                </option>
              ))}
            </SelectField>
          </div>
          {profiles.error ? <ApiErrorAlert error={profiles.error} onReload={profiles.reload} /> : null}

          <div className="grid gap-4 md:grid-cols-[2fr_1fr]">
            <TextAreaField label={L.params} rows={6} spellCheck={false} {...bind('params')} />
            <div className="text-muted-foreground grid content-start gap-1 pt-6 text-sm" data-testid="params-schema">
              {profile ? (
                schema ? (
                  <p>
                    この版は <span className="font-mono">{schema}</span>（JSON Schema）で渡す値の形を確かめます。
                  </p>
                ) : (
                  <p>この版は params.schema を持たないので、渡す値は {'{}'} だけを受け付けます。</p>
                )
              ) : null}
              <p>鍵やパスワードを渡す値に入れないでください。</p>
            </div>
          </div>

          <fieldset className="grid gap-2" data-field="approvalWaivers" aria-describedby="approval-waivers-error">
            <legend className="mb-1 text-sm font-medium">{L.approvalWaivers}</legend>
            {!profile ? (
              <p className="text-muted-foreground text-sm">版を選ぶと、その版で承認を省ける操作が並びます。</p>
            ) : waivable.length === 0 ? (
              <p className="text-muted-foreground text-sm">この版には承認を省ける操作がありません。</p>
            ) : (
              <ul className="grid gap-2">
                {waivable.map((approval) => (
                  <li key={approval.operation}>
                    <label className="flex items-start gap-2 text-sm">
                      <input
                        type="checkbox"
                        className="mt-1"
                        name="approvalWaivers"
                        value={approval.operation}
                        checked={waivers.includes(approval.operation)}
                        onChange={(event) => toggleWaiver(approval.operation, event.target.checked)}
                      />
                      <span>
                        <span className="font-mono">{approval.operation}</span>
                        {approval.description ? <span className="text-muted-foreground"> {approval.description}</span> : null}
                      </span>
                    </label>
                  </li>
                ))}
              </ul>
            )}
            {leftover.length > 0 ? (
              <div className="grid gap-2 rounded-md border border-dashed p-3 text-sm" data-testid="leftover-waivers">
                <p>選んだ版では承認を省けない操作が残っています。外してから保存してください。</p>
                <ul className="grid gap-1">
                  {leftover.map((operation) => (
                    <li key={operation} className="flex items-center gap-2">
                      <span className="font-mono">{operation}</span>
                      <Button type="button" size="sm" variant="outline" onClick={() => toggleWaiver(operation, false)}>
                        外す
                      </Button>
                    </li>
                  ))}
                </ul>
              </div>
            ) : null}
            <FieldError id="approval-waivers-error" messages={errorsFor('approvalWaivers')} />
          </fieldset>
        </CardContent>
      </Card>

      <div className="flex gap-3">
        <Button type="submit" disabled={submitting}>
          {submitting ? '保存しています…' : create ? '登録' : '保存'}
        </Button>
        <Button asChild variant="outline">
          <Link to={create ? '/projects' : `/projects/${encodeURIComponent(saved.id)}`}>キャンセル</Link>
        </Button>
      </div>

      {pendingWaiver ? (
        <ConfirmDialog
          title="承認を省く操作を足しますか"
          confirmLabel="承認を省く"
          initialFocus="cancel"
          onCancel={() => setPendingWaiver(null)}
          onConfirm={() => {
            const operation = pendingWaiver
            setWaivers((previous) => (previous.includes(operation) ? previous : [...previous, operation]))
            setPendingWaiver(null)
          }}
        >
          {/* 省く操作の名前と Profile の description と、本人の承認なしに行うことを示す（17 3.2、Q165）。 */}
          <p>
            <span className="font-mono">{pendingWaiver}</span>
            {pendingDescription ? <span>（{pendingDescription}）</span> : null}
          </p>
          <p>この Project では、この操作を本人の承認なしにスペシャルハーネスが行います。</p>
        </ConfirmDialog>
      ) : null}
    </form>
  )
}

/** フォームが選択肢に使う一覧と、ラベルの接頭辞を読む。 */
function useFormData() {
  const { api } = useSession()
  const runners = useResource((signal) => registry.listRunners(api, signal), 'runners')
  const harnesses = useResource((signal) => registry.listHarnesses(api, signal), 'harnesses')
  // 接頭辞が読めなくても登録はできる（サーバーが検証する）。
  const settings = useResource((signal) => settingsApi.get(api, signal).catch(() => null), 'settings')
  const ready = runners.data !== undefined && harnesses.data !== undefined && settings.data !== undefined
  return {
    error: runners.error ?? harnesses.error,
    ready,
    runners: runners.data ?? [],
    harnesses: harnesses.data ?? [],
    prefix: settings.data?.linear.routingLabelPrefix,
  }
}

export function ProjectNewPage() {
  const data = useFormData()
  return (
    <div className="grid gap-6">
      <PageTitle title="プロジェクトの登録" />
      {data.error ? <ApiErrorAlert error={data.error} /> : null}
      {data.ready ? <ProjectForm runners={data.runners} harnesses={data.harnesses} prefix={data.prefix} /> : data.error ? null : <Loading />}
    </div>
  )
}

export function ProjectEditPage({ id }: { id: string }) {
  const { api } = useSession()
  const data = useFormData()
  const project = useResource((signal) => registry.getProject(api, id, signal), `project:${id}`)
  const error = project.error ?? data.error
  return (
    <div className="grid gap-6">
      <PageTitle title="プロジェクトの編集" subtitle={id} />
      {error ? <ApiErrorAlert error={error} /> : null}
      {project.data && data.ready && !project.loading ? (
        // 読み直したら入力欄を最新の値で作り直す。
        <ProjectForm
          key={project.data.updatedAt}
          project={project.data}
          runners={data.runners}
          harnesses={data.harnesses}
          prefix={data.prefix}
          onReload={project.reload}
        />
      ) : error ? null : (
        <Loading />
      )}
    </div>
  )
}
