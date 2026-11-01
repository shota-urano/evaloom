import type { ReactNode } from 'react'

import { registry } from '@/api/registry'
import { settingsSource, type SettingsSource } from '@/api/settings'
import { useSession } from '@/auth/session'
import { ApiErrorAlert } from '@/components/api-error-alert'
import { Loading, PageTitle } from '@/components/page'
import { RunnerTable } from '@/components/runner-table'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { useResource } from '@/hooks/use-resource'

function Row({ label, children, testId }: { label: string; children: ReactNode; testId?: string }) {
  return (
    <div className="grid gap-1 py-2 sm:grid-cols-[14rem_1fr] sm:gap-4" data-testid={testId}>
      <dt className="text-muted-foreground text-sm">{label}</dt>
      <dd className="text-sm">{children}</dd>
    </div>
  )
}

/** GET /api/settings の応答の形が仕様に無く、backend にも経路が無いため表示できない項目（13 3.2 の Settings）。 */
const UNAVAILABLE = ['全体の既定タイムアウト', '通知先', 'Global Context'] as const

/**
 * Settings（13 3.2）。同時実行の上限は表示だけで、変更する入力欄は置かない（13 4節）。
 * source はテストで差し替えられるようにする。
 */
export function SettingsPage({ source = settingsSource }: { source?: SettingsSource }) {
  const { api } = useSession()
  const settings = useResource((signal) => source.load(signal), 'settings')
  const runners = useResource((signal) => registry.listRunners(api, signal), 'runners')

  return (
    <div className="grid gap-6">
      <PageTitle title="Settings" />

      <Card>
        <CardHeader>
          <CardTitle>
            <h2>同時実行の上限</h2>
          </CardTitle>
          <CardDescription>表示のみです。画面からは変更できません。値の出どころ: {source.origin}</CardDescription>
        </CardHeader>
        <CardContent>
          <ApiErrorAlert error={settings.error} onReload={settings.reload} />
          {settings.data === undefined ? (
            settings.error ? null : <Loading />
          ) : (
            <dl className="divide-y">
              <Row label="全体" testId="concurrency-global">
                <span className="font-semibold tabular-nums">{settings.data.concurrency.global}</span> 本
              </Row>
              <Row label="同一プロジェクト" testId="concurrency-per-project">
                <span className="font-semibold tabular-nums">{settings.data.concurrency.perProject}</span> 本
              </Row>
            </dl>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>
            <h2>Runner 一覧</h2>
          </CardTitle>
        </CardHeader>
        <CardContent>
          <ApiErrorAlert error={runners.error} onReload={runners.reload} />
          {runners.data === undefined ? runners.error ? null : <Loading /> : <RunnerTable runners={runners.data} />}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>
            <h2>その他の全体設定</h2>
          </CardTitle>
          <CardDescription>全体設定を返す API（GET /api/settings）の応答の形が決まっていないため、次の項目はまだ表示できません。</CardDescription>
        </CardHeader>
        <CardContent>
          <dl className="divide-y">
            {UNAVAILABLE.map((label) => (
              <Row key={label} label={label}>
                <span className="text-muted-foreground">取得できません</span>
              </Row>
            ))}
          </dl>
        </CardContent>
      </Card>
    </div>
  )
}
