import { ApiError } from '@/api/client'
import type { Project, ProjectHarness } from '@/api/registry'

/**
 * 登録画面のパス。Harness ID も Project ID と同じ slug（^[a-z0-9]+(-[a-z0-9]+)*$、backend/internal/api/validation.go）なので、
 * slug に使えない「_」を含めて、どの Harness の詳細（/harnesses/:id）とも衝突しないようにする。
 */
export const HARNESS_NEW_PATH = '/harnesses/_new'

export const harnessPath = (id: string) => `/harnesses/${encodeURIComponent(id)}`

// Harness の各項目の表示名（13 3.2「Harness 登録・編集」、02-data-model 3.2 harnesses）。
export const HARNESS_LABELS = {
  id: 'Harness ID',
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

/** 個別の GET が無いので、一覧に無い ID は個別 GET の 404 と同じ表示にする。 */
export const harnessNotFound = () => new ApiError(404, 'not_found', 'Harness not found')

export type HarnessUsage = { project: Project; assignment: ProjectHarness }

/** Harness を割り当てている Project と役割（GET /api/projects の harnesses）。 */
export function usagesOf(projects: readonly Project[], harnessId: string): HarnessUsage[] {
  return projects.flatMap((project) =>
    (project.harnesses ?? []).filter((h) => h.harnessId === harnessId).map((assignment) => ({ project, assignment })),
  )
}
