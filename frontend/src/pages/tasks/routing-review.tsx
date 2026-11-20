import type { Decision } from '@/api/decisions'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Link } from '@/lib/link'
import { decisionDetailPath } from '@/pages/decisions/labels'
import { routeDecisionOf } from './intake'

type Props = {
  taskId: string
  /** このタスクの開いている判断待ち。読み込み中は undefined。 */
  decisions: readonly Decision[] | undefined
  error: unknown
}

/**
 * 振り分けを待つタスク（status=routing、15-linear-intake 3.6.4）の案内。
 * 振り分けのラベルで振り分け先が決まらなかったタスクは、本人が判断待ち（kind=route）で承認するまで実行しない。
 * 判断待ちは提案が proposed か failed になったときに開く（15 3.6.4 手順5）ので、それまではリンクが無い。
 */
export function RoutingReview({ taskId, decisions, error }: Props) {
  const decision = routeDecisionOf(decisions, taskId)
  return (
    <Card className="border-violet-300" data-testid="routing-review">
      <CardHeader>
        <CardTitle>
          <h2>振り分けを待っています</h2>
        </CardTitle>
        <CardDescription>振り分けのラベルで振り分け先が決まらなかったため、振り分けの判断を待っています。承認するまで実行しません。</CardDescription>
      </CardHeader>
      <CardContent className="grid gap-3 text-sm">
        {decision ? (
          <p>
            <Link to={decisionDetailPath(decision.id)} className="font-medium underline underline-offset-4">
              振り分けの判断待ちを開く
            </Link>{' '}
            <span className="text-muted-foreground font-mono text-xs">{decision.id}</span>
          </p>
        ) : decisions === undefined ? (
          <p className="text-muted-foreground">{error ? '判断待ちを読めませんでした。' : '読み込み中…'}</p>
        ) : (
          <p data-testid="routing-pending">振り分けの提案を作っています。提案ができると判断待ちが開きます。</p>
        )}
        <p className="text-muted-foreground">Linear で振り分けのラベルを1つだけ付けて Issue を更新しても、次の照会でその振り分け先に決まります。</p>
      </CardContent>
    </Card>
  )
}
