import { useEffect, useId, useRef, type ReactNode } from 'react'

import { Button } from '@/components/ui/button'

type Props = {
  title: string
  children: ReactNode
  confirmLabel: string
  cancelLabel?: string
  /** 送信中など、どちらのボタンも押せないとき。 */
  busy?: boolean
  /** 開いたときにフォーカスを置くボタン。取り返しの付きにくい操作では cancel にする。 */
  initialFocus?: 'confirm' | 'cancel'
  onConfirm: () => void
  onCancel: () => void
}

/** 画面の上に重ねて出す確認ダイアログ。Escape は「やめる」と同じ。 */
export function ConfirmDialog({
  title,
  children,
  confirmLabel,
  cancelLabel = 'やめる',
  busy = false,
  initialFocus = 'confirm',
  onConfirm,
  onCancel,
}: Props) {
  const id = useId()
  const confirm = useRef<HTMLButtonElement>(null)
  const cancel = useRef<HTMLButtonElement>(null)
  useEffect(() => (initialFocus === 'cancel' ? cancel : confirm).current?.focus(), [initialFocus])
  return (
    <div className="fixed inset-0 z-50 grid place-items-center bg-black/40 p-4">
      <div
        role="alertdialog"
        aria-modal="true"
        aria-labelledby={`${id}-title`}
        aria-describedby={`${id}-description`}
        className="bg-background grid w-full max-w-md gap-4 rounded-xl border p-6 shadow-lg"
        onKeyDown={(event) => {
          if (event.key === 'Escape') onCancel()
        }}
      >
        <h2 id={`${id}-title`} className="font-semibold">
          {title}
        </h2>
        <div id={`${id}-description`} className="text-muted-foreground grid gap-2 text-sm">
          {children}
        </div>
        <div className="flex justify-end gap-2">
          <Button ref={cancel} type="button" variant="outline" onClick={onCancel} disabled={busy}>
            {cancelLabel}
          </Button>
          <Button ref={confirm} type="button" onClick={onConfirm} disabled={busy}>
            {confirmLabel}
          </Button>
        </div>
      </div>
    </div>
  )
}
