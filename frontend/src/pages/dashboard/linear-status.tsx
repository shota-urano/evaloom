import type { ReactNode } from 'react'

import { decisionLabel, isPollFailing, LINEAR_FAILURE_ALERT_THRESHOLD, pollErrorLabel, type LinearStatus, type LinearTeamStatus } from '@/api/linear'
import { ApiErrorAlert } from '@/components/api-error-alert'
import { Loading } from '@/components/page'

// Dashboard の Linear の取り込みの状況（15-linear-intake 5章 GET /api/linear/status・6章）。

const time = (value: string | null, empty = 'なし') =>
  value ? <time dateTime={value}>{new Date(value).toLocaleString('ja-JP')}</time> : <span className="text-muted-foreground">{empty}</span>

const pollError = (error: string | null) =>
  error === null ? (
    <span className="text-muted-foreground">なし</span>
  ) : (
    <>
      {pollErrorLabel(error)}
      <span className="text-muted-foreground font-mono text-xs">（{error}）</span>
    </>
  )

function Th({ children }: { children: ReactNode }) {
  return (
    <th scope="col" className="px-4 py-2 font-medium whitespace-nowrap">
      {children}
    </th>
  )
}

/**
 * エラー状況のうち Linear の照会の失敗（15 6章）。続けて失敗した回数が上限（5回）に達した Team を、
 * 失敗の分類と最後に成功した時刻つきで出す。
 */
export function LinearPollFailures({ teams, error }: { teams: LinearTeamStatus[] | undefined; error: unknown }) {
  const failing = (teams ?? []).filter(isPollFailing)
  return (
    <div className="grid gap-2" data-testid="linear-poll-failures">
      <h3 className="text-sm font-semibold">Linear の照会の失敗</h3>
      <p className="text-muted-foreground text-xs">{LINEAR_FAILURE_ALERT_THRESHOLD} 回以上続けて照会に失敗している Linear Team を表示します。</p>
      <ApiErrorAlert error={error} />
      {teams === undefined ? (
        error ? null : <Loading />
      ) : failing.length === 0 ? (
        <p className="text-muted-foreground text-sm">続けて失敗している照会はありません。</p>
      ) : (
        <ul className="divide-y" aria-label="Linear の照会の失敗">
          {failing.map((team) => (
            <li key={team.teamId} className="grid gap-0.5 py-2 text-sm">
              <div className="font-medium">
                Team <span className="font-mono">{team.teamId}</span>
              </div>
              <div>
                {pollError(team.lastError)} · {team.consecutiveFailures} 回続けて失敗
              </div>
              <div className="text-muted-foreground text-xs">最後に成功した時刻: {time(team.lastSuccessAt, 'まだ成功していません')}</div>
              {team.lastError === 'auth' ? (
                <div className="text-muted-foreground text-xs">
                  API キーのファイル（<span className="font-mono">~/.config/evaloom/linear-api-key</span>、権限 0600）を確認してください。
                </div>
              ) : null}
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

/** Linear の取り込みの状況。照会の対象の Team ごとの状況と、トリガーのラベルが付いている Issue の最後の判定。 */
export function LinearStatusSection({ status, error, onReload }: { status: LinearStatus | undefined; error: unknown; onReload: () => void }) {
  return (
    <section aria-labelledby="dashboard-linear" className="grid gap-3" data-testid="linear-status">
      <h2 id="dashboard-linear" className="font-semibold">
        Linear の取り込み
      </h2>
      <ApiErrorAlert error={error} onReload={onReload} />
      {status === undefined ? (
        error ? null : <Loading />
      ) : (
        <>
          {status.teams.length === 0 ? (
            <p className="text-muted-foreground text-sm">照会の対象の Linear Team はありません。プロジェクトに Linear Team ID を登録すると照会します。</p>
          ) : (
            <div className="overflow-x-auto rounded-xl border">
              <table className="w-full text-sm">
                <caption className="sr-only">Linear Team の照会の状況</caption>
                <thead className="bg-muted/50 text-left">
                  <tr>
                    <Th>Team ID</Th>
                    <Th>最後に成功した時刻</Th>
                    <Th>失敗の分類</Th>
                    <Th>続けて失敗した回数</Th>
                  </tr>
                </thead>
                <tbody>
                  {status.teams.map((team) => (
                    <tr key={team.teamId} className="border-t align-top" data-team={team.teamId}>
                      <td className="px-4 py-3 font-mono">{team.teamId}</td>
                      <td className="px-4 py-3 text-xs whitespace-nowrap">{time(team.lastSuccessAt)}</td>
                      <td className="px-4 py-3">{pollError(team.lastError)}</td>
                      <td className="px-4 py-3 tabular-nums">{team.consecutiveFailures} 回</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          {status.issues.length === 0 ? (
            <p className="text-muted-foreground text-sm">トリガーのラベルが付いている Issue はありません。</p>
          ) : (
            <div className="overflow-x-auto rounded-xl border">
              <table className="w-full text-sm">
                <caption className="sr-only">トリガーのラベルが付いている Issue</caption>
                <thead className="bg-muted/50 text-left">
                  <tr>
                    <Th>Issue</Th>
                    <Th>Team ID</Th>
                    <Th>最後の判定</Th>
                    <Th>判定の時刻</Th>
                  </tr>
                </thead>
                <tbody>
                  {status.issues.map((issue) => (
                    <tr key={issue.linearIssueId} className="border-t align-top">
                      <td className="px-4 py-3 font-mono">{issue.issueIdentifier}</td>
                      <td className="px-4 py-3 font-mono">{issue.teamId}</td>
                      <td className="px-4 py-3">
                        {issue.lastDecision === null ? (
                          <span className="text-muted-foreground">なし</span>
                        ) : (
                          <>
                            {decisionLabel(issue.lastDecision)}
                            <span className="text-muted-foreground font-mono text-xs">（{issue.lastDecision}）</span>
                          </>
                        )}
                      </td>
                      <td className="px-4 py-3 text-xs whitespace-nowrap">{time(issue.lastDecisionAt)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </>
      )}
    </section>
  )
}
