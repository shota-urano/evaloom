import { useState, type FormEvent, type ReactNode } from 'react'

import { ApiError } from '@/api/client'
import { registry, type Runner } from '@/api/registry'
import {
  settingsApi,
  settingsSource,
  STAGES,
  type GlobalSettings,
  type SettingsPatch,
  type SettingsSource,
  type SettingsView,
  type Stage,
} from '@/api/settings'
import { useSession } from '@/auth/session'
import { ApiErrorAlert } from '@/components/api-error-alert'
import { SelectField, TextField } from '@/components/form-field'
import { Loading, PageTitle } from '@/components/page'
import { RunnerTable } from '@/components/runner-table'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { useApiErrors } from '@/hooks/use-api-errors'
import { useResource, type Resource } from '@/hooks/use-resource'
import { Link } from '@/lib/link'
import { ROUTES_PATH } from '@/pages/routes/labels'

function Row({ label, children, testId }: { label: string; children: ReactNode; testId?: string }) {
  return (
    <div className="grid gap-1 py-2 sm:grid-cols-[14rem_1fr] sm:gap-4" data-testid={testId}>
      <dt className="text-muted-foreground text-sm">{label}</dt>
      <dd className="text-sm">{children}</dd>
    </div>
  )
}

// 入力欄の名前。サーバーの details[].field と同じ名前（backend/internal/api/linear.go の decodeSettingsPatch）。
type StageField = `limits.stageTimeoutMaxSec.${Stage}`
const STAGE_FIELDS: readonly StageField[] = STAGES.map((stage) => `limits.stageTimeoutMaxSec.${stage}` as const)
const FIELDS = [
  'linear.triggerLabel',
  'linear.routingLabelPrefix',
  'routing.proposer.microHarness',
  'routing.proposer.model',
  'routing.defaultRunnerId',
  ...STAGE_FIELDS,
  'approval.ttlSec',
] as const
type Field = (typeof FIELDS)[number]

const LABELS: Record<string, string> = {
  'linear.triggerLabel': '起動の合図のラベル（linear.triggerLabel）',
  'linear.routingLabelPrefix': '振り分けのラベルの接頭辞（linear.routingLabelPrefix）',
  'routing.proposer': '振り分けを提案する AI（routing.proposer）',
  'routing.proposer.microHarness': '提案に使うミクロハーネス（routing.proposer）',
  'routing.proposer.model': '提案に使うモデル（routing.proposer）',
  'routing.defaultRunnerId': '提案と直接振り分けたタスクを動かす Runner（routing.defaultRunnerId）',
  'limits.stageTimeoutMaxSec': '段の上限時間の上限（limits.stageTimeoutMaxSec）',
  ...Object.fromEntries(STAGES.map((stage) => [`limits.stageTimeoutMaxSec.${stage}`, `${stage}（秒）`])),
  'approval.ttlSec': '承認の有効時間（approval.ttlSec、秒）',
}

/** 02 4.2 のミクロハーネスの列挙値。 */
const MICRO_HARNESSES = [
  { value: 'claude_code', label: 'Claude Code CLI（claude_code）' },
  { value: 'codex', label: 'Codex CLI（codex）' },
]

type Values = Record<Field, string>

function toValues(view: SettingsView): Values {
  const values = {
    'linear.triggerLabel': view.linear.triggerLabel,
    'linear.routingLabelPrefix': view.linear.routingLabelPrefix,
    'routing.proposer.microHarness': view.routing.proposer.microHarness,
    'routing.proposer.model': view.routing.proposer.model ?? '',
    'routing.defaultRunnerId': view.routing.defaultRunnerId ?? '',
    'approval.ttlSec': String(view.approval.ttlSec),
  } as Values
  for (const stage of STAGES) values[`limits.stageTimeoutMaxSec.${stage}`] = String(view.limits.stageTimeoutMaxSec[stage])
  return values
}

/**
 * 422（15 Q160）: 変えた接頭辞で始まらない振り分けの表のラベル。details[].field は `routes.<ラベル>`、
 * reason は outside_routing_label_prefix（backend/internal/api/linear.go patchSettings）。
 */
