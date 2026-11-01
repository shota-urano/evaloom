import type { Runner } from '@/api/registry'
import { cn } from '@/lib/utils'

/** 02-data-model 3.1 runners.status の表示名。状態は色だけでなく文字でも示す（13 3.5）。 */
const RUNNER_STATUS_LABELS: Record<string, string> = { online: 'online', offline: 'offline' }

const TONES: Record<string, string> = {
  online: 'border-emerald-300 bg-emerald-50 text-emerald-900',
  offline: 'border-zinc-300 bg-zinc-100 text-zinc-700',
}

export function RunnerStatusBadge({ status }: { status: string }) {
  return (
    <span
      data-status={status}
      className={cn('inline-flex rounded-md border px-2 py-0.5 text-xs font-medium whitespace-nowrap', TONES[status] ?? 'bg-muted text-foreground')}
    >
      {RUNNER_STATUS_LABELS[status] ?? status}
    </span>
  )
}

const time = (value: string | null) =>
  value ? <time dateTime={value}>{new Date(value).toLocaleString('ja-JP')}</time> : <span className="text-muted-foreground">なし</span>

/** Runner 一覧（GET /api/runners）。Dashboard と Settings で使う。 */
export function RunnerTable({ runners }: { runners: readonly Runner[] }) {
  if (runners.length === 0) {
    return <p className="text-muted-foreground text-sm">Runner は登録されていません。`evaloomd runner register` で登録してください。</p>
  }
  return (
    <div className="overflow-x-auto rounded-xl border">
      <table className="w-full text-sm">
        <caption className="sr-only">Runner</caption>
        <thead className="bg-muted/50 text-left">
          <tr>
            <th scope="col" className="px-4 py-2 font-medium whitespace-nowrap">
              Runner
            </th>
            <th scope="col" className="px-4 py-2 font-medium whitespace-nowrap">
              状態
            </th>
            <th scope="col" className="px-4 py-2 font-medium whitespace-nowrap">
              最後の heartbeat
            </th>
          </tr>
        </thead>
        <tbody>
          {runners.map((runner) => (
            <tr key={runner.id} className="border-t align-top" data-runner={runner.id}>
              <td className="px-4 py-3">
                <div className="font-medium">{runner.name}</div>
                <div className="text-muted-foreground font-mono text-xs">{runner.id}</div>
              </td>
              <td className="px-4 py-3">
                <RunnerStatusBadge status={runner.status} />
              </td>
              <td className="px-4 py-3 text-xs whitespace-nowrap">{time(runner.lastHeartbeatAt)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}
