/** テスト用の text/event-stream 応答。send でイベントを1件ずつ流し、close / fail で切断する。 */
export class FakeStream {
  readonly response: Response
  private controller!: ReadableStreamDefaultController<Uint8Array>

  constructor() {
    const body = new ReadableStream<Uint8Array>({
      start: (controller) => {
        this.controller = controller
      },
    })
    this.response = new Response(body, { status: 200, headers: { 'Content-Type': 'text/event-stream' } })
  }

  /** そのままの文字列を流す（行の途中で区切られた塊を模すのに使う）。 */
  raw(text: string) {
    this.controller.enqueue(Uint8Array.from(new TextEncoder().encode(text)))
  }

  send(id: number, type: string, data: unknown) {
    this.raw(`id: ${id}\nevent: ${type}\ndata: ${JSON.stringify(data)}\n\n`)
  }

  log(id: number, stream: string, text: string) {
    this.send(id, 'log', { sourceId: `s${id}`, stream, text })
  }

  /** サーバーが本文を終えた（evaloomd は DB の読み出しに失敗すると本文を閉じる）。 */
  close() {
    this.controller.close()
  }

  /** 通信が切れた。 */
  fail() {
    this.controller.error(new TypeError('network error'))
  }
}
