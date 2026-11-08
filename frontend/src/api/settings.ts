import type { ApiClient } from './client'

// 全体設定（13-management-ui 3.2「Settings」）。
//
// GET/PATCH /api/settings は 15-linear-intake 5章の linear（トリガーのラベル名と実行先ラベルの接頭辞）だけを返す
// （backend/internal/api/linear.go・linear.md）。同時実行の上限などほかの全体設定は応答の形が仕様に無いので、
// 画面はこの型だけに依存し、値の取り出し方は settingsSource に閉じ込める。

/** 同時実行の上限（13 4節・04 4節・05 4節、requirements 8.7）。画面からは変更させない。 */
export type ConcurrencyLimits = {
  /** 全体で同時に動かせる Execution の数。 */
  global: number
  /** 同じプロジェクトで同時に動かせる Execution の数。 */
  perProject: number
}

export type GlobalSettings = {
  concurrency: ConcurrencyLimits
}

/** 13 4節の確定値「全体2本・同一プロジェクト1本」。正本は Control Plane の claim（04 3.3）。 */
export const CONCURRENCY_LIMITS: Readonly<ConcurrencyLimits> = Object.freeze({ global: 2, perProject: 1 })

export type SettingsSource = {
  /** 値の出どころ。画面に出して、API の値か仕様の確定値かを分かるようにする。 */
  origin: string
  load: (signal?: AbortSignal) => Promise<GlobalSettings>
}

/** GET /api/settings が無い間の取り出し方。仕様の確定値を返す。 */
export const settingsSource: SettingsSource = {
  origin: '仕様の確定値（13-management-ui 4節）',
  load: async () => ({ concurrency: { ...CONCURRENCY_LIMITS } }),
}

/** 15-linear-intake 3.4・5章（Q48）。既定値は ai-ready と repo:。 */
export type LinearLabelSettings = { triggerLabel: string; routingLabelPrefix: string }

/** GET / PATCH /api/settings の応答（backend/internal/api/linear.go の settingsView）。 */
export type SettingsView = { linear: LinearLabelSettings }

export const settingsApi = {
  get: (api: ApiClient, signal?: AbortSignal) => api.get<SettingsView>('/api/settings', { signal }),
  /** linear の中の指定したキーだけを置き換える（省略したキーは残る）。 */
  updateLinear: (api: ApiClient, linear: Partial<LinearLabelSettings>) => api.patch<SettingsView>('/api/settings', { linear }),
}
