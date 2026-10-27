import { useState, type FormEvent } from 'react'

import { preconditionFieldErrors, type FieldErrors } from '@/api/errors'
import { registry, type Harness, type HarnessInput } from '@/api/registry'
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
import { ADAPTERS, HARNESS_LABELS as L, HARNESS_REPOSITORY_FIELDS, HARNESS_REPOSITORY_LABELS, harnessNotFound, harnessPath } from './labels'

type FormValues = {
  id: string
  name: string
  adapter: string
  repoPath: string
  repoUrl: string
  supportedModels: string
  availableTools: string
  toolPermissions: string
}

// サーバーの details[].field と同じ名前。400 の details をこの名前の項目へ出す（13 6節）。
const FIELDS = ['id', 'name', 'adapter', 'repoPath', 'repoUrl', 'supportedModels', 'availableTools', 'toolPermissions'] as const

const REQUIRED = ['name', 'adapter', 'repoPath', 'repoUrl'] as const

const EMPTY: FormValues = {
  id: '',
  name: '',
  adapter: '',
  repoPath: '',
  repoUrl: '',
  supportedModels: '',
  availableTools: '',
  toolPermissions: '{}',
}

function toValues(harness: Harness): FormValues {
  return {
    id: harness.id,
    name: harness.name,
    adapter: harness.adapter,
    repoPath: harness.repoPath,
    repoUrl: harness.repoUrl,
    supportedModels: harness.supportedModels.join('\n'),
    availableTools: harness.availableTools.join('\n'),
    toolPermissions: JSON.stringify(harness.toolPermissions, null, 2),
  }
}

const lines = (value: string) =>
  value
    .split('\n')
    .map((line) => line.trim())
    .filter((line) => line !== '')

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
function validate(values: FormValues, create: boolean): { input?: HarnessInput; errors: FieldErrors } {
  const errors: FieldErrors = {}
  for (const field of create ? (['id', ...REQUIRED] as const) : REQUIRED) {
    if (values[field].trim() === '') errors[field] = [field === 'adapter' ? '選択してください' : '入力してください']
  }
  const toolPermissions = parseObject(values.toolPermissions)
  if (!toolPermissions) errors.toolPermissions = ['JSON のオブジェクト（{ … }）で入力してください']
  if (Object.keys(errors).length > 0 || !toolPermissions) return { errors }
  return {
    errors,
    input: {
      name: values.name.trim(),
      adapter: values.adapter,
      repoPath: values.repoPath.trim(),
      repoUrl: values.repoUrl.trim(),
      supportedModels: lines(values.supportedModels),
      availableTools: lines(values.availableTools),
      toolPermissions,
    },
  }
}

function HarnessForm({ harness, onReload }: { harness?: Harness; onReload?: () => void }) {
  const { api } = useSession()
  const create = harness === undefined
  const [values, setValues] = useState<FormValues>(() => (harness ? toValues(harness) : EMPTY))
  const [localErrors, setLocalErrors] = useState<FieldErrors>({})
  const [submitting, setSubmitting] = useState(false)
  const { error, setError, clear, fieldErrors, unmatched } = useApiErrors(FIELDS)
  const repositoryErrors = preconditionFieldErrors(error, HARNESS_REPOSITORY_FIELDS)
  // 拡張として登録された Adapter（03 3.4）で保存済みなら、その値も選べるようにする。
  const adapters =
    harness && !ADAPTERS.some((a) => a.value === harness.adapter) ? [...ADAPTERS, { value: harness.adapter, label: harness.adapter }] : ADAPTERS

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
      const id = create ? values.id.trim() : harness.id
      const saved = create ? await registry.createHarness(api, id, input) : await registry.updateHarness(api, id, input)
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
    <form className="grid gap-6" noValidate onSubmit={handleSubmit} aria-label={create ? 'Harness の登録' : 'Harness の編集'}>
      <ApiErrorAlert error={error} unmatchedDetails={unmatched} onReload={reload} fieldLabels={HARNESS_REPOSITORY_LABELS} />

      <Card>
        <CardHeader>
          <CardTitle>Harness</CardTitle>
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
          <TextField
            name="specialty"
            label={L.specialty}
            value={harness?.specialty ?? 'developer'}
            disabled
            readOnly
            hint="Phase 1 は developer のみです。"
          />
          <SelectField label={L.adapter} hint="Profile の定義ファイルの executionConfiguration.adapter と一致させます。" {...bind('adapter')}>
            <option value="">選択してください</option>
            {adapters.map((adapter) => (
              <option key={adapter.value} value={adapter.value}>
                {adapter.label}
              </option>
            ))}
          </SelectField>
          <TextField
            label={L.repoPath}
            placeholder="/Users/you/workspace/harness"
            autoComplete="off"
            spellCheck={false}
            hint="Control Plane のマシン上の絶対パス。"
            {...bind('repoPath')}
          />
          <TextField
            label={L.repoUrl}
            placeholder="git@github.com:you/harness.git"
            autoComplete="off"
            spellCheck={false}
            hint="git remote get-url origin と同じ値。"
            {...bind('repoUrl')}
          />
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>モデルとツール</CardTitle>
          <CardDescription>Tool Permissions は本人だけがこの画面から変更できます。Runner とハーネスからは変更できません。</CardDescription>
        </CardHeader>
        <CardContent className="grid gap-4 md:grid-cols-2">
          <TextAreaField label={L.supportedModels} rows={3} spellCheck={false} hint="1行に1つ。" {...bind('supportedModels')} />
          <TextAreaField label={L.availableTools} rows={3} spellCheck={false} hint="1行に1つ。" {...bind('availableTools')} />
          <TextAreaField
            label={L.toolPermissions}
            rows={4}
            spellCheck={false}
            className="font-mono"
            hint="JSON のオブジェクト。"
            {...bind('toolPermissions')}
          />
        </CardContent>
      </Card>

      <div className="flex gap-3">
        <Button type="submit" disabled={submitting}>
          {submitting ? '確かめています…' : create ? '登録' : '保存'}
        </Button>
        <Button asChild variant="outline">
          <Link to={create ? '/harnesses' : harnessPath(harness.id)}>キャンセル</Link>
        </Button>
      </div>
    </form>
  )
}

export function HarnessNewPage() {
  return (
    <div className="grid gap-6">
      <PageTitle title="Harness の登録" />
      <HarnessForm />
    </div>
  )
}

export function HarnessEditPage({ id }: { id: string }) {
  const { api } = useSession()
  const harness = useResource((signal) => registry.getHarness(api, id, signal), `harness:${id}`)
  const error = harness.error ?? (harness.data === undefined && !harness.loading ? harnessNotFound() : null)
  return (
    <div className="grid gap-6">
      <PageTitle title="Harness の編集" subtitle={id} />
      {error ? <ApiErrorAlert error={error} /> : null}
      {harness.data && !harness.loading ? (
        // 読み直したら入力欄を最新の値で作り直す。
        <HarnessForm key={harness.data.updatedAt} harness={harness.data} onReload={harness.reload} />
      ) : error ? null : (
        <Loading />
      )}
    </div>
  )
}
