import type { Harness, HarnessProfile, Project } from '@/api/registry'
import type { TaskDestination } from '@/api/tasks'

// タスクの振り分け先（Project かスペシャルハーネスの一方、02 3.3・13 3.2）の、画面での扱い。
// 選択欄の値は「project:<id>」「harness:<id>」の形にし、1つの選択欄で必ずどちらか一方だけを選ばせる。

export type DestinationKind = 'project' | 'harness'
export type DestinationRef = { kind: DestinationKind; id: string }

const PREFIX: Record<DestinationKind, string> = { project: 'project:', harness: 'harness:' }

export function encodeDestination(ref: DestinationRef): string {
  return `${PREFIX[ref.kind]}${ref.id}`
}

export function decodeDestination(value: string): DestinationRef | null {
  for (const kind of ['project', 'harness'] as const) {
    if (value.startsWith(PREFIX[kind]) && value.length > PREFIX[kind].length) return { kind, id: value.slice(PREFIX[kind].length) }
  }
  return null
}

export function destinationInput(ref: DestinationRef): TaskDestination {
  return ref.kind === 'project' ? { projectId: ref.id } : { harnessId: ref.id }
}

/** タスクの振り分け先。応答では使わない側が空文字列（backend/internal/api/tasks.md）。 */
export function taskDestination(task: { projectId: string; harnessId?: string }): DestinationRef | null {
  if (task.projectId) return { kind: 'project', id: task.projectId }
  if (task.harnessId) return { kind: 'harness', id: task.harnessId }
  return null
}

export const DESTINATION_KIND_LABELS: Record<DestinationKind, string> = { project: 'Project', harness: 'スペシャルハーネス' }

export function destinationPath(ref: DestinationRef): string {
  return ref.kind === 'project' ? `/projects/${encodeURIComponent(ref.id)}` : `/harnesses/${encodeURIComponent(ref.id)}`
}

/**
 * 振り分け先の Profile の版（Project は profileId、スペシャルハーネスは既定の版 defaultProfileId）。
 * 見つからなければ null。
 */
export function destinationProfileId(ref: DestinationRef, projects: readonly Project[], harnesses: readonly Harness[]): string | null {
  if (ref.kind === 'project') return projects.find((p) => p.id === ref.id)?.profileId || null
  return harnesses.find((h) => h.id === ref.id)?.defaultProfileId ?? null
}

/**
 * 受け入れ条件が必須か（13 4節「受け入れ条件」、契約 5.2）。振り分け先の Profile が acceptanceCriteria: required のときだけ必須。
 * Profile の定義が読めないときは画面では必須にしない（正本はサーバーの検証、13 3.1）。
 */
export function acceptanceCriteriaRequired(profileId: string | null, profiles: readonly HarnessProfile[]): boolean {
  if (!profileId) return false
  return profiles.find((p) => p.id === profileId)?.definition?.acceptanceCriteria === 'required'
}
