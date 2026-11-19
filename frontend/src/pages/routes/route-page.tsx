import { useState, type FormEvent } from 'react'

import { type FieldErrors } from '@/api/errors'
import { registry, type Harness, type Project, type Route } from '@/api/registry'
import { settingsApi } from '@/api/settings'
import { useSession } from '@/auth/session'
import { ApiErrorAlert } from '@/components/api-error-alert'
import { ConfirmDialog } from '@/components/confirm-dialog'
import { FieldError, SelectField, TextField } from '@/components/form-field'
import { Loading, PageTitle } from '@/components/page'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { useApiErrors } from '@/hooks/use-api-errors'
import { useResource } from '@/hooks/use-resource'
import { Link } from '@/lib/link'
import {
  CONFLICT_TEXT,
  NO_DEFAULT_PROFILE_TEXT,
  RECHECK_TEXT,
  ROUTE_LABELS as L,
  isConflict,
  labelInvalidText,
  routeRow,
  validateRoute,
  type RouteFormValues,
} from './labels'

// 振り分けの表（13 3.6）。ラベルと振り分け先の一覧、登録・変更・削除。

const FIELDS = ['label', 'projectId', 'harnessId'] as const

type FormProps = {
  /** 変更する行。無ければ登録。 */
  route?: Route
  projects: Project[]
  harnesses: Harness[]
  prefix: string | undefined
  onSaved: (route: Route, create: boolean) => void
  onCancel?: () => void
  onReload: () => void
  /** 送信を始めたとき（前のお知らせを消す）。 */
  onStart: () => void
}

function RouteForm({ route, projects, harnesses, prefix, onSaved, onCancel, onReload, onStart }: FormProps) {
  const { api } = useSession()
  const create = route === undefined
  const [values, setValues] = useState<RouteFormValues>({
    label: route?.label ?? '',
    projectId: route?.projectId ?? '',
    harnessId: route?.harnessId ?? '',
  })
  const [localErrors, setLocalErrors] = useState<FieldErrors>({})
  const [submitting, setSubmitting] = useState(false)
  const { error, setError, clear, fieldErrors, unmatched } = useApiErrors(FIELDS)
  const harness = harnesses.find((h) => h.id === values.harnessId)

  const errorsFor = (field: string) => {
    const server = field === 'label' && fieldErrors.label ? [labelInvalidText(prefix)] : (fieldErrors[field] ?? [])
    const messages = [...(localErrors[field] ?? []), ...server]
    return messages.length > 0 ? messages : undefined
  }

  const bind = (field: keyof RouteFormValues) => ({
    name: field,
    value: values[field],
    errors: errorsFor(field),
    onChange: (event: { target: { value: string } }) => setValues((previous) => ({ ...previous, [field]: event.target.value })),
  })

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (submitting) return
    clear()
    onStart()
    const { label, destination, errors } = validateRoute(values, create)
    setLocalErrors(errors)
    if (!destination) return
    setSubmitting(true)
    try {
      const saved = create ? await registry.createRoute(api, label ?? '', destination) : await registry.updateRoute(api, route.label, destination)
      onSaved(saved, create)
    } catch (cause) {
      setError(cause)
      setSubmitting(false)
    }
  }

  const conflict = isConflict(error)
  const title = create ? 'ラベルの登録' : `「${route.label}」の振り分け先の変更`
  return (
    <form className="grid gap-4" noValidate onSubmit={handleSubmit} aria-label={title}>
      <ApiErrorAlert
        error={error}
        unmatchedDetails={unmatched}
        fieldLabels={L}
        title={conflict ? (create ? 'ラベルを登録できませんでした' : '振り分け先を変更できませんでした') : undefined}
        description={conflict ? CONFLICT_TEXT : undefined}
        onReload={
          conflict
            ? undefined
            : () => {
                clear()
                onReload()
              }
        }
      />
      <div className="grid gap-4 md:grid-cols-3">
        <TextField
          label={L.label}
          autoComplete="off"
          spellCheck={false}
          disabled={!create}
          hint={create ? (prefix ? `「${prefix}」で始まる文字列。` : undefined) : 'ラベルは変更できません。変えるときは削除して登録し直してください。'}
          {...bind('label')}
        />
        <SelectField label={L.projectId} {...bind('projectId')}>
          <option value="">選ばない</option>
          {projects.map((project) => (
            <option key={project.id} value={project.id}>
              {project.name === project.id ? project.id : `${project.name}（${project.id}）`}
            </option>
          ))}
        </SelectField>
        <SelectField label={L.harnessId} {...bind('harnessId')}>
          <option value="">選ばない</option>
          {harnesses.map((h) => (
            <option key={h.id} value={h.id}>
              {h.name === h.id ? h.id : `${h.name}（${h.id}）`}
            </option>
          ))}
        </SelectField>
      </div>
      <div className="grid gap-1 text-sm" data-field="destination">
        <p className="text-muted-foreground">振り分け先は Project かスペシャルハーネスのどちらか一方を選びます。</p>
        <FieldError id="route-destination-error" messages={localErrors.destination} />
        {harness && harness.defaultProfileId === null ? (
          <p className="text-warning" data-testid="no-default-profile">
            {NO_DEFAULT_PROFILE_TEXT}
            <Link to={`/harnesses/${encodeURIComponent(harness.id)}/edit`} className="ml-1 underline underline-offset-4">
              既定の版を設定する
            </Link>
          </p>
        ) : null}
      </div>
      <div className="flex gap-3">
        <Button type="submit" disabled={submitting}>
          {submitting ? '保存しています…' : create ? '登録' : '保存'}
        </Button>
        {onCancel ? (
          <Button type="button" variant="outline" onClick={onCancel}>
            キャンセル
          </Button>
        ) : null}
      </div>
    </form>
  )
}

