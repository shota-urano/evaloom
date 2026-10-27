import { useCallback, useMemo, useState } from 'react'

import { splitFieldErrors } from '@/api/errors'

/**
 * フォームの送信エラーを持ち、400 の details を項目ごとに分ける。
 * fields には、そのフォームが表示している API の項目名を渡す。
 */
export function useApiErrors(fields: readonly string[]) {
  const [error, setError] = useState<unknown>(null)
  const key = fields.join('\n')
  const { fieldErrors, unmatched } = useMemo(
    () => splitFieldErrors(error, key.split('\n')),
    [error, key],
  )
  const clear = useCallback(() => setError(null), [])
  return { error, setError, clear, fieldErrors, unmatched }
}
