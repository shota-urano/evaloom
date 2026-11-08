import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { intakeBlockCauseLabel } from './intake'

type Props = {
  cause: string
  busy: boolean
  /** 中止（POST /api/tasks/{id}/cancel、04 3.5 の blocked の経路）。 */
  onCancel: () => void
}

/**
 * Linear からの取り込みで保留したタスク（15-linear-intake 3.6・3.6.3）の確認。
 * 保留は Execution を持たないため続行（04 3.8）はできず、本人ができるのは中止だけ。
 * Issue を直して更新すると、次の照会で判定をやり直す（15 3.6.3）。
 */
export function HeldReview({ cause, busy, onCancel }: Props) {
  return (
    <Card className="border-amber-300" data-testid="held-review">
      <CardHeader>
        <CardTitle>
          <h2>取り込みの保留</h2>
        </CardTitle>
        <CardDescription>Linear の Issue から作ったタスクを、実行せずに保留しています。このタスクは続行できません。</CardDescription>
      </CardHeader>
      <CardContent className="grid gap-4">
        <dl className="grid gap-2 text-sm sm:grid-cols-[10rem_1fr]">
          <dt className="text-muted-foreground">保留の理由</dt>
          <dd data-testid="held-cause">
            {intakeBlockCauseLabel(cause)} <span className="text-muted-foreground font-mono text-xs">{cause}</span>
          </dd>
        </dl>
        <p className="text-sm">Linear の Issue を直して更新すると、次の照会で判定をやり直します。このタスクをやめる場合は中止してください。</p>
        <div className="flex flex-wrap gap-2">
          <Button type="button" variant="destructive" disabled={busy} onClick={onCancel}>
            中止する
          </Button>
        </div>
      </CardContent>
    </Card>
  )
}