function labelsOutsidePrefix(error: unknown): string[] {
  if (!(error instanceof ApiError) || error.status !== 422) return []
  return error.details
    .filter((detail) => detail.reason === 'outside_routing_label_prefix' && detail.field.startsWith('routes.'))
    .map((detail) => detail.field.slice('routes.'.length))
}

const isPositiveInteger = (text: string) => /^[1-9][0-9]*$/.test(text.trim())

/** 画面で確かめる（正本はサーバー、13 3.1）。空欄と正の整数でない値は送らない。 */
function localErrors(values: Values): Partial<Record<Field, string[]>> {
  const errors: Partial<Record<Field, string[]>> = {}
  for (const field of ['linear.triggerLabel', 'linear.routingLabelPrefix', 'routing.proposer.microHarness'] as const) {
    if (values[field].trim() === '') errors[field] = ['入力してください']
  }
  for (const field of [...STAGE_FIELDS, 'approval.ttlSec'] as const) {
    if (values[field].trim() === '') errors[field] = ['入力してください']
    else if (!isPositiveInteger(values[field])) errors[field] = ['1 以上の整数を入力してください']
  }
  return errors
}

/**
 * 変えたキーだけを PATCH の本文にする。routing.proposer と limits.stageTimeoutMaxSec は値全体の置き換えなので、
 * 一部を変えたときも全項目を送る。
 */
function changes(before: Values, after: Values): SettingsPatch {
  const patch: SettingsPatch = {}
  const changed = (field: Field) => before[field] !== after[field]
  if (changed('linear.triggerLabel') || changed('linear.routingLabelPrefix')) {
    patch.linear = {}
    // 値の前後の空白も Linear のラベル名の一部として扱われうるので、入力のまま送る。
    if (changed('linear.triggerLabel')) patch.linear.triggerLabel = after['linear.triggerLabel']
    if (changed('linear.routingLabelPrefix')) patch.linear.routingLabelPrefix = after['linear.routingLabelPrefix']
  }
  const proposerChanged = changed('routing.proposer.microHarness') || changed('routing.proposer.model')
  if (proposerChanged || changed('routing.defaultRunnerId')) {
    patch.routing = {}
    if (proposerChanged) {
      const model = after['routing.proposer.model'].trim()
      patch.routing.proposer = { microHarness: after['routing.proposer.microHarness'], model: model === '' ? null : model }
    }
    if (changed('routing.defaultRunnerId')) {
      patch.routing.defaultRunnerId = after['routing.defaultRunnerId'] === '' ? null : after['routing.defaultRunnerId']
    }
  }
  if (STAGE_FIELDS.some(changed)) {
    const limits = {} as Record<Stage, number>
    for (const stage of STAGES) limits[stage] = Number(after[`limits.stageTimeoutMaxSec.${stage}`].trim())
    patch.limits = { stageTimeoutMaxSec: limits }
  }
  if (changed('approval.ttlSec')) patch.approval = { ttlSec: Number(after['approval.ttlSec'].trim()) }
  return patch
}

