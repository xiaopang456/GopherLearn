/**
 * Minimal `text/event-stream` frame parser.
 *
 * The server contract (spec/API.md) names four events but not their payload
 * shape, so this layer only splits frames. Interpreting an event happens in
 * `sseEvents.js`, which keeps the transport concern separate from the
 * conversation state machine in `chatStream.js`.
 */

/** @typedef {{ event: string, data: string, id: string }} SSEFrame */

const FIELD_EVENT = 'event'
const FIELD_DATA = 'data'
const FIELD_ID = 'id'
const FIELD_RETRY = 'retry'

export function createSSEParser() {
  let buffer = ''
  // A chunk can end between the CR and LF of a CRLF pair. Holding the CR back
  // avoids turning one terminator into two line breaks.
  let heldReturn = false
  /** @type {{event: string, data: string[], id: string} | null} */
  let block = null

  /** @returns {SSEFrame[]} */
  function consume(text) {
    buffer += (heldReturn ? '\r' : '') + text
    heldReturn = false
    if (buffer.endsWith('\r')) {
      // The next chunk may begin with the matching LF; decide later.
      heldReturn = true
      buffer = buffer.slice(0, -1)
    }

    const frames = []
    const lines = buffer.split(/\r\n|\r|\n/)
    // The final element is an incomplete line unless the chunk ended on a
    // terminator; keep it buffered for the next chunk.
    buffer = lines.pop() ?? ''

    for (const line of lines) {
      const frame = consumeLine(line)
      if (frame) frames.push(frame)
    }
    return frames
  }

  /** @returns {SSEFrame | null} */
  function consumeLine(line) {
    if (line === '') return flushBlock()
    if (line.startsWith(':')) return null // comment / heartbeat

    const colon = line.indexOf(':')
    const field = colon === -1 ? line : line.slice(0, colon)
    let value = colon === -1 ? '' : line.slice(colon + 1)
    if (value.startsWith(' ')) value = value.slice(1)

    if (!block) block = { event: '', data: [], id: '' }

    if (field === FIELD_EVENT) block.event = value
    else if (field === FIELD_DATA) block.data.push(value)
    else if (field === FIELD_ID) block.id = value
    else if (field === FIELD_RETRY) {
      /* reconnection delay is not used: the stream is POST, not EventSource */
    }
    // Unknown fields must be ignored per the SSE spec.

    return null
  }

  /** @returns {SSEFrame | null} */
  function flushBlock() {
    if (!block) return null
    const finished = block
    block = null

    // The SSE spec skips a block with no `data`. We additionally allow a
    // data-less named event so `message.completed` still lands if the server
    // sends it with an empty payload.
    if (finished.data.length === 0 && finished.event === '') return null

    return {
      event: finished.event === '' ? 'message' : finished.event,
      data: finished.data.join('\n'),
      id: finished.id,
    }
  }

  /** Frames still pending when the stream closed. @returns {SSEFrame[]} */
  function end() {
    const frames = []
    if (buffer !== '') {
      const frame = consumeLine(buffer)
      if (frame) frames.push(frame)
      buffer = ''
    }
    const tailFrame = flushBlock()
    if (tailFrame) frames.push(tailFrame)
    return frames
  }

  return { consume, end }
}
