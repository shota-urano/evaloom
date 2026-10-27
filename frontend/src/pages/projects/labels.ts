import type { TimeoutKey } from '@/api/registry'

// Project の各項目の表示名（13 3.2「Project 登録・編集」、02-data-model 3.2 projects）。
export const PROJECT_LABELS = {
  id: 'Project ID',
  name: '名称',
  localRepoPath: 'ローカルリポジトリのパス',
  gitRemoteUrl: 'Git リポジトリ URL',
  runnerId: 'Runner',
  baseBranch: 'Base Branch',
  defaultRole: '既定の役割',
  testCommand: 'テストコマンド',
  testReportFormat: 'テストレポートの形式',
  testReportPath: 'テストレポートの出力パス',
  setupCommand: 'セットアップコマンド',
  copyAllowlist: 'コピー許可リスト',
  testFilePatterns: 'テストファイルの追加パターン',
  allowedOperations: '許可操作',
  approvalPolicy: '承認ポリシー',
  contextLocation: 'Project Context',
} as const

export const TIMEOUT_LABELS: Record<TimeoutKey, string> = {
  setupSec: 'セットアップのタイムアウト（秒）',
  harnessSec: 'ハーネス実行のタイムアウト（秒）',
  testSec: 'テストのタイムアウト（秒）',
}

/** 422 のパス検証（internal/registry/repository.go）が返す details[].field → フォームの項目。 */
export const REPOSITORY_CHECK_FIELDS: Record<string, string> = {
  path: 'localRepoPath',
  url: 'gitRemoteUrl',
  baseBranch: 'baseBranch',
  runnerId: 'runnerId',
  worktreeRoot: 'runnerId',
}

/** 422 の一覧で details[].field を画面の名前で出すための表。 */
export const REPOSITORY_CHECK_LABELS: Record<string, string> = {
  path: PROJECT_LABELS.localRepoPath,
  url: PROJECT_LABELS.gitRemoteUrl,
  baseBranch: PROJECT_LABELS.baseBranch,
  runnerId: PROJECT_LABELS.runnerId,
  worktreeRoot: 'Runner の Worktree Root',
}