/** 全体設定（02 3.8 の6キー）の表示と変更。 */
function GlobalSettingsForm({ initial, runners }: { initial: SettingsView; runners: Runner[] | undefined }) {
  const { api } = useSession()
  const [saved, setSaved] = useState<Values>(() => toValues(initial))
  const [values, setValues] = useState<Values>(saved)
  const [local, setLocal] = useState<Partial<Record<Field, string[]>>>({})
  const [submitting, setSubmitting] = useState(false)
  const [message, setMessage] = useState<string | null>(null)
  const { error, setError, clear, fieldErrors, unmatched } = useApiErrors(FIELDS)
  const outside = labelsOutsidePrefix(error)
  const errorsFor = (field: Field) => {
    const messages = [...(local[field] ?? []), ...(fieldErrors[field] ?? [])]
    if (field === 'linear.routingLabelPrefix' && outside.length > 0) {
      messages.push(`振り分けの表のラベル${outside.map((label) => `「${label}」`).join('、')}がこの接頭辞で始まりません`)
    }
    return messages.length > 0 ? messages : undefined
  }
  // 422 の details は routes.<ラベル> なので、一覧ではラベルを名前にして出す。
  const alertLabels: Record<string, string> = { ...LABELS }
  for (const label of outside) alertLabels[`routes.${label}`] = `振り分けの表のラベル「${label}」`
  const set = (field: Field) => (event: { target: { value: string } }) => {
    setMessage(null)
    setValues((previous) => ({ ...previous, [field]: event.target.value }))
  }

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (submitting) return
    clear()
    setMessage(null)
    const errors = localErrors(values)
    setLocal(errors)
    if (Object.keys(errors).length > 0) return
    const patch = changes(saved, values)
    if (Object.keys(patch).length === 0) {
      setMessage('変更はありません。')
      return
    }
    setSubmitting(true)
    try {
      const next = toValues(await settingsApi.update(api, patch))
      setSaved(next)
      setValues(next)
      setMessage('保存しました。')
    } catch (cause) {
      setError(cause)
    } finally {
      setSubmitting(false)
    }
  }

  // 保存されている Runner が一覧に無いとき（削除済みなど）も、その ID を選択肢に残して表示する。
  const runnerOptions = runners ?? []
  const currentRunner = values['routing.defaultRunnerId']
  const missingRunner = currentRunner !== '' && !runnerOptions.some((runner) => runner.id === currentRunner)
  const microHarness = values['routing.proposer.microHarness']

  const text = (field: Field, extra: { hint?: string; numeric?: boolean } = {}) => (
    <TextField
      key={field}
      name={field}
      label={LABELS[field]}
      hint={extra.hint}
      inputMode={extra.numeric ? 'numeric' : undefined}
      value={values[field]}
      autoComplete="off"
      spellCheck={false}
      errors={errorsFor(field)}
      onChange={set(field)}
    />
  )

  return (
    <form className="grid gap-6" noValidate onSubmit={handleSubmit} aria-label="全体設定">
      <ApiErrorAlert
        error={error}
        unmatchedDetails={unmatched}
        fieldLabels={alertLabels}
        description={outside.length > 0 ? '新しい接頭辞で始まらないラベルが振り分けの表に残るため、保存しませんでした。どの項目も変わっていません。' : undefined}
      />
      {outside.length > 0 ? (
        <p className="text-sm">
          <Link to={ROUTES_PATH} className="underline underline-offset-4">
            振り分けの表を開く
          </Link>
        </p>
      ) : null}

      <fieldset className="grid gap-4" data-testid="settings-linear">
        <legend className="mb-2 text-sm font-semibold">Linear の取り込み</legend>
        <div className="grid gap-4 md:grid-cols-2">
          {text('linear.triggerLabel')}
          {text('linear.routingLabelPrefix')}
        </div>
      </fieldset>

      <fieldset className="grid gap-4" data-testid="settings-routing">
        <legend className="mb-2 text-sm font-semibold">振り分けの提案</legend>
        <div className="grid gap-4 md:grid-cols-2">
          <SelectField
            name="routing.proposer.microHarness"
            label={LABELS['routing.proposer.microHarness']}
            value={microHarness}
            errors={errorsFor('routing.proposer.microHarness')}
            onChange={set('routing.proposer.microHarness')}
          >
            {MICRO_HARNESSES.map((option) => (
              <option key={option.value} value={option.value}>
                {option.label}
              </option>
            ))}
            {MICRO_HARNESSES.some((option) => option.value === microHarness) ? null : <option value={microHarness}>{microHarness}</option>}
          </SelectField>
          {text('routing.proposer.model', { hint: '空欄にすると提案を作らず、本人が判断待ちの画面で一覧から選びます。' })}
          <SelectField
            name="routing.defaultRunnerId"
            label={LABELS['routing.defaultRunnerId']}
            value={currentRunner}
            errors={errorsFor('routing.defaultRunnerId')}
            onChange={set('routing.defaultRunnerId')}
          >
            <option value="">未設定</option>
            {runnerOptions.map((runner) => (
              <option key={runner.id} value={runner.id}>
                {runner.name}（{runner.id}・{runner.status}）
              </option>
            ))}
            {missingRunner ? <option value={currentRunner}>{currentRunner}（登録されていない Runner）</option> : null}
          </SelectField>
        </div>
      </fieldset>

      <fieldset className="grid gap-4" data-testid="settings-limits">
        <legend className="mb-2 text-sm font-semibold">{LABELS['limits.stageTimeoutMaxSec']}</legend>
        <p className="text-muted-foreground text-xs">
          Profile が段ごとに宣言する上限時間（timeoutSec）は、この値を超えられません。4段をまとめて保存します。
        </p>
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">{STAGE_FIELDS.map((field) => text(field, { numeric: true }))}</div>
      </fieldset>

      <fieldset className="grid gap-4" data-testid="settings-approval">
        <legend className="mb-2 text-sm font-semibold">承認</legend>
        <div className="grid gap-4 md:grid-cols-2">
          {text('approval.ttlSec', { numeric: true, hint: '承認するときに有効時間を指定しなかった場合に使います。' })}
        </div>
      </fieldset>

      {message ? (
        <p role="status" className="text-sm">
          {message}
        </p>
      ) : null}
      <div>
        <Button type="submit" disabled={submitting}>
          {submitting ? '保存しています…' : '保存'}
        </Button>
      </div>
    </form>
  )
}

