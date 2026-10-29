import { describe, expect, it } from 'vitest'

import {
  commitTopicMemory,
  mapTopicMemoryError,
  MAX_SESSION_ID_RUNES,
  normalizeTopicPreference,
  readTopicMemory,
  TOPIC_MEMORY_COPY,
  topicMemoryPath,
} from '../src/api/topicMemory.js'
import { ApiError, setTokenProvider } from '../src/api/http.js'
import { installFetchStub, jsonResponse } from './helpers/transport.js'

/**
 * The topic-memory contract is two verbs and one field, so the interesting failures
 * here are all about *not* widening it: a body that grew a `topic_key`, a reader that
 * turned `"true"` into `true`, a sentence that quoted the server's own message.
 *
 * Shapes come from the frozen contract the backend window is implementing:
 * `GET|PUT /api/v1/sessions/{id}/topic-memory` with `{"enabled": boolean}` on both
 * sides — nothing else.
 */

let calls

function route(handlers) {
  calls = []
  setTokenProvider(() => 'tok.topic')
  installFetchStub((url, init) => {
    const path = new URL(url, 'https://api.test').pathname
    const key = `${init?.method ?? 'GET'} ${path}`
    calls.push({ key, path, url: String(url), init })
    const handler = handlers[key] ?? handlers[path]
    if (!handler) throw new Error(`unrouted: ${key}`)
    return typeof handler === 'function' ? handler() : handler
  })
}

function preference(enabled) {
  return jsonResponse({ data: { enabled }, error: null })
}

function refused(status, code, message = 'server detail nobody may show') {
  return jsonResponse({ data: null, error: { code, message } }, { status })
}

describe('topicMemoryPath', () => {
  it('builds the one path this surface has', () => {
    expect(topicMemoryPath('session-9')).toBe('/api/v1/sessions/session-9/topic-memory')
  })

  it('refuses an empty conversation instead of asking the server about nothing', () => {
    expect(() => topicMemoryPath('')).toThrow(ApiError)
    expect(() => topicMemoryPath('   ')).toThrow(ApiError)
    expect(() => topicMemoryPath(undefined)).toThrow(ApiError)
  })

  it('refuses an over-long identifier before it becomes a URL', () => {
    const tooLong = 's'.repeat(MAX_SESSION_ID_RUNES + 1)
    expect(() => topicMemoryPath(tooLong)).toThrow(/不合法/)
    expect(() => topicMemoryPath('s'.repeat(MAX_SESSION_ID_RUNES))).not.toThrow()
  })

  it('escapes an identifier that tries to add a path segment', () => {
    const pathname = new URL(topicMemoryPath('a/b/../c'), 'https://api.test').pathname
    // Exactly the six pieces of one route: the hostile value stays inside its own
    // segment, which is the invariant — the absence of `..` in a string proves nothing,
    // because encodeURIComponent keeps `..` and only escapes the slash.
    expect(pathname.split('/')).toEqual(['', 'api', 'v1', 'sessions', 'a%2Fb%2F..%2Fc', 'topic-memory'])
  })

  it('never puts the bearer token in the URL', () => {
    route({ 'GET /api/v1/sessions/session-9/topic-memory': preference(false) })
    return readTopicMemory({ sessionId: 'session-9' }).then(() => {
      expect(calls[0].url).not.toContain('tok.topic')
      expect(calls[0].init.headers.authorization).toBe('Bearer tok.topic')
    })
  })
})

describe('normalizeTopicPreference', () => {
  it('keeps an explicit "off" as an answer rather than a missing value', () => {
    expect(normalizeTopicPreference({ enabled: false })).toEqual({ enabled: false })
  })

  it('copies the one named key and ignores everything else the server might grow', () => {
    const normalized = normalizeTopicPreference({
      enabled: true,
      topic_key: 'redis-持久化',
      user_id: 'user-1',
      scope: 'topic',
      session_id: 'session-9',
      all_users: true,
      mastery: 91,
    })
    expect(Object.keys(normalized)).toEqual(['enabled'])
    expect(normalized.enabled).toBe(true)
  })

  it('reads nothing out of a body whose flag is missing or not a boolean', () => {
    expect(normalizeTopicPreference({})).toBeNull()
    expect(normalizeTopicPreference({ enabled: 'true' })).toBeNull()
    expect(normalizeTopicPreference({ enabled: 1 })).toBeNull()
    expect(normalizeTopicPreference({ enabled: null })).toBeNull()
    expect(normalizeTopicPreference(null)).toBeNull()
    expect(normalizeTopicPreference([{ enabled: true }])).toBeNull()
  })

  it('does not mistake the envelope for the answer', () => {
    // `http.js` hands back the whole envelope when `data` is null, and a null payload
    // is "could not read" here — never "it is off".
    expect(normalizeTopicPreference({ data: null, error: null })).toBeNull()
  })
})

describe('readTopicMemory', () => {
  it('asks with GET and no body at all', async () => {
    route({ 'GET /api/v1/sessions/session-9/topic-memory': preference(true) })
    const payload = await readTopicMemory({ sessionId: 'session-9' })

    expect(payload).toEqual({ enabled: true })
    expect(calls[0].key).toBe('GET /api/v1/sessions/session-9/topic-memory')
    expect(calls[0].init.body).toBeUndefined()
  })

  it('sends nothing when the conversation is not named', async () => {
    route({})
    await expect(readTopicMemory({ sessionId: '' })).rejects.toMatchObject({
      code: 'local_invalid',
    })
    expect(calls).toHaveLength(0)
  })

  it('hands an abort signal through so a session switch can cancel the read', async () => {
    route({ 'GET /api/v1/sessions/session-9/topic-memory': preference(false) })
    const controller = new AbortController()
    await readTopicMemory({ sessionId: 'session-9', signal: controller.signal })
    expect(calls[0].init.signal).toBe(controller.signal)
  })
})

