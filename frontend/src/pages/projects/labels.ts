/**
 * 登録画面のパス。Project ID は slug（^[a-z0-9]+(-[a-z0-9]+)*$、backend/internal/api/validation.go）なので、
 * slug に使えない「_」を含めて、どの Project の詳細（/projects/:id）とも衝突しないようにする。
 */
export const PROJECT_NEW_PATH = '/projects/_new'

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
  setupAllowedHosts: 'setup の許可宛先',
  copyAllowlist: 'コピー許可リスト',
  allowedOperations: '許可操作',
  approvalPolicy: '承認ポリシー',
  contextLocation: 'Project Context',
  // 15-linear-intake 3.8（requirements 5.1 の Linear Team ID・Linear Routing Label）
  linearTeamId: 'Linear Team ID',
  linearRoutingLabel: '実行先ラベル',
} as const

/** approvalPolicy.mergePullRequest の値（17 3.2）。 */
export type MergePullRequestPolicy = 'require_approval' | 'direct_push'

/** 承認ポリシーの選択肢。値の意味は 17 3.2 の表の文言。 */
export const APPROVAL_POLICY_LABELS: Record<MergePullRequestPolicy, string> = {
  require_approval: 'require_approval（既定）：PR を作り、本人の承認の後に Evaloom がマージする',
  direct_push: 'direct_push：PR を作らず、Gate 2・3 を通った commit をベースブランチへ fast-forward で push する',
}

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

/** 409 の Linear Team ID と実行先ラベルの組の重複（details は linearRoutingLabel / duplicate）→ フォームの項目。 */
export const LINEAR_CONFLICT_FIELDS: Record<string, string> = {
  linearTeamId: 'linearTeamId',
  linearRoutingLabel: 'linearRoutingLabel',
}

/** 422・409 の一覧で details[].field を画面の名前で出すための表。 */
export const REPOSITORY_CHECK_LABELS: Record<string, string> = {
  linearTeamId: PROJECT_LABELS.linearTeamId,
  linearRoutingLabel: PROJECT_LABELS.linearRoutingLabel,
  path: PROJECT_LABELS.localRepoPath,
  url: PROJECT_LABELS.gitRemoteUrl,
  baseBranch: PROJECT_LABELS.baseBranch,
  runnerId: PROJECT_LABELS.runnerId,
  worktreeRoot: 'Runner の Worktree Root',
}
