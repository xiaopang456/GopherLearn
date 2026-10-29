import { beforeEach, describe, expect, it } from 'vitest'

import { useChat } from '../src/composables/useChat.js'
import { setTokenProvider } from '../src/api/http.js'
import {
  installFetchStub,
  jsonResponse,
  lateDeliverySSEResponse,
  sseFrame,
} from './helpers/transport.js'

/**
 * The one body an agent turn writes, and who is allowed to change it.
 *
 * These cases are about two failures that look identical from the transcript and are not: a
 * completed frame that the answer reducer took, and a completed frame that arrives too late or
 * belongs to another run. The second must leave the page exactly as it was - the same text, the
 * same paragraphs in the same places - because a learner who watched a turn get cancelled has
 * read a fact about that turn, and a frame cannot unsay it.
 *
 * They also pin the single-source rule: the text a block grows is the run's own text, so the
 * transcript, the caret and the scroll watcher are all reading the characters on screen rather
 * than a second copy that could disagree with them.
 */

const RUN = 'run-1'
let late
let pending

function frame(name, payload) {
  return sseFrame(name, { run_id: RUN, ...payload })
}

const NARRATION = [
  frame('agent.text.started', { block_id: 'blk-1', round: 1 }),
  frame('agent.text.delta', { block_id: 'blk-1', content: 'PROCESS' }),
  frame('agent.text.ended', { block_id: 'blk-1', kind: 'process' }),
]

const ANSWER = [
  frame('agent.text.started', { block_id: 'blk-2', round: 2 }),
  frame('agent.text.delta', { block_id: 'blk-2', content: 'ANSWER' }),
  frame('agent.text.ended', { block_id: 'blk-2', kind: 'answer_candidate' }),
]

async function flush(times = 8) {
  for (let index = 0; index < times; index += 1) await new Promise((resolve) => setTimeout(resolve, 0))
}

async function liveTurn() {
  late = lateDeliverySSEResponse()
  installFetchStub((url) => {
    const path = new URL(String(url), 'https://api.test').pathname
    if (path.endsWith('/stream')) return late.response
    if (path === '/api/v1/sessions') {
      return jsonResponse({
        data: { items: [{ id: 'agent-1', title: '学习会话', mode: 'learning_agent' }] },
        error: null,
      })
    }
    return jsonResponse({ data: { items: [] }, error: null })
  })
  const chat = useChat()
  await chat.loadSessions()
  await chat.openSession('agent-1')
  pending = chat.send('问题')
  await flush()
  late.push(frame('message.started', { message_id: 'assistant-1' }))
  for (const one of [...NARRATION, ...ANSWER]) late.push(one)
  await flush()
  return chat
}

function lastMessage(chat) {
  return chat.messages.value.at(-1)
}

beforeEach(() => {
  setTokenProvider(() => 'tok.blocks')
})

describe('the body a block turn writes', () => {
  it('is the run text itself, so every consumer reads the same characters', async () => {
    const chat = await liveTurn()
    try {
      // Two blocks are on screen as one growing body: the note the model wrote, then the
      // candidate answer. `run.text` carries that body, which is what the transcript entry,
      // the caret and the autoscroll watcher all read.
      expect(lastMessage(chat).content).toBe('PROCESSANSWER')
      expect(chat.run.value.text).toBe('PROCESSANSWER')
      // Nothing is folded while nothing has been confirmed.
      expect(lastMessage(chat).processNotes).toEqual([])
    } finally {
      chat.cancel()
      late.finish()
      await pending
    }
  })

  it('grows paragraph by paragraph instead of arriving once at the end', async () => {
    const chat = await liveTurn()
    try {
      const seen = [chat.run.value.text]
      expect(seen).toEqual(['PROCESSANSWER'])
      const long = frame('agent.text.started', { block_id: 'blk-3', round: 3 })
      late.push(long)
      await flush()
      const lengths = []
      for (const piece of ['第', '一段', '很长的', '回答正文', '还在继续']) {
        late.push(frame('agent.text.delta', { block_id: 'blk-3', content: piece }))
        await flush()
        lengths.push(chat.run.value.text.length)
      }
      // Every fragment moved the watched length: a scroll handler bound to this value follows
      // an answer as it is written, which is what a learner at the bottom of a long reply
      // needs.
      expect(lengths).toEqual(lengths.slice().sort((a, b) => a - b))
      expect(new Set(lengths).size).toBe(lengths.length)
      expect(chat.run.value.text.startsWith('PROCESSANSWER第')).toBe(true)
    } finally {
      chat.cancel()
      late.finish()
      await pending
    }
  })
})

describe('a completion the page did not accept', () => {
  it.each(['cancelled', 'foreign-run'])('cannot confirm a block or rewrite text: %s', async (kind) => {
    const chat = await liveTurn()
    try {
      if (kind === 'cancelled') {
        chat.cancel()
        await flush()
      }
      const before = lastMessage(chat).content
      expect(before).toBe('PROCESSANSWER')

      late.push(sseFrame('message.completed', {
        // The cancelled case replays this turn's own id; the foreign case is a frame from a
        // different run on the same socket. Both are frames the answer reducer must drop, and
        // a dropped frame has to stop at the reducer rather than reaching the blocks too.
        run_id: kind === 'cancelled' ? RUN : 'run-FOREIGN',
        message_id: 'assistant-1',
        content: 'ANSWER',
        final_block_id: 'blk-2',
      }))
      await flush()

      expect(lastMessage(chat).content).toBe(before)
      expect(lastMessage(chat).processNotes).toEqual([])
      // The answer paragraph is not promoted, so no fold appears beside it either.
      expect(chat.run.value.text).toBe(before)
    } finally {
      late.finish()
      await pending
    }
  })

  it('still lets the completion this run did accept do both jobs', async () => {
    const chat = await liveTurn()
    try {
      late.push(sseFrame('message.completed', {
        run_id: RUN,
        message_id: 'assistant-1',
        content: 'ANSWER',
        final_block_id: 'blk-2',
      }))
      await flush()
      await flush()

      expect(lastMessage(chat).content).toBe('ANSWER')
      expect(lastMessage(chat).processNotes).toEqual([
        { blockId: 'blk-1', round: 1, text: 'PROCESS' },
      ])
    } finally {
      late.finish()
      await pending
    }
  })
})
