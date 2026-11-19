import { ApiError } from '@/api/client'
import type { FieldErrors } from '@/api/errors'
import type { Harness, Project, Route, RouteDestination } from '@/api/registry'

/** 振り分けの表の画面（13 3.6）。 */
export const ROUTES_PATH = '/routes'

export const ROUTE_LABELS = {
  label: 'ラベル',
  projectId: 'Project',
  harnessId: 'スペシャルハーネス',
} as const

export type RouteFormValues = { label: string; projectId: string; harnessId: string }

/**
 * 画面での検証（正本はサーバー側、13 3.1）。振り分け先は Project かスペシャルハーネスのどちらか一方だけ。
 * 接頭辞の検証はサーバーに任せ、400 の内容を出す。
 */
export function validateRoute(
  values: RouteFormValues,
  create: boolean,
): { label?: string; destination?: RouteDestination; errors: FieldErrors } {
  const errors: FieldErrors = {}
  const label = values.label.trim()
  if (create && label === '') errors.label = ['入力してください']
  const project = values.projectId !== ''
  const harness = values.harnessId !== ''
  if (project && harness) errors.destination = ['Project かスペシャルハーネスのどちらか一方だけを選んでください']
  else if (!project && !harness) errors.destination = ['Project かスペシャルハーネスのどちらか一方を選んでください']
  if (Object.keys(errors).length > 0) return { errors }
  return { errors, label, destination: project ? { projectId: values.projectId } : { harnessId: values.harnessId } }
}

/** 400 の label の失敗を、接頭辞を添えた文言にする。 */
export function labelInvalidText(prefix: string | undefined): string {
  return prefix
    ? `「${prefix}」で始まるラベルにしてください（接頭辞だけ・起動ラベル・前後の空白・100文字を超えるラベルは使えません）`
    : '振り分けのラベルの接頭辞で始まるラベルにしてください'
}

/** 409（ラベルの重複、同じ振り分け先への2つ目のラベル、backend/internal/store/routes.go ErrRouteConflict）。 */
export const CONFLICT_TEXT = '同じラベルか、同じ振り分け先へのラベルがすでに登録されています。振り分け先ごとにラベルは1つだけです。'

export function isConflict(error: unknown): boolean {
  return error instanceof ApiError && error.status === 409
}

export type RouteRow = { route: Route; kind: 'Project' | 'スペシャルハーネス'; id: string; name: string; harness?: Harness }

/** 一覧の行。振り分け先の名前は Project / スペシャルハーネスの一覧から引き、無ければ ID を出す。 */
export function routeRow(route: Route, projects: readonly Project[], harnesses: readonly Harness[]): RouteRow {
  if (route.projectId !== null) {
    const project = projects.find((p) => p.id === route.projectId)
    return { route, kind: 'Project', id: route.projectId, name: project?.name ?? route.projectId }
  }
  const id = route.harnessId ?? ''
  const harness = harnesses.find((h) => h.id === id)
  return { route, kind: 'スペシャルハーネス', id, name: harness?.name ?? id, harness }
}

/** スペシャルハーネスへ直接振り分けるとき、既定の版が無いと実行されないこと（15 3.6）。 */
export const NO_DEFAULT_PROFILE_TEXT = 'このスペシャルハーネスには既定の版が無いため、このラベルで振り分けたタスクは実行されません。'

/** 表を変えたときの再判定（15 3.6.3）。 */
export const RECHECK_TEXT = '振り分けの表を変えると、振り分けを待っているタスクを次の照会で判定し直します。'