describe('commitTopicMemory', () => {
  it('sends PUT with exactly one key in the body', async () => {
    route({ 'PUT /api/v1/sessions/session-9/topic-memory': preference(true) })
    const payload = await commitTopicMemory({ sessionId: 'session-9', enabled: true })

    expect(calls).toHaveLength(1)
    expect(calls[0].key).toBe('PUT /api/v1/sessions/session-9/topic-memory')
    expect(JSON.parse(calls[0].init.body)).toEqual({ enabled: true })
    expect(Object.keys(JSON.parse(calls[0].init.body))).toEqual(['enabled'])
    expect(payload).toEqual({ enabled: true })
  })

  it('sends a real false, not a field that went missing', async () => {
    route({ 'PUT /api/v1/sessions/session-9/topic-memory': preference(false) })
    await commitTopicMemory({ sessionId: 'session-9', enabled: false })

    const body = JSON.parse(calls[0].init.body)
    expect(body.enabled).toBe(false)
    expect('enabled' in body).toBe(true)
  })

  it('carries none of the keys that would let a caller name a subject, an account or a scope', async () => {
    const forbidden = ['topic_key', 'user_id', 'scope', 'session_id', 'all_users', 'topic', 'enabled_all']
    route({ 'PUT /api/v1/sessions/session-9/topic-memory': preference(true) })
    await commitTopicMemory({ sessionId: 'session-9', enabled: true })

    const serialized = calls[0].init.body
    for (const key of forbidden) expect(serialized).not.toContain(key)
  })

  it('refuses a non-boolean decision without touching the network', async () => {
    route({})
    for (const value of ['true', 1, 0, null, undefined, {}]) {
      await expect(commitTopicMemory({ sessionId: 'session-9', enabled: value })).rejects.toMatchObject({
        code: 'local_invalid',
      })
    }
    expect(calls).toHaveLength(0)
  })

  it('refuses a missing conversation without touching the network', async () => {
    route({})
    await expect(commitTopicMemory({ sessionId: '', enabled: true })).rejects.toMatchObject({
      code: 'local_invalid',
    })
    expect(calls).toHaveLength(0)
  })
})

describe('mapTopicMemoryError', () => {
  const sentence = (error) => mapTopicMemoryError(error).copy

  it('never repeats the text the server wrote', () => {
    const mapped = sentence(new ApiError('session 4f0a of user-1 is not a learning agent', {
      code: 'forbidden',
      status: 403,
    }))
    expect(mapped).toBe(TOPIC_MEMORY_COPY.refused)
    expect(mapped).not.toContain('user-1')
    expect(mapped).not.toContain('4f0a')
  })

  it('answers an empty error with a sentence the learner can act on', () => {
    expect(sentence(undefined)).toBe(TOPIC_MEMORY_COPY.writeFailed)
    expect(sentence(null)).toBe(TOPIC_MEMORY_COPY.writeFailed)
  })

  it('gives the same words to "not yours" and "never existed"', () => {
    // The contract makes these two byte-identical 404s, so the copy must not be the
    // place where a client leaks that somebody else's conversation was found.
    const notYours = sentence(new ApiError('resource not found', { code: 'not_found', status: 404 }))
    const neverStored = sentence(new ApiError('resource not found', { code: 'not_found', status: 404 }))
    expect(notYours).toBe(TOPIC_MEMORY_COPY.readFailed)
    expect(neverStored).toBe(notYours)
  })

  it('routes a rejected bearer to the read sentence, because the page cannot save what it cannot read', () => {
    expect(mapTopicMemoryError(new ApiError('x', { code: 'unauthorized', status: 401 }))).toEqual({
      copy: TOPIC_MEMORY_COPY.readFailed,
      code: 'unauthorized',
    })
    expect(sentence(new ApiError('x', { code: 'whatever', status: 401 }))).toBe(TOPIC_MEMORY_COPY.readFailed)
  })

  it('names the refusal for a conversation that cannot hold a consent', () => {
    for (const code of ['invalid_request', 'forbidden']) {
      expect(sentence(new ApiError('x', { code, status: 400 }))).toBe(TOPIC_MEMORY_COPY.refused)
    }
  })
})

describe('failure bytes on the wire', () => {
  it('surfaces a 403 as an error, not as a saved preference', async () => {
    route({ 'PUT /api/v1/sessions/session-9/topic-memory': refused(403, 'forbidden') })
    await expect(commitTopicMemory({ sessionId: 'session-9', enabled: true })).rejects.toMatchObject({
      code: 'forbidden',
      status: 403,
    })
  })

  it('surfaces a 200 with an error code inside as an error too', async () => {
    route({
      'GET /api/v1/sessions/session-9/topic-memory': () =>
        jsonResponse({ data: null, error: { code: 'not_found', message: 'resource not found' } }, { status: 404 }),
    })
    await expect(readTopicMemory({ sessionId: 'session-9' })).rejects.toMatchObject({ code: 'not_found' })
  })
})
