import { useState, type FormEvent, type ReactNode } from 'react'

import { registry } from '@/api/registry'
import { settingsApi, settingsSource, type LinearLabelSettings, type SettingsSource } from '@/api/settings'
import { useSession } from '@/auth/session'
import { ApiErrorAlert } from '@/components/api-error-alert'
import { TextField } from '@/components/form-field'
import { Loading, PageTitle } from '@/components/page'
import { RunnerTable } from '@/components/runner-table'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { useApiErrors } from '@/hooks/use-api-errors'
import { useResource } from '@/hooks/use-resource'

function Row({ label, children, testId }: { label: string; children: ReactNode; testId?: string }) {
  return (
    <div className="grid gap-1 py-2 sm:grid-cols-[14rem_1fr] sm:gap-4" data-testid={testId}>
      <dt className="text-muted-foreground text-sm">{label}</dt>
      <dd className="text-sm">{children}</dd>
    </div>
  )
}

/** GET /api/settings の応答に無いため表示できない項目（13 3.2 の Settings。応答は 15 5章の linear だけ）。 */
const UNAVAILABLE = ['全体の既定タイムアウト', '通知先', 'Global Context'] as const

// サーバーの details[].field と同じ名前（backend/internal/api/linear.go の patchSettings）。
const LINEAR_FIELDS = ['linear.triggerLabel', 'linear.routingLabelPrefix'] as const
type LinearField = (typeof LINEAR_FIELDS)[number]
const LINEAR_LABELS: Record<LinearField, string> = {
  'linear.triggerLabel': 'トリガーのラベル名',
  'linear.routingLabelPrefix': '実行先ラベルの接頭辞',
}

/** トリガーのラベル名と実行先ラベルの接頭辞（15-linear-intake 3.4・5章、Q48）の編集。 */
function LinearLabelsForm({ initial }: { initial: LinearLabelSettings }) {
  const { api } = useSession()
  const [values, setValues] = useState<Record<LinearField, string>>({
    'linear.triggerLabel': initial.triggerLabel,
    'linear.routingLabelPrefix': initial.routingLabelPrefix,
  })
  const [localErrors, setLocalErrors] = useState<Record<string, string[]>>({})
  const [submitting, setSubmitting] = useState(false)
  const [saved, setSaved] = useState(false)
  const { error, setError, clear, fieldErrors, unmatched } = useApiErrors(LINEAR_FIELDS)
  const errorsFor = (field: LinearField) => {
    const messages = [...(localErrors[field] ?? []), ...(fieldErrors[field] ?? [])]
    return messages.length > 0 ? messages : undefined
  }

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (submitting) return
    clear()
    setSaved(false)
    const errors: Record<string, string[]> = {}
    for (const field of LINEAR_FIELDS) if (values[field].trim() === '') errors[field] = ['入力してください']
    setLocalErrors(errors)
    if (Object.keys(errors).length > 0) return
    setSubmitting(true)
    try {
      // 値の前後の空白も Linear のラベル名の一部として扱われうるので、入力のまま送る。
      const response = await settingsApi.updateLinear(api, {
        triggerLabel: values['linear.triggerLabel'],
        routingLabelPrefix: values['linear.routingLabelPrefix'],
      })
      setValues({ 'linear.triggerLabel': response.linear.triggerLabel, 'linear.routingLabelPrefix': response.linear.routingLabelPrefix })
      setSaved(true)
    } catch (cause) {
      setError(cause)
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <form className="grid gap-4" noValidate onSubmit={handleSubmit} aria-label="Linear のラベルの設定">
      <ApiErrorAlert error={error} unmatchedDetails={unmatched} fieldLabels={LINEAR_LABELS} />
      <div className="grid gap-4 md:grid-cols-2">
        {LINEAR_FIELDS.map((field) => (
          <TextField
            key={field}
            name={field}
            label={LINEAR_LABELS[field]}
            value={values[field]}
            autoComplete="off"
            spellCheck={false}
            errors={errorsFor(field)}
            onChange={(event) => {
              setSaved(false)
              setValues((previous) => ({ ...previous, [field]: event.target.value }))
            }}
          />
        ))}
      </div>
      {saved ? (
        <p role="status" className="text-sm">
          保存しました。次の照会から使います。
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

/**
 * Settings（13 3.2）。同時実行の上限は表示だけで、変更する入力欄は置かない（13 4節）。
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

      <Card data-testid="settings-concurrency">
        <CardHeader>
          <CardTitle>
            <h2>同時実行の上限</h2>
          </CardTitle>
          <CardDescription>表示のみです。画面からは変更できません。値の出どころ: {source.origin}</CardDescription>
        </CardHeader>
        <CardContent>
          <ApiErrorAlert error={settings.error} onReload={settings.reload} />
          {settings.data === undefined ? (
            settings.error ? null : <Loading />
          ) : (
            <dl className="divide-y">
              <Row label="全体" testId="concurrency-global">
                <span className="font-semibold tabular-nums">{settings.data.concurrency.global}</span> 本
              </Row>
              <Row label="同一プロジェクト" testId="concurrency-per-project">
                <span className="font-semibold tabular-nums">{settings.data.concurrency.perProject}</span> 本
              </Row>
            </dl>
          )}
        </CardContent>
      </Card>

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

      <Card data-testid="settings-linear">
        <CardHeader>
          <CardTitle>
            <h2>Linear の取り込み</h2>
          </CardTitle>
          <CardDescription>
            トリガーのラベルが付いた Issue からタスクを作り、実行先ラベル（接頭辞で始まるラベル）でプロジェクトを決めます。evaloomd
            を再起動すると既定値（ai-ready・repo:）に戻ります。
          </CardDescription>
        </CardHeader>
        <CardContent>
          <ApiErrorAlert error={global.error} onReload={global.reload} />
          {global.data === undefined ? global.error ? null : <Loading /> : <LinearLabelsForm initial={global.data.linear} />}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>
            <h2>その他の全体設定</h2>
          </CardTitle>
          <CardDescription>全体設定の API（GET /api/settings）は Linear の項目だけを返すため、次の項目はまだ表示できません。</CardDescription>
        </CardHeader>
        <CardContent>
          <dl className="divide-y">
            {UNAVAILABLE.map((label) => (
              <Row key={label} label={label}>
                <span className="text-muted-foreground">取得できません</span>
              </Row>
            ))}
          </dl>
        </CardContent>
      </Card>
    </div>
  )
}