export function RoutePage() {
  const { api } = useSession()
  const routes = useResource((signal) => registry.listRoutes(api, signal), 'routes')
  const projects = useResource((signal) => registry.listProjects(api, signal), 'projects')
  const harnesses = useResource((signal) => registry.listHarnesses(api, signal), 'harnesses')
  // 接頭辞が読めなくても登録はできる（サーバーが検証する）。
  const settings = useResource((signal) => settingsApi.get(api, signal).catch(() => null), 'settings')
  const [editing, setEditing] = useState<Route | null>(null)
  const [deleting, setDeleting] = useState<Route | null>(null)
  const [deleteError, setDeleteError] = useState<unknown>(null)
  const [busy, setBusy] = useState(false)
  const [notice, setNotice] = useState<string | null>(null)
  // 登録に成功したら空のフォームに作り直す。
  const [formKey, setFormKey] = useState(0)

  const error = routes.error ?? projects.error ?? harnesses.error
  const reload = () => {
    routes.reload()
    projects.reload()
    harnesses.reload()
  }
  const ready = routes.data !== undefined && projects.data !== undefined && harnesses.data !== undefined && settings.data !== undefined
  const prefix = settings.data?.linear.routingLabelPrefix

  function handleSaved(route: Route, create: boolean) {
    setNotice(create ? `ラベル「${route.label}」を登録しました。` : `ラベル「${route.label}」の振り分け先を変更しました。`)
    setEditing(null)
    setFormKey((n) => n + 1)
    routes.reload()
  }

  async function handleDelete() {
    if (!deleting) return
    setBusy(true)
    setDeleteError(null)
    try {
      await registry.deleteRoute(api, deleting.label)
      setNotice(`ラベル「${deleting.label}」を削除しました。`)
      if (editing?.label === deleting.label) setEditing(null)
    } catch (cause) {
      setDeleteError(cause)
    }
    setDeleting(null)
    setBusy(false)
    routes.reload()
  }

  return (
    <div className="grid gap-6">
      <PageTitle title="振り分けの表" />
      <p className="text-muted-foreground text-sm" data-testid="recheck-note">
        Linear のタスクに付いたラベルから、振り分け先（Project かスペシャルハーネス）を引きます。{RECHECK_TEXT}
      </p>
      <ApiErrorAlert error={error} onReload={reload} />
      {notice ? (
        <Alert role="status">
          <AlertDescription>
            {notice}
            {RECHECK_TEXT}
          </AlertDescription>
        </Alert>
      ) : null}
      <ApiErrorAlert error={deleteError} title={deleteError ? '削除できませんでした' : undefined} />
      {!ready ? (
        error ? null : (
          <Loading />
        )
      ) : (
        <>
          <Card>
            <CardHeader>
              <CardTitle>{editing ? `「${editing.label}」の振り分け先の変更` : 'ラベルの登録'}</CardTitle>
            </CardHeader>
            <CardContent>
              <RouteForm
                key={editing ? `edit:${editing.label}:${editing.updatedAt}` : `create:${formKey}`}
                route={editing ?? undefined}
                projects={projects.data ?? []}
                harnesses={harnesses.data ?? []}
                prefix={prefix}
                onSaved={handleSaved}
                onCancel={editing ? () => setEditing(null) : undefined}
                onReload={reload}
                onStart={() => setNotice(null)}
              />
            </CardContent>
          </Card>

          {(routes.data ?? []).length === 0 ? (
            <Card>
              <CardContent className="text-muted-foreground text-sm">登録済みのラベルはありません。</CardContent>
            </Card>
          ) : (
            <div className="overflow-x-auto rounded-xl border">
              <table className="w-full text-sm">
                <caption className="sr-only">振り分けの表</caption>
                <thead className="bg-muted/50 text-left">
                  <tr>
                    <th scope="col" className="px-4 py-2 font-medium whitespace-nowrap">
                      ラベル
                    </th>
                    <th scope="col" className="px-4 py-2 font-medium whitespace-nowrap">
                      振り分け先の種類
                    </th>
                    <th scope="col" className="px-4 py-2 font-medium whitespace-nowrap">
                      振り分け先の名前
                    </th>
                    <th scope="col" className="px-4 py-2 font-medium whitespace-nowrap">
                      <span className="sr-only">操作</span>
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {(routes.data ?? []).map((route) => {
                    const row = routeRow(route, projects.data ?? [], harnesses.data ?? [])
                    const to = row.kind === 'Project' ? `/projects/${encodeURIComponent(row.id)}` : `/harnesses/${encodeURIComponent(row.id)}`
                    return (
                      <tr key={route.label} className="border-t align-top" data-testid="route-row">
                        <td className="px-4 py-3 font-mono text-xs whitespace-nowrap">{route.label}</td>
                        <td className="px-4 py-3 whitespace-nowrap">{row.kind}</td>
                        <td className="px-4 py-3">
                          <Link to={to} className="underline-offset-4 hover:underline">
                            {row.name}
                          </Link>
                          <div className="text-muted-foreground font-mono text-xs">{row.id}</div>
                          {row.harness && row.harness.defaultProfileId === null ? (
                            <div className="text-muted-foreground text-xs">既定の版が無いため実行されません</div>
                          ) : null}
                        </td>
                        <td className="px-4 py-3">
                          <div className="flex justify-end gap-2">
                            <Button
                              type="button"
                              size="sm"
                              variant="outline"
                              aria-label={`「${route.label}」を変更`}
                              onClick={() => {
                                setNotice(null)
                                setEditing(route)
                              }}
                            >
                              変更
                            </Button>
                            <Button
                              type="button"
                              size="sm"
                              variant="outline"
                              aria-label={`「${route.label}」を削除`}
                              onClick={() => {
                                setNotice(null)
                                setDeleteError(null)
                                setDeleting(route)
                              }}
                            >
                              削除
                            </Button>
                          </div>
                        </td>
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            </div>
          )}
        </>
      )}

      {deleting ? (
        <ConfirmDialog
          title={`「${deleting.label}」を削除しますか`}
          confirmLabel="削除"
          initialFocus="cancel"
          busy={busy}
          onCancel={() => setDeleting(null)}
          onConfirm={handleDelete}
        >
          <p>{RECHECK_TEXT}</p>
        </ConfirmDialog>
      ) : null}
    </div>
  )
}
