import type { ReactNode } from 'react'

/** 各画面の見出し。subtitle は ID などの補足。actions は右端のボタン。 */
export function PageTitle({ title, subtitle, actions }: { title: string; subtitle?: string; actions?: ReactNode }) {
  return (
    <div className="flex flex-wrap items-start justify-between gap-3">
      <div>
        <h1 className="text-lg font-semibold">{title}</h1>
        {subtitle ? <p className="text-muted-foreground font-mono text-sm">{subtitle}</p> : null}
      </div>
      {actions ? <div className="flex gap-2">{actions}</div> : null}
    </div>
  )
}

export function Loading() {
  return (
    <p role="status" className="text-muted-foreground text-sm">
      読み込んでいます…
    </p>
  )
}
