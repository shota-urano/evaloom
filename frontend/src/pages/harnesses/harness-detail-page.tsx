import { useState, type FormEvent, type ReactNode } from 'react'

import { ApiError } from '@/api/client'
import { preconditionFieldErrors } from '@/api/errors'
import {
  operationStatusLabel,
  registry,
  shortSha,
  type Harness,
  type HarnessProfile,
  type ProfileProgramStage,
  type ProfileWorkStage,
  type Project,
  type Route,
} from '@/api/registry'
import { useSession } from '@/auth/session'
import { ApiErrorAlert } from '@/components/api-error-alert'
import { TextField } from '@/components/form-field'
import { Loading, PageTitle } from '@/components/page'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { useApiErrors } from '@/hooks/use-api-errors'
import { useResource } from '@/hooks/use-resource'
import { Link } from '@/lib/link'
import {
  acceptanceCriteriaLabel,
  directRoutes,
  HARNESS_LABELS as L,
  harnessNotFound,
  harnessPath,
  PROFILE_CHECK_LABELS,
  projectsUsing,
  secretAccessLabel,
  versionOf,
} from './labels'

// backend/internal/registry/profile.go の profileVersionPattern と同じ SemVer（3要素必須、prerelease・build 可）。
const SEMVER =
  /^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$/

function Row({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="grid gap-1 py-2 sm:grid-cols-[14rem_1fr] sm:gap-4">
      <dt className="text-muted-foreground text-sm">{label}</dt>
      <dd className="text-sm break-all">{children}</dd>
    </div>
  )
}

const mono = (value: string) => <span className="font-mono">{value}</span>

function SettingsCard({ harness, profiles }: { harness: Harness; profiles: readonly HarnessProfile[] }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>設定</CardTitle>
      </CardHeader>
      <CardContent>
        <dl className="divide-y">
          <Row label={L.repoPath}>{mono(harness.repoPath)}</Row>
          <Row label={L.repoUrl}>{mono(harness.repoUrl)}</Row>
          <Row label="既定の版">{versionOf(profiles, harness.defaultProfileId) ? mono(versionOf(profiles, harness.defaultProfileId) ?? '') : '未設定（直接振り分けたタスクは実行されません）'}</Row>
          <Row label="直接振り分けたタスクで承認を省く操作">
            {harness.approvalWaivers.length === 0 ? 'なし' : mono(harness.approvalWaivers.join(', '))}
          </Row>
        </dl>
      </CardContent>
    </Card>
  )
}

type RegisterProps = {
  harness: Harness
  profiles: readonly HarnessProfile[]
  onRegistered: () => void
}