/** 同時実行の上限。表示だけで、変更する入力欄は置かない（13 4節）。Dashboard でも使う。 */
export function ConcurrencyLimitsCard({ origin, state }: { origin: string; state: Resource<GlobalSettings> }) {
  return (
    <Card data-testid="settings-concurrency">
      <CardHeader>
        <CardTitle>
          <h2>同時実行の上限</h2>
        </CardTitle>
        <CardDescription>表示のみです。画面からは変更できません。値の出どころ: {origin}</CardDescription>
      </CardHeader>
      <CardContent>
        <ApiErrorAlert error={state.error} onReload={state.reload} />
        {state.data === undefined ? (
          state.error ? null : <Loading />
        ) : (
          <dl className="divide-y">
            <Row label="全体" testId="concurrency-global">
              <span className="font-semibold tabular-nums">{state.data.concurrency.global}</span> 本
            </Row>
            <Row label="振り分け先ごと" testId="concurrency-per-project">
              <span className="font-semibold tabular-nums">{state.data.concurrency.perProject}</span> 本
            </Row>
          </dl>
        )}
      </CardContent>
    </Card>
  )
}

/**
 * Settings（13 3.2）。全体設定（02 3.8 のキー）の表示と変更、同時実行の上限（表示のみ、13 4節）、Runner 一覧。
 * source はテストで差し替えられるようにする。
 */
export function SettingsPage({ source = settingsSource }: { source?: SettingsSource }) {
  const { api } = useSession()
  const settings = useResource((signal) => source.load(signal), 'settings')
  const runners = useResource((signal) => registry.listRunners(api, signal), 'runners')
  const global = useResource((signal) => settingsApi.get(api, signal), 'global-settings')

  return (
    <div className="grid gap-6">
      <PageTitle title="Settings" />

      <Card data-testid="settings-global">
        <CardHeader>
          <CardTitle>
            <h2>全体設定</h2>
          </CardTitle>
          <CardDescription>変えた項目だけを保存します。1つでも受け付けられなかったときは、どの項目も変わりません。</CardDescription>
        </CardHeader>
        <CardContent>
          <ApiErrorAlert error={global.error} onReload={global.reload} />
          {global.data === undefined ? global.error ? null : <Loading /> : <GlobalSettingsForm initial={global.data} runners={runners.data} />}
        </CardContent>
      </Card>

      <ConcurrencyLimitsCard origin={source.origin} state={settings} />

      <Card>
        <CardHeader>
          <CardTitle>
            <h2>Runner 一覧</h2>
          </CardTitle>
        </CardHeader>
        <CardContent>
          <ApiErrorAlert error={runners.error} onReload={runners.reload} />
          {runners.data === undefined ? runners.error ? null : <Loading /> : <RunnerTable runners={runners.data} />}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>
            <h2>Global Context</h2>
          </CardTitle>
          <CardDescription>Global Context を読む本人用の API が無いため、まだ表示できません。</CardDescription>
        </CardHeader>
      </Card>
    </div>
  )
}
