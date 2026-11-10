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
  defaultRole: '既定の役割',
  // 15-linear-intake 3.8（requirements 5.1 の Linear Team ID・Linear Routing Label）
  linearTeamId: 'Linear Team ID',
  linearRoutingLabel: '実行先ラベル',
} as const

/** 422 の前提条件の失敗（Runner が見つからないなど）が返す details[].field → フォームの項目。 */
export const PRECONDITION_FIELDS: Record<string, string> = {
  runnerId: 'runnerId',
}

/** 409 の Linear Team ID と実行先ラベルの組の重複（details は linearRoutingLabel / duplicate）→ フォームの項目。 */
export const LINEAR_CONFLICT_FIELDS: Record<string, string> = {
  linearTeamId: 'linearTeamId',
  linearRoutingLabel: 'linearRoutingLabel',
}

/** 422・409 の一覧で details[].field を画面の名前で出すための表。 */
export const ERROR_FIELD_LABELS: Record<string, string> = {
  linearTeamId: PROJECT_LABELS.linearTeamId,
  linearRoutingLabel: PROJECT_LABELS.linearRoutingLabel,
  runnerId: PROJECT_LABELS.runnerId,
}
