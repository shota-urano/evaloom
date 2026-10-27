import type { ComponentProps, MouseEvent } from 'react'

import { navigate } from './router'

/** 同じタブ内の遷移は navigate で行い、修飾キー付きのクリックはブラウザに任せる。 */
export function Link({ to, onClick, ...props }: Omit<ComponentProps<'a'>, 'href'> & { to: string }) {
  function handleClick(event: MouseEvent<HTMLAnchorElement>) {
    onClick?.(event)
    if (event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return
    event.preventDefault()
    navigate(to)
  }
  return <a href={to} onClick={handleClick} {...props} />
}
