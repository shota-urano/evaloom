import { useState, type FormEvent, type ReactNode } from 'react'

import { ApiError } from '@/api/client'
import { preconditionFieldErrors } from '@/api/errors'
import { operationStatusLabel, registry, shortSha, type Harness, type HarnessProfile } from '@/api/registry'
import { useSession } from '@/auth/session'
import { ApiErrorAlert } from '@/components/api-error-alert'
import { TextField } from '@/components/form-field'
import { Loading, PageTitle } from '@/components/page'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { useApiErrors } from '@/hooks/use-api-errors'
import { useResource } from '@/hooks/use-resource'
import { Link } from '@/lib/link'
import { HARNESS_LABELS as L, harnessNotFound, harnessPath, PROFILE_CHECK_LABELS, usagesOf, type HarnessUsage } from './labels'

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
const list = (values: readonly string[]) =>
  values.length === 0 ? (
    <span className="text-muted-foreground">なし</span>
  ) : (
    <ul className="grid gap-0.5 font-mono">
      {values.map((value) => (
        <li key={value}>{value}</li>
      ))}
    </ul>
  )

function SettingsCard({ harness }: { harness: Harness }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>設定</CardTitle>
      </CardHeader>
      <CardContent>
        <dl className="divide-y">
          <Row label={L.specialty}>{mono(harness.specialty)}</Row>
          <Row label={L.adapter}>{mono(harness.adapter)}</Row>
          <Row label={L.repoPath}>{mono(harness.repoPath)}</Row>
          <Row label={L.repoUrl}>{mono(harness.repoUrl)}</Row>
          <Row label={L.supportedModels}>{list(harness.supportedModels)}</Row>
          <Row label={L.availableTools}>{list(harness.availableTools)}</Row>
          <Row label={L.toolPermissions}>
            <pre className="bg-muted/50 rounded-md p-2 font-mono text-xs">{JSON.stringify(harness.toolPermissions, null, 2)}</pre>
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

function ProfilesCard({ harness, profiles, onChanged }: { harness: Harness; profiles: readonly HarnessProfile[]; onChanged: () => void }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>Profile の版</CardTitle>
        <CardDescription>
          版は SemVer が正本で、登録時の commit SHA を記録します。Phase 1 の版はすべて「暫定運用」です。新しい版を登録しても、プロジェクトの割り当ては自動では変わりません。
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-6">
        {profiles.length === 0 ? (
          <p className="text-muted-foreground text-sm">登録済みの版はありません。</p>
        ) : (
          <div className="overflow-x-auto rounded-lg border">
            <table className="w-full text-sm" aria-label="登録済みの Profile の版">
              <thead className="bg-muted/50 text-left">
                <tr>
                  <th scope="col" className="px-3 py-2 font-medium">
                    版
                  </th>
                  <th scope="col" className="px-3 py-2 font-medium">
                    commit SHA
                  </th>
                  <th scope="col" className="px-3 py-2 font-medium">
                    Model ID
                  </th>
                  <th scope="col" className="px-3 py-2 font-medium">
                    状態
                  </th>
                  <th scope="col" className="px-3 py-2 font-medium">
                    登録日時
                  </th>
                </tr>
              </thead>
              <tbody>
                {profiles.map((p) => (
                  <tr key={p.id} className="border-t">
                    <td className="px-3 py-2 font-mono font-medium">{p.version}</td>
                    <td className="px-3 py-2 font-mono text-xs break-all">{p.commitSha}</td>
                    <td className="px-3 py-2 font-mono">{p.modelId}</td>
                    <td className="px-3 py-2 whitespace-nowrap">{operationStatusLabel(p.operationStatus)}</td>
                    <td className="px-3 py-2 whitespace-nowrap">{new Date(p.registeredAt).toLocaleString('ja-JP')}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        <RegisterProfileForm harness={harness} profiles={profiles} onRegistered={onChanged} />
      </CardContent>
    </Card>
  )
}

function UsagesCard({ usages }: { usages: readonly HarnessUsage[] }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>使用中プロジェクト</CardTitle>
        <CardDescription>割り当ての変更はプロジェクト詳細で行います。</CardDescription>
      </CardHeader>
      <CardContent>
        {usages.length === 0 ? (
          <p className="text-muted-foreground text-sm">この Harness を割り当てているプロジェクトはありません。</p>
        ) : (
          <div className="overflow-x-auto rounded-lg border">
            <table className="w-full text-sm" aria-label="この Harness を使っているプロジェクト">
              <thead className="bg-muted/50 text-left">
                <tr>
                  <th scope="col" className="px-3 py-2 font-medium">
                    プロジェクト
                  </th>
                  <th scope="col" className="px-3 py-2 font-medium">
                    役割
                  </th>
                  <th scope="col" className="px-3 py-2 font-medium">
                    版
                  </th>
                  <th scope="col" className="px-3 py-2 font-medium">
                    commit SHA
                  </th>
                </tr>
              </thead>
              <tbody>
                {usages.map(({ project, assignment }) => (
                  <tr key={`${project.id}:${assignment.role}`} className="border-t">
                    <td className="px-3 py-2">
                      <Link to={`/projects/${encodeURIComponent(project.id)}`} className="font-medium underline-offset-4 hover:underline">
                        {project.name}
                      </Link>
                    </td>
                    <td className="px-3 py-2 font-mono">{assignment.role}</td>
                    <td className="px-3 py-2 font-mono">{assignment.version}</td>
                    <td className="px-3 py-2 font-mono" title={assignment.commitSha}>
                      {shortSha(assignment.commitSha)}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </CardContent>
    </Card>
  )
}

/** Harness 詳細（13 3.2）。Profile の版の一覧と登録、使用中プロジェクト、設定。 */
export function HarnessDetailPage({ id }: { id: string }) {
  const { api } = useSession()
  const harness = useResource((signal) => registry.getHarness(api, id, signal), `harness:${id}`)
  const profiles = useResource((signal) => registry.listHarnessProfiles(api, id, signal), `profiles:${id}`)
  const projects = useResource((signal) => registry.listProjects(api, signal), 'projects')
  const notFound = harness.data === undefined && !harness.loading && !harness.error
  const error = harness.error ?? (notFound ? harnessNotFound() : null) ?? profiles.error ?? projects.error
  const data = harness.data
  const reload = () => {
    harness.reload()
    profiles.reload()
    projects.reload()
  }
  return (
    <div className="grid gap-6">
      <PageTitle
        title={data?.name ?? 'Harness'}
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
      {data && profiles.data && projects.data ? (
        <>
          <ProfilesCard harness={data} profiles={profiles.data} onChanged={profiles.reload} />
          <UsagesCard usages={usagesOf(projects.data, id)} />
          <SettingsCard harness={data} />
        </>
      ) : error ? null : (
        <Loading />
      )}
    </div>
  )
}