/** Profile の版の登録（POST /api/harness-profiles、03 3.5）。 */
function RegisterProfileForm({ harness, profiles, onRegistered }: RegisterProps) {
  const { api } = useSession()
  const [version, setVersion] = useState('')
  const [localError, setLocalError] = useState<string | null>(null)
  const [submitting, setSubmitting] = useState(false)
  const [message, setMessage] = useState<string | null>(null)
  // 409・422 の内容は送った版について出すので、送信時の版を覚えておく。
  const [sent, setSent] = useState('')
  const { error, setError, clear, fieldErrors, unmatched } = useApiErrors(['version'])
  const preconditionErrors = preconditionFieldErrors(error, { version: 'version' })
  const errors = [...(localError ? [localError] : []), ...(fieldErrors.version ?? []), ...(preconditionErrors.version ?? [])]

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (submitting) return
    clear()
    setMessage(null)
    const value = version.trim()
    const invalid = value === '' ? '入力してください' : SEMVER.test(value) ? null : 'SemVer（例 1.2.0）で入力してください'
    setLocalError(invalid)
    if (invalid) return
    const existed = profiles.some((p) => p.version === value)
    setSent(value)
    setSubmitting(true)
    try {
      const profile = await registry.registerHarnessProfile(api, { harnessId: harness.id, version: value })
      const status = operationStatusLabel(profile.operationStatus)
      setMessage(
        existed
          ? `版 ${profile.version} は同じ commit SHA（${profile.commitSha}）で登録済みです。変更はありません（${status}）。`
          : `版 ${profile.version} を登録しました。commit SHA: ${profile.commitSha}、状態: ${status}。`,
      )
      setVersion('')
      onRegistered()
    } catch (cause) {
      setError(cause)
      // 409 は状態が変わったことを示すので、最新の一覧を読み直す（13 6節）。
      if (cause instanceof ApiError && cause.status === 409) onRegistered()
    } finally {
      setSubmitting(false)
    }
  }

  let description: string | undefined
  if (error instanceof ApiError && error.status === 409) {
    const registered = profiles.find((p) => p.version === sent)
    description =
      `版 ${sent} は別の commit SHA ですでに登録されています。公開済みのタグ（profile/v${sent}）は付け替えられないため、` +
      `登録済みの版はそのままです。` +
      (registered ? `登録済みの commit SHA: ${registered.commitSha}。` : '') +
      `新しい内容は別の版として登録してください。（サーバーの応答: ${error.message}）`
  }

  return (
    <form className="grid gap-4" noValidate onSubmit={handleSubmit} aria-label="Profile の版の登録">
      <ApiErrorAlert error={error} unmatchedDetails={unmatched} fieldLabels={PROFILE_CHECK_LABELS} description={description} />
      <div className="grid items-start gap-4 md:grid-cols-[minmax(0,20rem)_auto]">
        <TextField
          name="version"
          label="版"
          placeholder="1.2.0"
          value={version}
          autoComplete="off"
          spellCheck={false}
          onChange={(event) => setVersion(event.target.value)}
          errors={errors.length > 0 ? errors : undefined}
          hint="ハーネスリポジトリのタグ profile/v<版> を commit SHA に解決し、その commit の evaloom/profile.yaml を読みます。"
        />
        <div className="md:pt-[1.375rem]">
          <Button type="submit" disabled={submitting}>
            {submitting ? '確かめています…' : '版を登録'}
          </Button>
        </div>
      </div>
      {message ? (
        <p role="status" className="text-sm">
          {message}
        </p>
      ) : null}
    </form>
  )
}

type ProfilesProps = {
  harness: Harness
  profiles: readonly HarnessProfile[]
  selectedId: string | null
  onSelect: (id: string) => void
  onChanged: () => void
}

