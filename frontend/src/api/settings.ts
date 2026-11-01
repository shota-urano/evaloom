// 全体設定（13-management-ui 3.2「Settings」）。
//
// 01-architecture 5.1 には GET/PATCH /api/settings があるが、応答の形は仕様に無く、backend にも実装が無い
// （backend/internal/api/server.go に経路が無い）。画面はこの型だけに依存し、値の取り出し方は settingsSource に閉じ込める。
// GET /api/settings の形が決まったら、settingsSource をその API を読む実装に差し替える。

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
