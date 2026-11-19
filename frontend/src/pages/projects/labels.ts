/**
 * 登録画面のパス。Project ID は slug（^[a-z0-9]+(-[a-z0-9]+)*$、backend/internal/api/validation.go）なので、
 * slug に使えない「_」を含めて、どの Project の詳細（/projects/:id）とも衝突しないようにする。
 */
export const PROJECT_NEW_PATH = '/projects/_new'

// Project の6項目の表示名（13 3.5）。名前の項目は、登録時だけ Project ID（slug）も入力する。
export const PROJECT_LABELS = {
  id: 'Project ID',
  name: '名前',
  routingLabel: '振り分けのラベル',
  harnessId: '頼むスペシャルハーネス',
  profileId: '版',
  params: '渡す値（JSON）',
  approvalWaivers: '承認を省く設定',
  runnerId: '動かすマシン',
} as const

/** 422 の前提条件の失敗（渡す値がスキーマに合わない、Runner が無いなど）の details[].field → フォームの項目。 */
export const PRECONDITION_FIELDS: Record<string, string> = {
  harnessId: 'harnessId',
  runnerId: 'runnerId',
  profileId: 'profileId',
  params: 'params',
  approvalWaivers: 'approvalWaivers',
}

export const ERROR_FIELD_LABELS: Record<string, string> = { ...PROJECT_LABELS }
