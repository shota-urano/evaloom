/** 判断待ちの画面（13 3.4）。詳細の URL は Linear のコメントと macOS 通知から開く `/decisions/<id>`。 */
export const DECISIONS_PATH = '/decisions'

export function decisionDetailPath(id: string) {
  return `${DECISIONS_PATH}/${encodeURIComponent(id)}`
}
