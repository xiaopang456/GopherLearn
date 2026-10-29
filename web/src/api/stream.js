import {
  apiBase,
  buildUrl,
  currentToken,
  isAbort,
  parseErrorBody,
  reportIfRejected,
} from './http.js'
import { createSSEParser } from '../lib/sseParser.js'
import { toStreamEvent } from '../lib/sseEvents.js'
import { isStepEventName, toStepEvent } from '../lib/agentSteps.js'
import { isTextBlockEventName, toTextBlockEvent } from '../lib/agentTextBlocks.js'
import { toWireKnowledge } from '../lib/knowledge.js'
import { streamPath } from './sessions.js'

/**
 * POST + `text/event-stream`. `EventSource` is unusable here: it cannot send an
 * Authorization header and it cannot issue POST, so the token would have to go
 * into the query string — which the contract forbids. Reading the response body
 * as a stream keeps the credential in a header.
 *
 * @param {object} options
 * @param {string} options.sessionId
 * @param {string} options.content
 * @param {object} [options.knowledge]   Chat knowledge draft; ignored unless it
 *   is usable, and never sent for a Learning Agent session (the caller decides)
 * @param {AbortSignal} options.signal
 * @param {(event: object) => void} options.onEvent   normalised stream event
 * @param {boolean} [options.streamSteps]  ask for the agent's progress frames; the
 *   caller sets it from the session's stored mode, and the server refuses the field on
 *   a turn whose strategy has no steps to report
 * @param {(step: object) => void} [options.onStep] where a progress frame goes
 * @param {(block: object) => void} [options.onText] where one agent text-block frame goes
 * @param {string} [options.token]
 * @returns {Promise<void>} resolves once the stream reached its end
 */
export async function streamMessage({
  sessionId,
  content,
  knowledge,
  signal,
  onEvent,
  streamSteps = false,
  onStep,
  onText,
  token,
  requestId,
  knowledgePolicy,
  topicConsent,
}) {
  const bearer = token === undefined ? currentToken() : token
  const url = buildUrl(streamPath(sessionId))

  // Session mode and identity remain server-owned. Agent callers may explicitly
  // supply the original operation fields; ordinary callers keep their contract.
  const body = { content: String(content ?? '') }
  // Explicit original Agent fields supplied by the caller. No operation key
  // is generated here; ordinary calls that omit them keep their wire body.
  if (requestId !== undefined) body.requestId = requestId
  if (knowledgePolicy !== undefined) body.knowledgePolicy = knowledgePolicy
  if (topicConsent !== undefined) body.topicConsent = topicConsent
  const wire = toWireKnowledge(knowledge)
  if (wire) body.knowledge = wire
  // Progress is requested only explicitly; ordinary calls keep their wire body.
  if (streamSteps === true) body.stream_steps = true

  let response
  try {
    response = await fetch(url, {
      method: 'POST',
      headers: {
        accept: 'text/event-stream',
        'content-type': 'application/json',
        ...(bearer ? { authorization: `Bearer ${bearer}` } : {}),
      },
      body: JSON.stringify(body),
      signal,
    })
  } catch (cause) {
    if (isAbort(cause)) throw cause
    throw new StreamTransportError('无法建立到服务的流式连接。', {
      code: 'stream_connect_failed',
    })
  }

  if (!response.ok) {
    const error = await parseErrorBody(response)
    throw reportIfRejected(
      new StreamTransportError(error.message, {
        code: error.code,
        status: response.status,
      }),
    )
  }

  if (!response.body) {
    throw new StreamTransportError('服务未返回可读取的事件流。', {
      code: 'stream_empty_body',
    })
  }

  const reader = response.body.getReader()
  const decoder = new TextDecoder('utf-8')
  const parser = createSSEParser()

  const emit = (frame) => {
    // A step frame is progress, not answer text. It gets its own channel and never
    // reaches `onEvent`, so the answer state machine cannot bind a run identity from it
    // (which would make the real deltas look foreign) nor count a watched turn's progress
    // as an ignored message. A caller that did not ask to watch receives silence, which
    // is exactly what it asked for.
    if (isStepEventName(frame.event)) {
      const step = toStepEvent(frame)
      if (step && typeof onStep === 'function') onStep(step)
      return null
    }
    // An agent turn's published text is a third thing: neither progress about a step nor a
    // `message.delta`. Folding it by block id is what keeps a round's note separate from the
    // answer that followed it, so it gets its own channel and its own reducer, and the same
    // rule applies — never let it into `onEvent`, where it would bind the run identity away
    // from the frames that carry the confirmed answer.
    if (isTextBlockEventName(frame.event)) {
      const block = toTextBlockEvent(frame)
      if (block && typeof onText === 'function') onText(block)
      return null
    }
    const event = toStreamEvent(frame)
    if (event.kind !== 'unknown') onEvent(event)
    return event
  }

  try {
    for (;;) {
      const { done, value } = await reader.read()
      if (done) break
      // `stream: true` is what lets a multi-byte UTF-8 character span chunks.
      const text = decoder.decode(value, { stream: true })
      for (const frame of parser.consume(text)) emit(frame)
    }
    const rest = decoder.decode()
    if (rest) for (const frame of parser.consume(rest)) emit(frame)
    for (const frame of parser.end()) emit(frame)
  } catch (cause) {
    if (isAbort(cause)) throw cause
    throw new StreamTransportError('事件流在回答完成前中断。', {
      code: 'stream_interrupted',
      cause,
    })
  } finally {
    try {
      reader.cancel()
    } catch {
      /* already closed */
    }
  }
}

export function streamEndpoint(sessionId) {
  // Exposed for tests and diagnostics; note there is never a query string.
  return `${apiBase()}${streamPath(sessionId)}`
}

export class StreamTransportError extends Error {
  constructor(message, { code = 'stream_failed', status = 0, cause } = {}) {
    super(message, cause ? { cause } : undefined)
    this.name = 'StreamTransportError'
    this.code = code
    this.status = status
  }
}
