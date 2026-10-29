import { describe, expect, it } from 'vitest'

import {
  canSend,
  validateDisplayName,
  validateEmail,
  validateMessageContent,
  validatePassword,
} from '../src/lib/validation.js'
import { utf8ByteLength } from '../src/lib/text.js'
import { readAuthPayload, normalizeUser } from '../src/api/auth.js'
import {
  normalizeMessage,
  normalizeSession,
  readList,
  streamPath,
} from '../src/api/sessions.js'
import { createSSEParser } from '../src/lib/sseParser.js'
import { toStreamEvent } from '../src/lib/sseEvents.js'

describe('password policy is measured in bytes', () => {
  it('accepts exactly eight ASCII bytes', () => {
    expect(validatePassword('abcdefg1')).toBe(null)
  })

  it('rejects seven bytes', () => {
    expect(validatePassword('abcdefg')).toMatch(/8 字节/)
  })

  it('counts a Chinese character as three bytes', () => {
    expect(utf8ByteLength('密码足够长')).toBe(15)
    expect(validatePassword('密')).toMatch(/3 字节/)
  })

  it('counts an astral character as four bytes', () => {
    expect(utf8ByteLength('🦫')).toBe(4)
  })

  it('caps at 128 bytes', () => {
    expect(validatePassword('a'.repeat(128))).toBe(null)
    expect(validatePassword('a'.repeat(129))).toMatch(/128 字节/)
  })

  it('rejects an empty password', () => {
    expect(validatePassword('')).toBe('请输入密码。')
  })
})

describe('email policy stays permissive', () => {
  it.each([
    'learner@example.test',
    'a+b@mail.example.com.cn',
    '名字@例え.jp',
  ])('accepts %s', (value) => {
    expect(validateEmail(value)).toBe(null)
  })

  it.each(['', '   ', 'no-at-sign', 'a@b', 'a b@c.test', '@x.test'])('rejects %s', (value) => {
    expect(validateEmail(value)).toBeTruthy()
  })
})

describe('display name', () => {
  it('rejects blank and over-long values', () => {
    expect(validateDisplayName('  ')).toBe('请填写昵称。')
    expect(validateDisplayName('名'.repeat(65))).toMatch(/64/)
    expect(validateDisplayName('学习者')).toBe(null)
  })
})

describe('composer limits', () => {
  it('treats whitespace as nothing to send', () => {
    expect(canSend('   \n ')).toBe(false)
    expect(validateMessageContent('  ')).toBe(null)
  })

  it('blocks an oversized message', () => {
    expect(validateMessageContent('x'.repeat(8001))).toMatch(/8000/)
    expect(canSend('x'.repeat(8001))).toBe(false)
  })

  it('allows a normal message', () => {
    expect(canSend('为什么需要索引')).toBe(true)
  })
})

describe('payload normalizers tolerate the contract’s open spots', () => {
  it('reads a bearer token under either spelling', () => {
    expect(readAuthPayload({ token: 't1', user: { email: 'a@b.test' } })).toEqual({
      token: 't1',
      user: { id: '', email: 'a@b.test', displayName: '' },
    })
    expect(readAuthPayload({ access_token: 't2', profile: { display_name: '小明' } }).token).toBe('t2')
    expect(normalizeUser({ id: 7, username: 'x@y.test' }).email).toBe('x@y.test')
  })

  it('finds a list under items, sessions, messages or a bare array', () => {
    expect(readList({ items: [1] })).toEqual([1])
    expect(readList({ sessions: [2] }, 'sessions')).toEqual([2])
    expect(readList([3])).toEqual([3])
    expect(readList(null)).toEqual([])
    expect(readList('nope')).toEqual([])
  })

  it('never invents a role the transcript cannot render', () => {
    expect(normalizeMessage({ role: 'USER', content: 'x' }, 'fb').role).toBe('user')
    expect(normalizeMessage({ role: 'wizard' }, 'fb').role).toBe('assistant')
    expect(normalizeMessage({}, 'fb1').id).toBe('fb1')
    expect(normalizeSession({}).title).toBe('未命名会话')
  })

  it('builds a stream path with no query string', () => {
    expect(streamPath('s 1')).toBe('/api/v1/sessions/s%201/stream')
  })
})

describe('toStreamEvent', () => {
  const parseOne = (text) => {
    const parser = createSSEParser()
    const frames = [...parser.consume(text), ...parser.end()]
    return frames.map(toStreamEvent)
  }

  it('maps the four contract events', () => {
    expect(parseOne('event: message.started\ndata: {"message_id":"a"}\n\n')[0].kind).toBe('started')
    expect(parseOne('event: message.delta\ndata: {"delta":"x"}\n\n')[0].kind).toBe('delta')
    expect(parseOne('event: message.completed\ndata: {}\n\n')[0].kind).toBe('completed')
    expect(parseOne('event: message.failed\ndata: {"code":"x"}\n\n')[0].kind).toBe('failed')
  })

  it('accepts camelCase and a nested error object', () => {
    const [event] = parseOne(
      'event: message.failed\ndata: {"messageId":"a","error":{"code":"invalid_request","message":"no"}}\n\n',
    )
    expect(event).toMatchObject({
      kind: 'failed',
      messageId: 'a',
      code: 'invalid_request',
      message: 'no',
    })
  })

  it('marks an unmapped event as unknown without losing the frame', () => {
    const [event] = parseOne('event: message.progress\ndata: {"pct":5}\n\n')
    expect(event.kind).toBe('unknown')
  })

  it('degrades a malformed JSON body instead of throwing', () => {
    const [event] = parseOne('event: message.delta\ndata: {oops\n\n')
    expect(event).toMatchObject({ kind: 'delta', delta: '' })
  })

  it('treats a non-object body as an empty payload', () => {
    const [event] = parseOne('event: message.completed\ndata: 7\n\n')
    expect(event.kind).toBe('completed')
    expect(event.content).toBe('')
  })
})
