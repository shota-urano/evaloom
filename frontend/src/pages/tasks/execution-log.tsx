import { useLayoutEffect, useRef, useState } from 'react'

import type { ApiClient } from '@/api/client'
import type { StreamState } from '@/api/event-stream'
import { FINAL_EXECUTION_STATUSES, LOG_STREAMS, tasks, type LogStream } from '@/api/tasks'
import { ApiErrorAlert } from '@/components/api-error-alert'
import { Loading } from '@/components/page'
import { useResource } from '@/hooks/use-resource'
import { cn } from '@/lib/utils'
import { savedLines, useLiveLog } from './live-log'

const STATE_LABELS: Record<StreamState, string> = {
  connecting: '接続しています…',
  open: '受信中（実行中のログを追記します）',
  reconnecting: '切断されました。再接続しています…',
  closed: '受信を停止しました',
}

/**
 * 選んだ Execution のログ（13 3.3）。実行中は SSE で追記し、終了後は保存済みログ（GET /api/executions/{id}/logs）を表示する。
 * どちらで読むかは表示し始めたときの状態で決める。表示中に終了しても SSE の受信は続け、届いたログを最後まで足す。
 * stream（harness / setup / test / runner）で切り替える。
 */
export function ExecutionLog({ api, executionId, status }: { api: ApiClient; executionId: string; status: string }) {
  const [live] = useState(() => !FINAL_EXECUTION_STATUSES.has(status))
  const [stream, setStream] = useState<LogStream>('harness')
  const liveLog = useLiveLog(api, executionId, live)
  const saved = useResource((signal) => (live ? Promise.resolve(undefined) : tasks.getLogs(api, executionId, signal)), `logs:${executionId}:${live}`)
  const lines = live ? liveLog.lines : saved.data ? savedLines(saved.data) : []
  const shown = lines.filter((line) => line.stream === stream)

  // 末尾を見ているときだけ、追記に合わせて末尾へ送る。
  const box = useRef<HTMLPreElement>(null)
  const atBottom = useRef(true)
  useLayoutEffect(() => {
    if (box.current && atBottom.current) box.current.scrollTop = box.current.scrollHeight
  }, [shown.length, stream])

  return (
    <div className="grid gap-3">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div role="tablist" aria-label="ログの stream" className="bg-muted inline-flex rounded-lg p-1">
          {LOG_STREAMS.map((name) => {
            const count = lines.filter((line) => line.stream === name).length
            return (
              <button
                key={name}
                type="button"
                role="tab"
                aria-selected={stream === name}
                aria-controls="execution-log"
                onClick={() => setStream(name)}
                className={cn(
                  'rounded-md px-3 py-1 font-mono text-xs',
                  stream === name ? 'bg-background font-medium shadow-xs' : 'text-muted-foreground hover:text-foreground',
                )}
              >
                {name} <span className="text-muted-foreground">({count})</span>
              </button>
            )
          })}
        </div>
        <p className="text-muted-foreground text-xs" data-testid="log-source">
          {live ? STATE_LABELS[liveLog.state.value] : '保存済みのログを表示しています'}
        </p>
      </div>
      {live && liveLog.state.value === 'closed' ? <ApiErrorAlert error={liveLog.state.error} /> : null}
      {!live && saved.error ? <ApiErrorAlert error={saved.error} onReload={saved.reload} /> : null}
      {!live && saved.data === undefined && !saved.error ? (
        <Loading />
      ) : (
        <pre
          id="execution-log"
          ref={box}
          role="log"
          aria-label={`ログ（${stream}）`}
          onScroll={(event) => {
            const el = event.currentTarget
            atBottom.current = el.scrollHeight - el.scrollTop - el.clientHeight < 16
          }}
          className="bg-muted/50 max-h-96 min-h-24 overflow-auto rounded-md border p-3 font-mono text-xs leading-relaxed whitespace-pre-wrap break-all"
        >
          {shown.length === 0 ? (
            <span className="text-muted-foreground">{live ? 'まだログはありません。' : 'この stream の保存済みログはありません。'}</span>
          ) : (
            shown.map((line) => (
              <div key={line.key} data-log-line>
                {line.text}
              </div>
            ))
          )}
        </pre>
      )}
    </div>
  )
}
