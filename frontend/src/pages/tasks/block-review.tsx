import { useState } from 'react'

import type { ApiClient } from '@/api/client'
import { ApiError } from '@/api/client'
import { CONTINUABLE_BLOCK_CAUSES, tasks, type ExecutionDetail, type GateResult, type ResumeDecision } from '@/api/tasks'
import { ApiErrorAlert } from '@/components/api-error-alert'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import type { BlockedEvent } from '@/hooks/use-execution-events'
import { useResource } from '@/hooks/use-resource'
import { BLOCK_CAUSE_LABELS, resolveBlock, type ArtifactSource } from './block'

function Findings({ gate }: { gate: GateResult | null }) {
  if (!gate) return <p className="text-muted-foreground text-sm">この Block に対応する Gate の結果はありません。</p>
  if (gate.findings.length === 0) return <p className="text-muted-foreground text-sm">Finding はありません。</p>
  return (
    <ul className="grid gap-1 text-sm" aria-label="Finding">
      {gate.findings.map((finding, index) => (
        <li key={index}>
          <span className="font-mono text-xs">
            {finding.path || '(ファイルなし)'}
            {finding.line > 0 ? `:${finding.line}` : ''}
          </span>{' '}
          <span className="text-muted-foreground font-mono text-xs">[{finding.rule}]</span> {finding.message}
        </li>
      ))}
    </ul>
  )
}

function Diff({ api, execution, source }: { api: ApiClient; execution: ExecutionDetail; source: ArtifactSource }) {
  const artifacts = source(execution)
  const diff = artifacts?.find((a) => a.kind === 'diff')
  const text = useResource(
    (signal) => (diff ? tasks.getArtifactText(api, execution.id, diff.id, signal) : Promise.resolve(undefined)),
    `diff:${execution.id}:${diff?.id ?? ''}`,
  )
  if (artifacts === null) {
    return <p className="text-muted-foreground text-sm">成果物（差分）の一覧を返す API が無いため、ここには差分を出せません。</p>
  }
  if (!diff) return <p className="text-muted-foreground text-sm">差分はありません。</p>
  if (text.error) return <ApiErrorAlert error={text.error} />
  if (text.data === undefined) return <p className="text-muted-foreground text-sm">差分を読み込んでいます…</p>
  return (
    <pre aria-label="差分" className="bg-muted max-h-96 overflow-auto rounded-md p-3 font-mono text-xs whitespace-pre">
      {text.data}
    </pre>
  )
}

type Props = {
  api: ApiClient
  taskId: string
  execution: ExecutionDetail
  event: BlockedEvent | undefined
  artifactSource: ArtifactSource
  /** 送信が受け付けられたとき。 */
  onDecided: (message: string) => void
  /** タスクと Execution を読み直す。 */
  onReload: () => void
}

/** Blocked の確認と続行・中止（13 3.4、04 3.8）。 */
export function BlockReview({ api, taskId, execution, event, artifactSource, onDecided, onReload }: Props) {
  const block = resolveBlock(execution, event)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>(null)
  const [stale, setStale] = useState(false)

  async function decide(decision: ResumeDecision) {
    if (busy || !block?.headSha) return
    setBusy(true)
    setError(null)
    setStale(false)
    try {
      // 画面に表示している HEAD SHA を送る（13 3.4 の2）。
      await tasks.resume(api, taskId, { blockId: block.blockId, decision, headSha: block.headSha })
      onDecided(decision === 'continue' ? '続行を受け付けました。タスクは Queued に戻り、Gate 3 から再開します。' : '中止を受け付けました。')
    } catch (cause) {
      // SHA 不一致（422）は内容が変わったことを示すので、読み直させる（13 3.4 の3）。
      if (cause instanceof ApiError && cause.status === 422) setStale(true)
      else setError(cause)
      if (cause instanceof ApiError && cause.status === 409) onReload()
    } finally {
      setBusy(false)
    }
  }

  const continuable = block !== null && CONTINUABLE_BLOCK_CAUSES.has(block.cause)

  return (
    <Card className="border-amber-300">
      <CardHeader>
        <CardTitle>
          <h2>Blocked の確認（#{execution.seq}）</h2>
        </CardTitle>
        <CardDescription>変更内容を確認し、続行か中止を選んでください。続行を選んだ場合だけ先へ進みます。</CardDescription>
      </CardHeader>
      <CardContent className="grid gap-4">
        {stale ? (
          <Alert variant="warning" data-testid="block-stale">
            <AlertTitle>内容が変わりました</AlertTitle>
            <AlertDescription>
              <p>表示していた HEAD SHA が、記録されている SHA と一致しませんでした。画面を読み直して、変更内容を確認し直してください。</p>
              <Button
                type="button"
                variant="outline"
                size="sm"
                className="mt-2"
                onClick={() => {
                  setStale(false)
                  onReload()
                }}
              >
                画面を読み直す
              </Button>
            </AlertDescription>
          </Alert>
        ) : null}
        <ApiErrorAlert error={error} onReload={onReload} />
        {block === null ? (
          <p role="status" className="text-muted-foreground text-sm">
            Block の情報を受信しています…
          </p>
        ) : (
          <>
            <dl className="grid gap-2 text-sm sm:grid-cols-[10rem_1fr]">
              <dt className="text-muted-foreground">原因</dt>
              <dd>
                <span className="font-mono text-xs">{block.cause}</span> {BLOCK_CAUSE_LABELS[block.cause] ?? ''}
              </dd>
              <dt className="text-muted-foreground">確認する HEAD SHA</dt>
              <dd className="font-mono text-xs break-all" data-testid="block-head-sha">
                {block.headSha ?? '表示できません（この原因の Block の HEAD SHA を返す API がありません）'}
              </dd>
            </dl>
            <section className="grid gap-2">
              <h3 className="text-sm font-medium">Finding</h3>
              <Findings gate={block.gate} />
            </section>
            <section className="grid gap-2">
              <h3 className="text-sm font-medium">差分</h3>
              <Diff api={api} execution={execution} source={artifactSource} />
            </section>
            {continuable ? (
              block.headSha ? (
                <div className="flex flex-wrap gap-2">
                  <Button type="button" disabled={busy} onClick={() => decide('continue')}>
                    続行する
                  </Button>
                  <Button type="button" variant="destructive" disabled={busy} onClick={() => decide('abort')}>
                    中止する
                  </Button>
                </div>
              ) : null
            ) : (
              <p className="text-sm">この原因の Block は続行できません。原因を直してから、上の「停止（中止）」か「再実行」を選んでください。</p>
            )}
          </>
        )}
      </CardContent>
    </Card>
  )
}
