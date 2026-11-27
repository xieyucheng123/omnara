import { expect, it } from 'vitest'

import { responseWithStreamLifetime } from './response-stream'

it.each([204, 205, 304])(
  'returns a %i response untouched instead of wrapping its body',
  (status) => {
    // Some fetch layers (including test doubles) hand back an empty stream
    // rather than a null body for null-body statuses. Wrapping it would throw
    // "Response with null body status cannot have body".
    const response = new Response(new ReadableStream<Uint8Array>({ start: (c) => c.close() }), {
      status: 200,
    })
    Object.defineProperty(response, 'status', { configurable: true, value: status })

    let disposed = false
    const wrapped = responseWithStreamLifetime(response, new AbortController().signal, () => {
      disposed = true
    })

    expect(wrapped).toBe(response)
    expect(disposed).toBe(true)
  },
)