function ProfilesCard({ harness, profiles, selectedId, onSelect, onChanged }: ProfilesProps) {
  const { api } = useSession()
  const [settingId, setSettingId] = useState<string | null>(null)
  const [defaultError, setDefaultError] = useState<unknown>(null)
  const [defaultMessage, setDefaultMessage] = useState<string | null>(null)

  /** 既定の版（直接振り分けたタスクで使う版、13 3.2）を設定する。PATCH /api/harnesses/{id}。 */
  async function makeDefault(profile: HarnessProfile) {
    if (settingId) return
    setSettingId(profile.id)
    setDefaultError(null)
    setDefaultMessage(null)
    try {
      await registry.updateHarness(api, harness.id, { defaultProfileId: profile.id })
      setDefaultMessage(`既定の版を ${profile.version} にしました。直接振り分けたタスクはこの版で実行します。`)
      onChanged()
    } catch (cause) {
      setDefaultError(cause)
      if (cause instanceof ApiError && cause.status === 409) onChanged()
    } finally {
      setSettingId(null)
    }
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>版</CardTitle>
        <CardDescription>
          版は SemVer が正本で、登録時の commit SHA を記録します。評価基盤ができるまで、版はすべて「暫定運用」です。新しい版を登録しても、Project
          が頼む版と既定の版は自動では変わりません。
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-6">
        <ApiErrorAlert error={defaultError} fieldLabels={{ defaultProfileId: '既定の版', approvalWaivers: '承認を省く操作' }} />
        {defaultMessage ? (
          <p role="status" className="text-sm">
            {defaultMessage}
          </p>
        ) : null}
        {profiles.length === 0 ? (
          <p className="text-muted-foreground text-sm">登録済みの版はありません。</p>
        ) : (
          <div className="overflow-x-auto rounded-lg border">
            <table className="w-full text-sm" aria-label="登録済みの版">
              <thead className="bg-muted/50 text-left">
                <tr>
                  {['版', 'commit SHA', '契約の版', '運用の状態', '登録時刻', '操作'].map((label) => (
                    <th key={label} scope="col" className="px-3 py-2 font-medium whitespace-nowrap">
                      {label}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {profiles.map((p) => {
                  const isDefault = harness.defaultProfileId === p.id
                  const selected = selectedId === p.id
                  return (
                    <tr key={p.id} className={`border-t align-top ${selected ? 'bg-muted/40' : ''}`}>
                      <td className="px-3 py-2 font-mono font-medium whitespace-nowrap">
                        {p.version}
                        {isDefault ? <span className="ml-2 rounded border px-1.5 py-0.5 font-sans text-xs font-normal">既定の版</span> : null}
                      </td>
                      <td className="px-3 py-2 font-mono text-xs break-all">{p.commitSha}</td>
                      <td className="px-3 py-2 font-mono">{p.contractVersion ?? p.definition?.contractVersion ?? '-'}</td>
                      <td className="px-3 py-2 whitespace-nowrap">{operationStatusLabel(p.operationStatus)}</td>
                      <td className="px-3 py-2 whitespace-nowrap">{new Date(p.registeredAt).toLocaleString('ja-JP')}</td>
                      <td className="px-3 py-2">
                        <div className="flex flex-wrap gap-2">
                          <Button type="button" size="sm" variant={selected ? 'secondary' : 'outline'} aria-pressed={selected} onClick={() => onSelect(p.id)}>
                            宣言を見る
                          </Button>
                          {isDefault ? null : (
                            <Button type="button" size="sm" variant="outline" disabled={settingId !== null} onClick={() => void makeDefault(p)}>
                              {settingId === p.id ? '設定しています…' : '既定の版にする'}
                            </Button>
                          )}
                        </div>
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        )}
        <RegisterProfileForm harness={harness} profiles={profiles} onRegistered={onChanged} />
      </CardContent>
    </Card>
  )
}

const STAGES = [
  { key: 'prepare', label: '準備（prepare）' },
  { key: 'work', label: '作業（work）' },
  { key: 'check', label: '確かめる（check）' },
  { key: 'publish', label: '外に出す（publish）' },
] as const

function List({ values, empty = 'なし' }: { values: readonly string[] | undefined; empty?: string }) {
  if (!values || values.length === 0) return <span className="text-muted-foreground">{empty}</span>
  return (
    <ul className="grid gap-0.5">
      {values.map((value) => (
        <li key={value} className="font-mono text-xs break-all">
          {value}
        </li>
      ))}
    </ul>
  )
}

function StageRow({ label, stage, work }: { label: string; stage: ProfileProgramStage | undefined; work?: ProfileWorkStage }) {
  const timeout = work?.timeoutSec ?? stage?.timeoutSec
  return (
    <tr className="border-t align-top" data-stage={label}>
      <th scope="row" className="px-3 py-2 text-left font-medium whitespace-nowrap">
        {label}
      </th>
      <td className="px-3 py-2 whitespace-nowrap">{timeout === undefined ? '-' : `${timeout} 秒`}</td>
      <td className="px-3 py-2">
        {work ? <span className="text-muted-foreground">モデルの API だけ（作業の段、契約 6.2）</span> : <List values={stage?.network?.allow} empty="許可なし" />}
      </td>
      <td className="px-3 py-2">
        {work || !stage?.secrets || stage.secrets.length === 0 ? (
          <span className="text-muted-foreground">なし</span>
        ) : (
          <ul className="grid gap-0.5">
            {stage.secrets.map((secret) => (
              <li key={secret.slot}>
                <span className="font-mono">{secret.slot}</span>：{secretAccessLabel(secret.access)}
              </li>
            ))}
          </ul>
        )}
      </td>
      <td className="px-3 py-2">
        {work ? (
          <dl className="grid gap-0.5">
            <div>
              <dt className="text-muted-foreground inline">ミクロハーネス：</dt>
              <dd className="inline font-mono">{work.microHarness ?? '-'}</dd>
            </div>
            <div>
              <dt className="text-muted-foreground inline">モデル：</dt>
              <dd className="inline font-mono">{work.model ?? '-'}</dd>
            </div>
          </dl>
        ) : stage?.command ? (
          <span className="font-mono text-xs break-all">{stage.command.join(' ')}</span>
        ) : (
          <span className="text-muted-foreground">-</span>
        )}
      </td>
    </tr>
  )
}

/** 版の詳細（13 3.7）。Profile 定義（契約 5章）の宣言を読みやすく並べ、定義の全文も出す。鍵は名前と使い方だけ。 */
function ProfileDetailCard({ profile }: { profile: HarnessProfile }) {
  const definition = profile.definition ?? {}
  const stages = definition.stages ?? {}
  const approvals = definition.approvals ?? []
  return (
    <Card aria-label={`版 ${profile.version} の宣言`}>
      <CardHeader>
        <CardTitle>版 {profile.version} の宣言</CardTitle>
        <CardDescription>
          commit {shortSha(profile.commitSha)} の evaloom/profile.yaml の宣言です。鍵は名前と使い方だけを出し、値は画面で扱いません。
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-6">
        <dl className="divide-y">
          <Row label="説明（description）">{definition.description ?? '-'}</Row>
          <Row label="受け入れ条件">{acceptanceCriteriaLabel(definition.acceptanceCriteria)}</Row>
          <Row label="渡す値の形（params.schema）">{definition.params?.schema ? mono(definition.params.schema) : 'なし'}</Row>
        </dl>

        <section className="grid gap-2">
          <h3 className="text-sm font-medium">段ごとの宣言</h3>
          <div className="overflow-x-auto rounded-lg border">
            <table className="w-full text-sm" aria-label="段ごとの宣言">
              <thead className="bg-muted/50 text-left">
                <tr>
                  {['段', '上限時間', '外への接続の許可宛先', '鍵の名前と使い方', '実行するもの'].map((label) => (
                    <th key={label} scope="col" className="px-3 py-2 font-medium whitespace-nowrap">
                      {label}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {STAGES.map(({ key, label }) => {
                  if (key === 'work') return <StageRow key={key} label={label} stage={undefined} work={stages.work ?? {}} />
                  const stage = stages[key]
                  if (key === 'prepare' && !stage) return null
                  return <StageRow key={key} label={label} stage={stage ?? undefined} />
                })}
              </tbody>
            </table>
          </div>
          {stages.prepare ? null : <p className="text-muted-foreground text-xs">準備の段（prepare）は宣言されていません。</p>}
        </section>

        <section className="grid gap-2">
          <h3 className="text-sm font-medium">承認が要る操作</h3>
          {approvals.length === 0 ? (
            <p className="text-muted-foreground text-sm">承認が要る操作は宣言されていません。</p>
          ) : (
            <div className="overflow-x-auto rounded-lg border">
              <table className="w-full text-sm" aria-label="承認が要る操作">
                <thead className="bg-muted/50 text-left">
                  <tr>
                    {['操作', '説明', '承認を省けるか（waivable）'].map((label) => (
                      <th key={label} scope="col" className="px-3 py-2 font-medium whitespace-nowrap">
                        {label}
                      </th>
                    ))}
                  </tr>
                </thead>
                <tbody>
                  {approvals.map((approval) => (
                    <tr key={approval.operation} className="border-t">
                      <td className="px-3 py-2 font-mono">{approval.operation}</td>
                      <td className="px-3 py-2">{approval.description ?? '-'}</td>
                      <td className="px-3 py-2 whitespace-nowrap">
                        {approval.waivable ? '省ける（waivable: true）' : '省けない（waivable: false）'}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </section>

        <section className="grid gap-2">
          <h3 className="text-sm font-medium">記録として残すもの</h3>
          {definition.records ? (
            <dl className="divide-y">
              <Row label="残すファイル（keep）">
                <List values={definition.records.keep} />
              </Row>
              <Row label="次の仕事に渡す件数（pass.recent）">{definition.records.pass?.recent ?? '-'}</Row>
            </dl>
          ) : (
            <p className="text-muted-foreground text-sm">記録は宣言されていません。</p>
          )}
        </section>

        <details className="rounded-lg border p-3">
          <summary className="cursor-pointer text-sm font-medium">定義の全文</summary>
          <pre className="bg-muted/40 mt-3 max-h-[32rem] overflow-auto rounded p-3 font-mono text-xs" aria-label="定義の全文">
            {JSON.stringify(profile.definition ?? null, null, 2)}
          </pre>
        </details>
      </CardContent>
    </Card>
  )
}

type UsagesProps = { projects: readonly Project[]; routes: readonly Route[]; profiles: readonly HarnessProfile[] }

/** 使っている Project と、このスペシャルハーネスへ直接振り分けるラベル（13 3.7）。 */
function UsagesCard({ projects, routes, profiles }: UsagesProps) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>使っている Project と直接振り分けるラベル</CardTitle>
        <CardDescription>Project が頼む版の変更は Project の編集で、ラベルの変更は振り分けの表で行います。</CardDescription>
      </CardHeader>
      <CardContent className="grid gap-6">
        {projects.length === 0 ? (
          <p className="text-muted-foreground text-sm">このスペシャルハーネスを頼む Project はありません。</p>
        ) : (
          <div className="overflow-x-auto rounded-lg border">
            <table className="w-full text-sm" aria-label="このスペシャルハーネスを使っている Project">
              <thead className="bg-muted/50 text-left">
                <tr>
                  <th scope="col" className="px-3 py-2 font-medium">
                    Project
                  </th>
                  <th scope="col" className="px-3 py-2 font-medium">
                    版
                  </th>
                  <th scope="col" className="px-3 py-2 font-medium">
                    振り分けのラベル
                  </th>
                </tr>
              </thead>
              <tbody>
                {projects.map((project) => (
                  <tr key={project.id} className="border-t">
                    <td className="px-3 py-2">
                      <Link to={`/projects/${encodeURIComponent(project.id)}`} className="font-medium underline-offset-4 hover:underline">
                        {project.name}
                      </Link>
                    </td>
                    <td className="px-3 py-2 font-mono">{versionOf(profiles, project.profileId) ?? '-'}</td>
                    <td className="px-3 py-2 font-mono">{project.routingLabel ?? '-'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        <div className="grid gap-2">
          <h3 className="text-sm font-medium">直接振り分けるラベル</h3>
          {routes.length === 0 ? (
            <p className="text-muted-foreground text-sm">振り分けの表に、このスペシャルハーネスへ直接振り分けるラベルはありません。</p>
          ) : (
            <ul className="grid gap-1 text-sm" aria-label="直接振り分けるラベル">
              {routes.map((route) => (
                <li key={route.label} className="font-mono">
                  {route.label}
                </li>
              ))}
            </ul>
          )}
        </div>
      </CardContent>
    </Card>
  )
}

/** スペシャルハーネス詳細（13 3.7）。版の一覧と登録、版ごとの宣言、既定の版、使っている Project とラベル、設定。 */
export function HarnessDetailPage({ id }: { id: string }) {
  const { api } = useSession()
  const harness = useResource((signal) => registry.getHarness(api, id, signal), `harness:${id}`)
  const profiles = useResource((signal) => registry.listHarnessProfiles(api, id, signal), `profiles:${id}`)
  const projects = useResource((signal) => registry.listProjects(api, signal), 'projects')
  const routes = useResource((signal) => registry.listRoutes(api, signal), 'routes')
  const [selected, setSelected] = useState<string | null>(null)
  const notFound = harness.data === undefined && !harness.loading && !harness.error
  const error = harness.error ?? (notFound ? harnessNotFound() : null) ?? profiles.error ?? projects.error ?? routes.error
  const data = harness.data
  const reload = () => {
    harness.reload()
    profiles.reload()
    projects.reload()
    routes.reload()
  }
  const changed = () => {
    harness.reload()
    profiles.reload()
  }
  // 選んでいなければ既定の版、無ければ最後に登録した版の宣言を出す。
  const list = profiles.data ?? []
  const shown =
    list.find((p) => p.id === selected) ?? list.find((p) => p.id === data?.defaultProfileId) ?? (list.length > 0 ? list[list.length - 1] : undefined)
  return (
    <div className="grid gap-6">
      <PageTitle
        title={data?.name ?? 'スペシャルハーネス'}
        subtitle={id}
        actions={
          <>
            <Button asChild variant="outline">
              <Link to="/harnesses">一覧へ戻る</Link>
            </Button>
            {data ? (
              <Button asChild>
                <Link to={`${harnessPath(id)}/edit`}>編集</Link>
              </Button>
            ) : null}
          </>
        }
      />
      <ApiErrorAlert error={error} onReload={reload} />
      {data && profiles.data && projects.data && routes.data ? (
        <>
          <ProfilesCard harness={data} profiles={profiles.data} selectedId={shown?.id ?? null} onSelect={setSelected} onChanged={changed} />
          {shown ? <ProfileDetailCard profile={shown} /> : null}
          <UsagesCard projects={projectsUsing(projects.data, id)} routes={directRoutes(routes.data, id)} profiles={profiles.data} />
          <SettingsCard harness={data} profiles={profiles.data} />
        </>
      ) : error ? null : (
        <Loading />
      )}
    </div>
  )
}
