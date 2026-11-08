import type { TaskSummary } from '@/api/tasks'

// Linear からの取り込みで保留したタスク（15-linear-intake 3.6・3.6.3）の表示。

/** tasks.intake_block_cause（15 3.6 の表と 5・6、3.8）の意味。文言は 15 3.6 の「状況」に合わせる。 */
export const INTAKE_BLOCK_CAUSE_LABELS: Readonly<Record<string, string>> = {
  routing_multiple_labels: '実行先ラベルが2つ以上ある',
  routing_unknown_label: '実行先ラベルが登録されていない',
  routing_team_mismatch: 'ラベルは登録済みだが、Issue の Team がその Project の Team と違う',
  routing_no_label: '実行先ラベルが1つも無い',
  missing_acceptance_criteria: '受け入れ条件が書かれていない',
  config_missing_assignment: 'Project の default_role に割り当てが無い',
}

/** 保留の理由を画面の文言にする。未知の値は原文のまま出す。 */
export function intakeBlockCauseLabel(cause: string): string {
  return INTAKE_BLOCK_CAUSE_LABELS[cause] ?? cause
}

/** 取り込みで保留しているタスク（Execution の無い blocked、15 3.6）なら保留の理由、そうでなければ null。保留は続行できず、中止だけができる（15 3.6.3、04 3.5）。 */
export function heldCause(task: Pick<TaskSummary, 'status' | 'intakeBlockCause'>): string | null {
  return task.status === 'blocked' && task.intakeBlockCause ? task.intakeBlockCause : null
}
