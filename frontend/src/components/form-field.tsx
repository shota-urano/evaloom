import { useId, type ComponentProps, type ReactNode } from 'react'

import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Textarea } from '@/components/ui/textarea'

/** 項目の下に出すエラー文言。入力欄とは aria-describedby で結びつける。 */
export function FieldError({ id, messages }: { id: string; messages?: readonly string[] }) {
  if (!messages || messages.length === 0) return null
  return (
    <div id={id} data-slot="field-error" className="text-destructive text-sm">
      {messages.map((message) => (
        <p key={message}>{message}</p>
      ))}
    </div>
  )
}

type FieldProps = {
  /** API の項目名（details[].field と同じ camelCase）。 */
  name: string
  label: string
  /** splitFieldErrors / useApiErrors で得た、この項目の文言。 */
  errors?: readonly string[]
  hint?: string
}

type ControlProps = {
  id: string
  name: string
  'aria-invalid'?: true
  'aria-describedby'?: string
}

function FieldShell({ name, label, errors, hint, control }: FieldProps & { control: (props: ControlProps) => ReactNode }) {
  const id = useId()
  const inputId = `${id}-input`
  const hintId = `${id}-hint`
  const errorId = `${id}-error`
  const invalid = errors !== undefined && errors.length > 0
  const describedBy = [hint ? hintId : null, invalid ? errorId : null].filter(Boolean).join(' ')
  return (
    <div className="grid gap-2" data-field={name}>
      <Label htmlFor={inputId}>{label}</Label>
      {control({
        id: inputId,
        name,
        'aria-invalid': invalid ? true : undefined,
        'aria-describedby': describedBy === '' ? undefined : describedBy,
      })}
      {hint ? (
        <p id={hintId} className="text-muted-foreground text-xs">
          {hint}
        </p>
      ) : null}
      <FieldError id={errorId} messages={errors} />
    </div>
  )
}

/** ラベル・1行入力・項目ごとのエラー表示をまとめたフォーム項目。 */
export function TextField({ name, label, errors, hint, ...props }: FieldProps & Omit<ComponentProps<'input'>, 'name' | 'id'>) {
  return (
    <FieldShell
      name={name}
      label={label}
      errors={errors}
      hint={hint}
      control={(control) => <Input {...props} {...control} />}
    />
  )
}

/** 複数行入力版の TextField。 */
export function TextAreaField({
  name,
  label,
  errors,
  hint,
  ...props
}: FieldProps & Omit<ComponentProps<'textarea'>, 'name' | 'id'>) {
  return (
    <FieldShell
      name={name}
      label={label}
      errors={errors}
      hint={hint}
      control={(control) => <Textarea {...props} {...control} />}
    />
  )
}
