import type { EventSourceLike, EventSourceListener } from '@/api/client'

/** テスト用の EventSource。emit でサーバーからのイベントを模す。 */
export class FakeEventSource implements EventSourceLike {
  readonly url: string
  closed = false
  private readonly listeners = new Map<string, Set<EventSourceListener>>()

  constructor(url: string) {
    this.url = url
  }

  addEventListener(type: string, listener: EventSourceListener) {
    let set = this.listeners.get(type)
    if (!set) this.listeners.set(type, (set = new Set()))
    set.add(listener)
  }

  removeEventListener(type: string, listener: EventSourceListener) {
    this.listeners.get(type)?.delete(listener)
  }

  close() {
    this.closed = true
  }

  emit(type: string, data: unknown, id?: number) {
    const event = new MessageEvent<string>(type, { data: JSON.stringify(data), lastEventId: id === undefined ? '' : String(id) })
    this.listeners.get(type)?.forEach((listener) => listener(event))
  }
}

/** 作った EventSource を記録する factory。 */
export function fakeEvents() {
  const sources: FakeEventSource[] = []
  const factory = (url: string) => {
    const source = new FakeEventSource(url)
    sources.push(source)
    return source
  }
  const open = (url: string) => sources.filter((s) => s.url === url && !s.closed)
  return { factory, sources, open }
}
