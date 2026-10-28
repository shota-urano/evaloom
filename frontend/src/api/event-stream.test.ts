import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { apiError, fakeFetch, type Recorded } from '@/test/fake-fetch'
import { FakeStream } from '@/test/fake-stream'
import { ApiClient } from './client'
import { EventStreamParser, followExecutionEvents, streamTiming, type StreamEvent, type StreamState } from './event-stream'

describe('EventStreamParser', () => {
  it('塊の途中で区切られても、空行までを1件のイベントにする', () => {
    const parser = new EventStreamParser()
    expect(parser.push('id: 7\nevent: log\nda')).toEqual([])
    expect(parser.push('ta: {"text":"a"}\n')).toEqual([])
    expect(parser.push('\nid: 8\r\nevent: status_changed\r\ndata: x\r\n\r\n')).toEqual([
      { id: 7, type: 'log', data: '{"text":"a"}' },
      { id: 8, type: 'status_changed', data: 'x' },
    ])
  })

  it('コメント行を読み飛ばし、複数の data 行を改行でつなぎ、event が無ければ message とする', () => {
    const parser = new EventStreamParser()
    expect(parser.push(': keep-alive\n\ndata: a\ndata: b\n\n')).toEqual([{ id: null, type: 'message', data: 'a\nb' }])
  })

  it('CR と LF が別の塊に分かれても、空行を余分に作らない', () => {
    const parser = new EventStreamParser()
    expect(parser.push('id: 1\r')).toEqual([])
    expect(parser.push('\ndata: a\r')).toEqual([])
    expect(parser.push('\n\r\n')).toEqual([{ id: 1, type: 'message', data: 'a' }])
  })
})

describe('followExecutionEvents', () => {
  let streams: FakeStream[]
  let calls: Recorded[]
  let stop: (() => void) | undefined

  beforeEach(() => {
    streams = []
    streamTiming.reconnectDelayMs = 1
    streamTiming.maxReconnectDelayMs = 4
  })
  afterEach(() => {
    stop?.()
    streamTiming.reconnectDelayMs = 1000
    streamTiming.maxReconnectDelayMs = 5000
  })

  function follow(respond?: (request: Recorded, index: number) => Response | undefined) {
    const fake = fakeFetch((request) => {
      const custom = respond?.(request, calls.filter((c) => c.url === request.url).length - 1)
      if (custom) return custom
      const stream = new FakeStream()
      streams.push(stream)
      return stream.response
    })
    calls = fake.calls
    const events: StreamEvent[] = []
    const states: { state: StreamState; error?: unknown }[] = []
    stop = followExecutionEvents(new ApiClient(fake.fetch), '01EXEC', {
      onEvent: (event) => events.push(event),
      onState: (state, error) => states.push({ state, error }),
    })
    return { events, states }
  }

  it('届いたイベントを1件ずつ、届いた順に渡す', async () => {
    const { events } = follow()
    await vi.waitFor(() => expect(streams).toHaveLength(1))
    expect(calls[0].url).toBe('/api/executions/01EXEC/events')
    expect(calls[0].headers.Accept).toBe('text/event-stream')
    expect(calls[0].headers['Last-Event-ID']).toBeUndefined()

    streams[0].log(1, 'harness', 'one')
    await vi.waitFor(() => expect(events).toHaveLength(1))
    expect(JSON.parse(events[0].data).text).toBe('one')
    streams[0].log(2, 'harness', 'two')
    await vi.waitFor(() => expect(events.map((e) => e.id)).toEqual([1, 2]))
  })

  it('切断されたら最後に受け取った ID を Last-Event-ID にして再接続し、受け取り済みのイベントは重ねて渡さない', async () => {
    const { events, states } = follow()
    await vi.waitFor(() => expect(streams).toHaveLength(1))
    streams[0].log(3, 'harness', 'a')
    streams[0].send(4, 'status_changed', { from: 'claimed', to: 'running' })
    await vi.waitFor(() => expect(events).toHaveLength(2))
    streams[0].fail()

    await vi.waitFor(() => expect(streams).toHaveLength(2))
    expect(calls[1].headers['Last-Event-ID']).toBe('4')
    expect(states.map((s) => s.state)).toContain('reconnecting')
    // サーバーが境目のイベントを重ねて送っても、表示は重複しない。
    streams[1].send(4, 'status_changed', { from: 'claimed', to: 'running' })
    streams[1].log(5, 'harness', 'b')
    streams[1].close()

    // 本文が終わっても再接続し、続きから受け取る。
    await vi.waitFor(() => expect(streams).toHaveLength(3))
    expect(calls[2].headers['Last-Event-ID']).toBe('5')
    streams[2].log(6, 'harness', 'c')
    await vi.waitFor(() => expect(events.map((e) => e.id)).toEqual([3, 4, 5, 6]))
  })

  it('5xx・通信エラーでは再接続を続け、4xx では止める', async () => {
    const { events, states } = follow((_request, index) => {
      if (index === 0) return apiError(500, 'internal', 'boom')
      if (index === 1) return apiError(404, 'not_found', 'Execution not found')
      return undefined
    })
    await vi.waitFor(() => expect(states.at(-1)?.state).toBe('closed'))
    expect(calls).toHaveLength(2)
    expect(states.at(-1)!.error).toMatchObject({ status: 404 })
    await new Promise((resolve) => setTimeout(resolve, 20))
    expect(calls).toHaveLength(2)
    expect(events).toHaveLength(0)
  })

  it('止めたあとは再接続しない', async () => {
    follow()
    await vi.waitFor(() => expect(streams).toHaveLength(1))
    stop?.()
    await new Promise((resolve) => setTimeout(resolve, 20))
    expect(calls).toHaveLength(1)
  })
})
