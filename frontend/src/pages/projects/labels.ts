/**
 * 登録画面のパス。Project ID は slug（^[a-z0-9]+(-[a-z0-9]+)*$、backend/internal/api/validation.go）なので、
 * slug に使えない「_」を含めて、どの Project の詳細（/projects/:id）とも衝突しないようにする。
 */
export const PROJECT_NEW_PATH = '/projects/_new'

// Project の各項目の表示名（13 3.2「Project 登録・編集」、02-data-model 3.2 projects）。
export const PROJECT_LABELS = {
  id: 'Project ID',
  name: '名称',
  runnerId: 'Runner',
  harnessId: 'Harness ID',
  profileId: 'Profile ID',
  params: '渡す値（JSON）',
  approvalWaivers: '承認を省く操作',
} as const

/** 422 の前提条件の失敗（Runner が見つからないなど）が返す details[].field → フォームの項目。 */
export const PRECONDITION_FIELDS: Record<string, string> = {
  runnerId: 'runnerId',
  profileId: 'profileId',
  approvalWaivers: 'approvalWaivers',
}

/** 409 の Linear Team ID と実行先ラベルの組の重複（details は linearRoutingLabel / duplicate）→ フォームの項目。 */
export const LINEAR_CONFLICT_FIELDS: Record<string, string> = {}
export const ERROR_FIELD_LABELS: Record<string, string> = { ...PROJECT_LABELS }
