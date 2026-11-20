import { useState, type FormEvent } from 'react'

import { preconditionFieldErrors, type FieldErrors } from '@/api/errors'
import { operationStatusLabel, registry, shortSha, waivableOperations, type Harness, type HarnessInput, type HarnessProfile } from '@/api/registry'
import { useSession } from '@/auth/session'
import { ApiErrorAlert } from '@/components/api-error-alert'
import { ConfirmDialog } from '@/components/confirm-dialog'
import { FieldError, SelectField, TextField } from '@/components/form-field'
import { Loading, PageTitle } from '@/components/page'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { useApiErrors } from '@/hooks/use-api-errors'
import { useResource } from '@/hooks/use-resource'
import { Link } from '@/lib/link'
import { navigate } from '@/lib/router'
import { HARNESS_LABELS as L, HARNESS_REPOSITORY_FIELDS, HARNESS_REPOSITORY_LABELS, harnessNotFound, harnessPath } from './labels'

type FormValues = { id: string; name: string; repoPath: string; repoUrl: string }
const FIELDS = ['id', 'name', 'repoPath', 'repoUrl', 'defaultProfileId', 'approvalWaivers'] as const
const REQUIRED = ['name', 'repoPath', 'repoUrl'] as const
const EMPTY: FormValues = { id: '', name: '', repoPath: '', repoUrl: '' }
function toValues(harness: Harness): FormValues {
  return { id: harness.id, name: harness.name, repoPath: harness.repoPath, repoUrl: harness.repoUrl }
}
function validate(values: FormValues, create: boolean): { input?: HarnessInput; errors: FieldErrors } {
  const errors: FieldErrors = {}
  for (const field of create ? ['id', ...REQUIRED] as const : REQUIRED) {
    if (!values[field].trim()) errors[field] = ['入力してください']
  }
  return Object.keys(errors).length ? { errors } : { errors, input: { name: values.name.trim(), repoPath: values.repoPath.trim(), repoUrl: values.repoUrl.trim() } }
}

const profileLabel = (p: HarnessProfile) => `${p.version}（${shortSha(p.commitSha)}・${operationStatusLabel(p.operationStatus)}）`

type FormProps = { harness?: Harness; profiles?: readonly HarnessProfile[]; onReload?: () => void }

