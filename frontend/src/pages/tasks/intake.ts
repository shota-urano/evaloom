import type { Decision } from '@/api/decisions'
import type { TaskSummary } from '@/api/tasks'

// Linear から取り込んだタスクの、振り分けを待つ（routing）・保留（intake_block_cause）の表示（15-linear-intake 3.6・3.6.3・3.6.4）。

/**
 * tasks.intake_block_cause（02-data-model 5.7）の意味。保留の理由はこの2つだけで、
 * 振り分けが決まらないときは保留ではなく振り分けの提案に回す（15 3.6.2）。
 */
export const INTAKE_BLOCK_CAUSE_LABELS: Readonly<Record<string, string>> = {
  missing_acceptance_criteria: '振り分け先の版が受け入れ条件を求めているのに、Issue に受け入れ条件が無い',
  config_missing_profile: 'スペシャルハーネスへ直接振り分けたが、そのスペシャルハーネスに既定の版が無い',
}

/** 保留を解く方法（02 5.7 の「解除」、15 3.6.3）。未知の値には一般的な案内を出す。 */
export const INTAKE_BLOCK_CAUSE_HINTS: Readonly<Record<string, string>> = {
  missing_acceptance_criteria: 'Linear の Issue の本文に「受け入れ条件」の見出しで受け入れ条件を書いて更新すると、次の照会で判定をやり直します。',
  config_missing_profile: 'このスペシャルハーネスに既定の版を設定すると、次の照会で判定をやり直します。',
}
const DEFAULT_HINT = '保留の理由が解消されると、次の照会で判定をやり直します。'

/** 保留の理由を画面の文言にする。未知の値は原文のまま出す。 */
export function intakeBlockCauseLabel(cause: string): string {
  return INTAKE_BLOCK_CAUSE_LABELS[cause] ?? cause
}

/** 保留を解く方法の案内。 */
export function intakeBlockCauseHint(cause: string): string {
  return INTAKE_BLOCK_CAUSE_HINTS[cause] ?? DEFAULT_HINT
}

/** 取り込みで保留しているタスク（Execution の無い blocked、15 3.6）なら保留の理由、そうでなければ null。保留は続行できず、中止だけができる（15 3.6.3、04 3.5）。 */
export function heldCause(task: Pick<TaskSummary, 'status' | 'intakeBlockCause'>): string | null {
  return task.status === 'blocked' && task.intakeBlockCause ? task.intakeBlockCause : null
}

/** このタスクの開いている振り分けの判断待ち（kind=route、15 3.6.4 手順5）。一覧は新しい順なので最初の1件。 */
export function routeDecisionOf(items: readonly Decision[] | undefined, taskId: string): Decision | undefined {
  return items?.find((decision) => decision.kind === 'route' && decision.taskId === taskId && decision.status === 'open')
}
