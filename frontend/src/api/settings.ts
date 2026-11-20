import type { ApiClient } from './client'

// 全体設定（13-management-ui 3.2「Settings」）。
//
// GET/PATCH /api/settings は 02 3.8 の6キーを扱う（backend/internal/api/linear.go・linear.md）。
// 同時実行の上限は 02 3.8 のキーではなく確定値（13 4節・02 4章）なので、値の取り出し方を settingsSource に閉じ込める。

/** 同時実行の上限（13 4節・04 4節・05 4節、requirements 8.7）。画面からは変更させない。 */
export type ConcurrencyLimits = {
  /** 全体で同時に動かせる Execution の数。 */
  global: number
  /** 同じ振り分け先（Project またはスペシャルハーネス）で同時に動かせる Execution の数。 */
  perProject: number
}

export type GlobalSettings = {
  concurrency: ConcurrencyLimits
}

/** 13 4節の確定値「全体2本・振り分け先ごとに1本」。正本は Control Plane の claim（04 3.3）。 */
export const CONCURRENCY_LIMITS: Readonly<ConcurrencyLimits> = Object.freeze({ global: 2, perProject: 1 })

export type SettingsSource = {
  /** 値の出どころ。画面に出して、API の値か仕様の確定値かを分かるようにする。 */
  origin: string
  load: (signal?: AbortSignal) => Promise<GlobalSettings>
}

/** 仕様の確定値を返す取り出し方（上限を返す API は仕様に無い）。 */
export const settingsSource: SettingsSource = {
  origin: '仕様の確定値（13-management-ui 4節）',
  load: async () => ({ concurrency: { ...CONCURRENCY_LIMITS } }),
}

/** 15-linear-intake 3.4・5章（Q48）。既定値は ai-ready と route:（02 3.8、Q120）。 */
export type LinearLabelSettings = { triggerLabel: string; routingLabelPrefix: string }

/** routing.proposer（02 3.8、Q111）。model が null なら提案を作らない。 */
export type RouteProposer = { microHarness: string; model: string | null }

/** 02 3.8 の routing.*。defaultRunnerId は未設定なら null。 */
export type RoutingSettings = { proposer: RouteProposer; defaultRunnerId: string | null }

/** 段の名前（02 4.1）。limits.stageTimeoutMaxSec の4段。 */
export const STAGES = ['prepare', 'work', 'check', 'publish'] as const
export type Stage = (typeof STAGES)[number]
export type StageTimeoutMaxSec = Record<Stage, number>

/**
 * GET / PATCH /api/settings の応答（backend/internal/api/linear.go・linear.md）。02 3.8 の6キーを
 * キーの接頭辞で入れ子にした形。行が無いキーは既定値が入る。
 */
export type SettingsView = {
  linear: LinearLabelSettings
  routing: RoutingSettings
  limits: { stageTimeoutMaxSec: StageTimeoutMaxSec }
  approval: { ttlSec: number }
}

/**
 * PATCH /api/settings の本文。02 3.8 のキーだけを持てる（未知のキーはサーバーが 400 にする）。
 * routing.proposer と limits.stageTimeoutMaxSec は値全体の置き換えなので、部分の形を許さない。
 */
export type SettingsPatch = {
  linear?: Partial<LinearLabelSettings>
  routing?: { proposer?: RouteProposer; defaultRunnerId?: string | null }
  limits?: { stageTimeoutMaxSec: StageTimeoutMaxSec }
  approval?: { ttlSec: number }
}

/** 02 3.8 の6キー（details[].field と同じ名前）。 */
export const SETTING_KEYS = [
  'linear.triggerLabel',
  'linear.routingLabelPrefix',
  'routing.proposer',
  'routing.defaultRunnerId',
  'limits.stageTimeoutMaxSec',
  'approval.ttlSec',
] as const
export type SettingKey = (typeof SETTING_KEYS)[number]

/**
 * 型の外から値が混ざっても、02 3.8 のキーだけを本文に写す（未知のキーは送らない）。
 * 置き換えの値（proposer・stageTimeoutMaxSec）も決まった項目だけを写す。
 */
export function settingsPatchBody(patch: SettingsPatch): SettingsPatch {
  const body: SettingsPatch = {}
  if (patch.linear) {
    const linear: Partial<LinearLabelSettings> = {}
    if (patch.linear.triggerLabel !== undefined) linear.triggerLabel = patch.linear.triggerLabel
    if (patch.linear.routingLabelPrefix !== undefined) linear.routingLabelPrefix = patch.linear.routingLabelPrefix
    if (Object.keys(linear).length > 0) body.linear = linear
  }
  if (patch.routing) {
    const routing: NonNullable<SettingsPatch['routing']> = {}
    if (patch.routing.proposer !== undefined) routing.proposer = { microHarness: patch.routing.proposer.microHarness, model: patch.routing.proposer.model }
    if (patch.routing.defaultRunnerId !== undefined) routing.defaultRunnerId = patch.routing.defaultRunnerId
    if (Object.keys(routing).length > 0) body.routing = routing
  }
  if (patch.limits?.stageTimeoutMaxSec) {
    const source = patch.limits.stageTimeoutMaxSec
    body.limits = { stageTimeoutMaxSec: { prepare: source.prepare, work: source.work, check: source.check, publish: source.publish } }
  }
  if (patch.approval?.ttlSec !== undefined) body.approval = { ttlSec: patch.approval.ttlSec }
  return body
}

export const settingsApi = {
  get: (api: ApiClient, signal?: AbortSignal) => api.get<SettingsView>('/api/settings', { signal }),
  /** 指定したキーだけを置き換える（省略したキーは残る）。1つでも拒否されたら何も変わらない。 */
  update: (api: ApiClient, patch: SettingsPatch) => api.patch<SettingsView>('/api/settings', settingsPatchBody(patch)),
}