function HarnessForm({ harness, profiles = [], onReload }: FormProps) {
  const { api } = useSession()
  const create = harness === undefined
  const [values, setValues] = useState<FormValues>(() => (harness ? toValues(harness) : EMPTY))
  const [localErrors, setLocalErrors] = useState<FieldErrors>({})
  const [submitting, setSubmitting] = useState(false)
  // 既定の版と承認を省く操作は編集でだけ扱う（登録時はまだ版が無い）。
  const [defaultProfileId, setDefaultProfileId] = useState<string>(() => harness?.defaultProfileId ?? '')
  const [waivers, setWaivers] = useState<string[]>(() => harness?.approvalWaivers ?? [])
  const [pendingWaiver, setPendingWaiver] = useState<string | null>(null)
  const profile = profiles.find((p) => p.id === defaultProfileId)
  const waivable = profile ? waivableOperations(profile.definition) : []
  // 既定の版で waivable でない操作（既定の版が無ければすべて）は保存できないので、外すよう促す（03 3.3）。
  const leftover = waivers.filter((operation) => !waivable.some((w) => w.operation === operation))
  const { error, setError, clear, fieldErrors, unmatched } = useApiErrors(FIELDS)
  const repositoryErrors = preconditionFieldErrors(error, HARNESS_REPOSITORY_FIELDS)

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

  function toggleWaiver(operation: string, checked: boolean) {
    if (checked) setPendingWaiver(operation)
    else setWaivers((previous) => previous.filter((w) => w !== operation))
  }

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (submitting) return
    clear()
    const { input, errors } = validate(values, create)
    if (!create && leftover.length > 0) errors.approvalWaivers = ['既定の版で承認を省けない操作を外してください']
    setLocalErrors(errors)
    if (!input || Object.keys(errors).length > 0) return
    setSubmitting(true)
    try {
      const id = create ? values.id.trim() : harness.id
      const saved = create
        ? await registry.createHarness(api, id, input)
        : await registry.updateHarness(api, id, { ...input, defaultProfileId: defaultProfileId || null, approvalWaivers: [...waivers] })
      navigate(harnessPath(saved.id))
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
    <form className="grid gap-6" noValidate onSubmit={handleSubmit} aria-label={create ? 'スペシャルハーネスの登録' : 'スペシャルハーネスの編集'}>
      <ApiErrorAlert error={error} unmatchedDetails={unmatched} onReload={reload} fieldLabels={HARNESS_REPOSITORY_LABELS} />

      <Card>
        <CardHeader>
          <CardTitle>スペシャルハーネス</CardTitle>
          <CardDescription>
            登録時に、パスが Git リポジトリのルートであること、origin がハーネスリポジトリ URL と一致することを確かめます。Profile
            の版は、登録後に詳細画面から登録します。
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
          <TextField label={L.repoPath} autoComplete="off" {...bind('repoPath')} />
          <TextField label={L.repoUrl} autoComplete="off" {...bind('repoUrl')} />

        </CardContent>
      </Card>

      {create ? null : (
        <Card>
          <CardHeader>
            <CardTitle>直接振り分けたタスクの設定</CardTitle>
            <CardDescription>
              振り分けの表でこのスペシャルハーネスへ直接振り分けたタスクは、既定の版で実行します。既定の版が無いと、直接振り分けたタスクは実行されません。
            </CardDescription>
          </CardHeader>
          <CardContent className="grid gap-4">
            <div className="md:max-w-md">
              <SelectField
                name="defaultProfileId"
                label="既定の版"
                value={defaultProfileId}
                errors={errorsFor('defaultProfileId')}
                onChange={(event) => setDefaultProfileId(event.target.value)}
                hint={profiles.length === 0 ? '版がまだ登録されていません。詳細画面から版を登録してください。' : undefined}
              >
                <option value="">未設定</option>
                {profiles.map((p) => (
                  <option key={p.id} value={p.id}>
                    {profileLabel(p)}
                  </option>
                ))}
              </SelectField>
            </div>
            <fieldset className="grid gap-2" data-field="approvalWaivers" aria-describedby="approval-waivers-error">
              <legend className="mb-1 text-sm font-medium">直接振り分けたタスクで承認を省く操作</legend>
              {!profile ? (
                <p className="text-muted-foreground text-sm">既定の版を選ぶと、その版で承認を省ける（waivable: true）操作が並びます。</p>
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
                  <p>既定の版では承認を省けない操作が残っています。外してから保存してください。</p>
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
      )}

      <div className="flex gap-3">
        <Button type="submit" disabled={submitting}>
          {submitting ? '確かめています…' : create ? '登録' : '保存'}
        </Button>
        <Button asChild variant="outline">
          <Link to={create ? '/harnesses' : harnessPath(harness.id)}>キャンセル</Link>
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
          <p>
            <span className="font-mono">{pendingWaiver}</span>{' '}
            を足すと、このスペシャルハーネスへ直接振り分けたタスクでは、この操作の承認を待たずに実行します。
          </p>
        </ConfirmDialog>
      ) : null}
    </form>
  )
}

export function HarnessNewPage() {
  return (
    <div className="grid gap-6">
      <PageTitle title="スペシャルハーネスの登録" />
      <HarnessForm />
    </div>
  )
}

export function HarnessEditPage({ id }: { id: string }) {
  const { api } = useSession()
  const harness = useResource((signal) => registry.getHarness(api, id, signal), `harness:${id}`)
  const profiles = useResource((signal) => registry.listHarnessProfiles(api, id, signal), `profiles:${id}`)
  const error = harness.error ?? (harness.data === undefined && !harness.loading ? harnessNotFound() : null) ?? profiles.error
  const reload = () => {
    harness.reload()
    profiles.reload()
  }
  return (
    <div className="grid gap-6">
      <PageTitle title="スペシャルハーネスの編集" subtitle={id} />
      {error ? <ApiErrorAlert error={error} /> : null}
      {harness.data && !harness.loading && profiles.data ? (
        // 読み直したら入力欄を最新の値で作り直す。
        <HarnessForm key={harness.data.updatedAt} harness={harness.data} profiles={profiles.data} onReload={reload} />
      ) : error ? null : (
        <Loading />
      )}
    </div>
  )
}
