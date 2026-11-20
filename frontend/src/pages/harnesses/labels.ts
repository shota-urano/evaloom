import { ApiError } from '@/api/client'
import type { HarnessProfile, Project, Route } from '@/api/registry'

/**
 * 登録画面のパス。Harness ID も Project ID と同じ slug（^[a-z0-9]+(-[a-z0-9]+)*$、backend/internal/api/validation.go）なので、
 * slug に使えない「_」を含めて、どの Harness の詳細（/harnesses/:id）とも衝突しないようにする。
 */
export const HARNESS_NEW_PATH = '/harnesses/_new'

export const harnessPath = (id: string) => `/harnesses/${encodeURIComponent(id)}`

// Harness の各項目の表示名（13 3.2「Harness 登録・編集」、02-data-model 3.2 harnesses）。
export const HARNESS_LABELS = {
  id: 'スペシャルハーネス ID',
  name: '名称',
  specialty: '専門分野',
  adapter: 'Adapter',
  repoPath: 'ハーネスリポジトリのパス',
  repoUrl: 'ハーネスリポジトリ URL',
  supportedModels: '対応モデル',
  availableTools: '利用可能ツール',
  toolPermissions: 'Tool Permissions',
} as const

/** 03 3.4 の Adapter（08-harness-adapter）。登録済みかどうかはサーバーが確かめる。 */
export const ADAPTERS = [
  { value: 'codex', label: 'Codex（codex）' },
  { value: 'claude_code', label: 'Claude Code（claude_code）' },
] as const

/** 422 のパス検証（internal/registry/repository.go）が返す details[].field → フォームの項目。 */
export const HARNESS_REPOSITORY_FIELDS: Record<string, string> = {
  path: 'repoPath',
  url: 'repoUrl',
}

/** 422 の一覧で details[].field を画面の名前で出すための表。 */
export const HARNESS_REPOSITORY_LABELS: Record<string, string> = {
  path: HARNESS_LABELS.repoPath,
  url: HARNESS_LABELS.repoUrl,
  worktreeRoot: 'Runner の Worktree Root',
}

/** 版の登録（POST /api/harness-profiles、backend/internal/api/harness_profiles.md）の 422 の details[].field の表示名。 */
export const PROFILE_CHECK_LABELS: Record<string, string> = {
  version: '版',
  tagName: 'タグ',
  commitSha: 'commit SHA',
  definition: '定義ファイル（evaloom/profile.yaml）',
  profileVersion: '定義ファイルの profileVersion',
  'executionConfiguration.adapter': '定義ファイルの executionConfiguration.adapter',
}

/** GET /api/harnesses/{id} が返さなかったときの表示（個別 GET の 404 と同じ）。 */
export const harnessNotFound = () => new ApiError(404, 'not_found', 'Harness not found')

/** このスペシャルハーネスを頼む Project（02 3.2 projects.harness_id）。 */
export function projectsUsing(projects: readonly Project[], harnessId: string): Project[] {
  return projects.filter((project) => project.harnessId === harnessId)
}

/** 振り分けの表でこのスペシャルハーネスへ直接振り分けるラベル（02 3.2 routes.harness_id）。 */
export function directRoutes(routes: readonly Route[], harnessId: string): Route[] {
  return routes.filter((route) => route.harnessId === harnessId)
}

/** 版の ID から表示用の版を引く。見つからなければ ID をそのまま返す。 */
export function versionOf(profiles: readonly HarnessProfile[], profileId: string | null | undefined): string | null {
  if (!profileId) return null
  return profiles.find((p) => p.id === profileId)?.version ?? profileId
}

/** 鍵の使い方（契約 5.1 secrets[].access）の表示名。 */
export function secretAccessLabel(access: string): string {
  if (access === 'read') return '読み取り（read）'
  if (access === 'write') return '書き込み（write）'
  return access
}

/** 受け入れ条件の要否（契約 5.2 acceptanceCriteria）の表示名。 */
export function acceptanceCriteriaLabel(value: string | undefined): string {
  if (value === 'required') return '必須（required）'
  if (value === 'optional') return '任意（optional）'
  return value ?? '宣言なし'
}
