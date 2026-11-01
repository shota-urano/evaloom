import { BLOCK_CAUSE_GATES, type ExecutionDetail, type GateResult } from '@/api/tasks'
import type { BlockedEvent } from '@/hooks/use-execution-events'

// Blocked の確認（13 3.4）で表示する内容の組み立て。

/** 02-data-model 5.6 の Block 原因の意味。 */
export const BLOCK_CAUSE_LABELS: Record<string, string> = {
  gate2_missing_tests: 'Gate 2 で既存テストの欠けを検出した',
  gate2_unverifiable: 'テストの件数・ID が取れず検証できない',
  gate3_mismatch: '検証 SHA と HEAD の不一致、または未コミットの変更がある',
  context_conflict: '解決できない Context の矛盾がある',
  git_state: '再実行前の Git 状態が想定と違う（PR がクローズ済みなど）',
}

/** 成果物の一覧（task-detail-page の ArtifactSource と同じ形）。 */
export type ArtifactRef = { id: string; kind: string }
export type ArtifactSource = (execution: ExecutionDetail) => readonly ArtifactRef[] | null

/**
 * 表示中の Block。
 * - blockId と cause は blocked イベント（02 5.3）から受け取る。
 * - headSha は Block を起こした Gate の結果（result=blocked）の targetSha。画面にはこの SHA を出し、送信にもこの SHA を使う。
 *   execution_blocks を返す本人用 API は仕様（01 5.1）に無いため、Gate によらない Block（context_conflict / git_state）では SHA を出せない。
 */
export type BlockView = { blockId: string; cause: string; gate: GateResult | null; headSha: string | null }

export function resolveBlock(execution: ExecutionDetail, event: BlockedEvent | undefined): BlockView | null {
  if (!event?.blockId || !event.cause) return null
  const gateNumber = BLOCK_CAUSE_GATES[event.cause]
  const gate =
    gateNumber === undefined
      ? null
      : ([...execution.gateResults]
          .filter((g) => g.gate === gateNumber && g.result === 'blocked')
          .sort((a, b) => a.createdAt.localeCompare(b.createdAt))
          .at(-1) ?? null)
  return { blockId: event.blockId, cause: event.cause, gate, headSha: gate?.targetSha ?? null }
}
