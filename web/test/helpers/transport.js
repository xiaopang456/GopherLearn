/** Test doubles for the transport layer: enough of `fetch`/`Response` to
 *  exercise the real `http.js` and `stream.js` code paths. */

function bytesOf(chunk) {
  // Anything that is not a string is already encoded; relying on `instanceof`
  // is unreliable when the test realm and the encoder disagree about
  // Uint8Array.
  if (typeof chunk === 'string') return new TextEncoder().encode(chunk)
  return chunk
}

export function jsonResponse(payload, { status = 200 } = {}) {
  const body = typeof payload === 'string' ? payload : JSON.stringify(payload)
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: { get: () => 'application/json' },
    text: async () => body,
  }
}

export function sseFrame(event, payload) {
  const data = payload === undefined ? '' : JSON.stringify(payload)
  return `event: ${event}\ndata: ${data}\n\n`
}

/**
 * A `Response`-shaped object whose body yields `chunks` then ends.
 * Pass a Uint8Array chunk to simulate a multi-byte character split across
 * network reads.
 */
export function sseResponse(chunks, { status = 200, closeAfterChunks = true } = {}) {
  let index = 0
  const reader = {
    async read() {
      if (index >= chunks.length) {
        if (!closeAfterChunks) return new Promise(() => {}) // hang forever
        return { done: true, value: undefined }
      }
      return { done: false, value: bytesOf(chunks[index++]) }
    },
    async cancel() {
      index = chunks.length
    },
  }

  return {
    ok: status >= 200 && status < 300,
    status,
    headers: { get: (name) => (name === 'content-type' ? 'text/event-stream' : null) },
    text: async () => chunks.map((chunk) => asText(chunk)).join(''),
    body: { getReader: () => reader },
  }
}

function asText(chunk) {
  return typeof chunk === 'string' ? chunk : new TextDecoder().decode(chunk)
}

/**
 * A stream that emits `chunks` and then blocks until the request's signal
 * aborts, at which point reading rejects with `AbortError` — the same shape a
 * real browser produces for `fetch(url, { signal })`.
 */
export function abortableSSEResponse(chunks, signal) {
  let index = 0
  const reader = {
    read() {
      if (index < chunks.length) {
        return Promise.resolve({ done: false, value: bytesOf(chunks[index++]) })
      }
      return new Promise((resolve, reject) => {
        const abort = () => {
          const error = new Error('The user aborted a request.')
          error.name = 'AbortError'
          reject(error)
        }
        if (signal?.aborted) abort()
        else signal?.addEventListener('abort', abort, { once: true })
      })
    },
    cancel: () => Promise.resolve(),
  }

  return {
    ok: true,
    status: 200,
    headers: { get: () => 'text/event-stream' },
    text: async () => '',
    body: { getReader: () => reader },
  }
}

/** Replace `globalThis.fetch`, recording every call. */
export function installFetchStub(handler) {
  const calls = []
  const impl = async (url, init = {}) => {
    const record = { url: String(url), init }
    calls.push(record)
    const next =
      typeof handler === 'function' ? handler(url, init, calls.length) : handler
    if (next instanceof Error) throw next
    if (typeof next === 'function') return next(url, init)
    return next
  }
  globalThis.fetch = impl
  return { calls, fetch: impl }
}

/** A promise plus its resolvers, so a test can order concurrent responses. */
export function deferred() {
  let resolve
  let reject
  const promise = new Promise((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
}

/**
 * A stream the test drives by hand. Reads queue up until `push` supplies a
 * frame, and it deliberately ignores the abort signal — which is how a real
 * socket can still deliver a frame that was already in flight when the user
 * pressed stop. That late frame is what the per-run callback must reject.
 */
export function lateDeliverySSEResponse() {
  const waiting = []
  const buffer = []
  let ended = false

  const deliver = () => {
    if (waiting.length === 0) return
    const resolve = waiting.shift()
    if (buffer.length > 0) {
      resolve({ done: false, value: buffer.shift() })
    } else if (ended) {
      resolve({ done: true, value: undefined })
    }
  }

  const response = {
    ok: true,
    status: 200,
    headers: { get: () => 'text/event-stream' },
    text: async () => '',
    body: {
      getReader: () => ({
        read: () =>
          new Promise((resolve) => {
            if (buffer.length > 0) {
              resolve({ done: false, value: buffer.shift() })
            } else if (ended) {
              resolve({ done: true, value: undefined })
            } else {
              waiting.push(resolve)
            }
          }),
        cancel: () => Promise.resolve(),
      }),
    },
  }

  return {
    response,
    push(frame) {
      buffer.push(bytesOf(frame))
      deliver()
    },
    finish() {
      ended = true
      deliver()
    },
  }
}

export function abortError() {
  const error = new Error('aborted')
  error.name = 'AbortError'
  return error
}
