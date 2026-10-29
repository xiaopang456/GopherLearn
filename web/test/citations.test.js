import { beforeEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'

import CitationList from '../src/components/CitationList.vue'
import MessageBubble from '../src/components/MessageBubble.vue'
import { normalizeMessage } from '../src/api/sessions.js'
import { normalizeSourceList } from '../src/lib/sources.js'
import { createRunState, reduceRun, RunPhase } from '../src/lib/chatStream.js'
import { createSSEParser } from '../src/lib/sseParser.js'
import { toStreamEvent } from '../src/lib/sseEvents.js'
import { useChat } from '../src/composables/useChat.js'
import { setTokenProvider } from '../src/api/http.js'
import { jsonResponse, lateDeliverySSEResponse, sseResponse } from './helpers/transport.js'

/** Exactly what `domain.Source` marshals to. */
function serverSource(overrides = {}) {
  return {
    source_id: 'src-chunk-1',
    document_id: 'doc-1',
    collection_id: 'col-1',
    chunk_id: 'chunk-1',
    title: '操作系统笔记.md',
    section: '虚拟内存',
    snippet: '页面置换算法决定哪一页被换出。',
    ...overrides,
  }
}

function frame(event, payload) {
  return `event: ${event}\ndata: ${JSON.stringify(payload)}\n\n`
}

describe('normalizeSourceList', () => {
  it('always produces an array so the transcript needs no null test', () => {
    expect(normalizeSourceList(undefined)).toEqual([])
    expect(normalizeSourceList(null)).toEqual([])
    expect(normalizeSourceList('nonsense')).toEqual([])
    expect(normalizeSourceList([])).toEqual([])
  })

  it('maps the server keys onto one frontend shape', () => {
    expect(normalizeSourceList([serverSource()])).toEqual([
      {
        sourceId: 'src-chunk-1',
        documentId: 'doc-1',
        collectionId: 'col-1',
        chunkId: 'chunk-1',
        title: '操作系统笔记.md',
        section: '虚拟内存',
        snippet: '页面置换算法决定哪一页被换出。',
      },
    ])
  })

  it('publishes no retrieval score, because the contract does not carry one', () => {
    const [source] = normalizeSourceList([serverSource({ score: 0.91 })])
    expect(source).not.toHaveProperty('score')
    expect(Object.values(source)).not.toContain(0.91)
  })

  it('drops entries with nothing to show and repeated source ids', () => {
    const list = normalizeSourceList([
      { source_id: 'a', title: '甲', snippet: 'x' },
      { source_id: 'b' },
      { source_id: 'a', title: '甲', snippet: 'x' },
      { title: '乙', snippet: 'y', source_id: 'c' },
    ])
    expect(list.map((item) => item.title)).toEqual(['甲', '乙'])
  })

  it('caps at the five passages one answer may cite', () => {
    const many = Array.from({ length: 9 }, (_, index) =>
      serverSource({ source_id: `s-${index}`, chunk_id: `c-${index}`, title: `t${index}` }),
    )
    expect(normalizeSourceList(many)).toHaveLength(5)
  })
})

describe('the three delivery paths agree', () => {
  const sources = [serverSource(), serverSource({ source_id: 'src-2', chunk_id: 'ch-2', title: '并发.md' })]

  it('history, SSE completion and a plain message produce one structure', () => {
    const fromHistory = normalizeMessage(
      { id: 'm-1', role: 'assistant', content: '答案', sources },
      'fb',
    )

    let run = createRunState({ clientRunId: 'r', placeholderId: 'p' })
    run = reduceRun(
      run,
      toStreamEvent({ event: 'message.completed', data: JSON.stringify({ content: '答案', sources }), id: '' }),
    )
    const fromStream = { content: run.text, sources: run.sources }

    const empty = normalizeMessage({ id: 'm-2', role: 'user', content: '问题' }, 'fb')

    expect(fromHistory.sources).toEqual(fromStream.sources)
    expect(fromHistory.sources).toHaveLength(2)
    expect(empty.sources).toEqual([])
  })

  it('omitting the sources key reads the same as sending none', () => {
    expect(normalizeMessage({ id: 'm', role: 'assistant', content: 'x' }, 'fb').sources).toEqual(
      [],
    )
  })
})

describe('the run state machine owns citations', () => {
  it('attaches sources only on completion, and only to this run', () => {
    let run = createRunState({ clientRunId: 'r', placeholderId: 'p' })
    run = reduceRun(run, { kind: 'started', runId: '', messageId: 'a-1', sources: [] })
    expect(run.sources).toEqual([])

    run = reduceRun(run, {
      kind: 'delta',
      delta: '片段',
      runId: '',
      messageId: 'a-1',
      sources: [serverSource()],
    })
    expect(run.sources).toEqual([])

    run = reduceRun(run, {
      kind: 'completed',
      content: '片段',
      runId: '',
      messageId: 'a-1',
      sources: [serverSource()],
    })
    expect(run.phase).toBe(RunPhase.Completed)
    expect(run.sources).toHaveLength(1)
  })

  it('a late failure cannot strip or graft citations onto a finished answer', () => {
    let run = createRunState({ clientRunId: 'r', placeholderId: 'p' })
    run = reduceRun(run, {
      kind: 'completed',
      content: '答案',
      runId: '',
      messageId: 'a-1',
      sources: [serverSource()],
    })
    const settled = run

    run = reduceRun(run, {
      kind: 'failed',
      runId: '',
      messageId: 'a-1',
      code: 'x',
      message: 'late',
      sources: [],
    })
    expect(run.phase).toBe(RunPhase.Completed)
    expect(run.sources).toEqual(settled.sources)
    expect(run.ignoredEvents).toBe(settled.ignoredEvents + 1)

    run = reduceRun(settled, {
      kind: 'completed',
      content: '别的',
      runId: '',
      messageId: 'a-1',
      sources: [serverSource({ source_id: 'other' })],
    })
    expect(run.sources).toHaveLength(1)
    expect(run.sources[0].sourceId).toBe('src-chunk-1')
    expect(run.text).toBe('答案')
  })

  it('an event from another run cannot deliver citations here', () => {
    let run = createRunState({ clientRunId: 'r', placeholderId: 'p' })
    run = reduceRun(run, { kind: 'started', runId: 'srv-A', messageId: '', sources: [] })
    run = reduceRun(run, {
      kind: 'completed',
      content: 'x',
      runId: 'srv-B',
      messageId: '',
      sources: [serverSource()],
    })
    expect(run.sources).toEqual([])
    expect(run.phase).toBe(RunPhase.Streaming)
  })
})

describe('parsing sources off the wire', () => {
  it('recovers citations from a completed frame split across chunks', () => {
    const text =
      frame('message.started', { message_id: 'a-1' }) +
      frame('message.delta', { message_id: 'a-1', delta: '答案' }) +
      frame('message.completed', { message_id: 'a-1', content: '答案', sources: [serverSource()] })

    const parser = createSSEParser()
    const events = [...parser.consume(text.slice(0, 90)), ...parser.consume(text.slice(90))]
    const parsed = events.map(toStreamEvent)

    expect(parsed.at(-1).kind).toBe('completed')
    expect(parsed.at(-1).sources).toHaveLength(1)
    expect(parsed.at(-1).sources[0].title).toBe('操作系统笔记.md')
  })

  it('reads a completion with no sources key as an ungrounded answer', () => {
    const event = toStreamEvent({
      event: 'message.completed',
      data: JSON.stringify({ message_id: 'a-1', content: '普通回答' }),
      id: '',
    })
    expect(event.sources).toEqual([])
  })
})

describe('useChat mirrors citations into the transcript', () => {
  beforeEach(() => {
    vi.stubEnv('VITE_API_BASE_URL', 'https://api.test')
    setTokenProvider(() => 'tok.cite')
  })

  function serveStream(frames) {
    globalThis.fetch = async (url, init = {}) => {
      const path = new URL(String(url), 'https://api.test').pathname
      if (path.endsWith('/stream')) return sseResponse(frames)
      if (path === '/api/v1/sessions' && init.method === 'POST') {
        return jsonResponse({ data: { id: 's-1', mode: 'chat' }, error: null })
      }
      if (path === '/api/v1/sessions') return jsonResponse({ data: { items: [] }, error: null })
      return jsonResponse({ data: { items: [] }, error: null })
    }
  }

  it('puts citations on the assistant placeholder and nowhere else', async () => {
    serveStream([
      frame('message.started', { message_id: 'a-1' }),
      frame('message.delta', { message_id: 'a-1', delta: '依据资料回答' }),
      frame('message.completed', {
        message_id: 'a-1',
        content: '依据资料回答',
        sources: [serverSource()],
      }),
    ])
    const chat = useChat()
    await chat.send('什么是页面置换')

    const [question, answer] = chat.messages.value
    expect(question.role).toBe('user')
    expect(question.sources).toEqual([])
    expect(answer.status).toBe('done')
    expect(answer.sources).toHaveLength(1)
    expect(answer.sources[0].snippet).toBe('页面置换算法决定哪一页被换出。')
  })

  it('leaves an ungrounded answer with an empty list, not a stale one', async () => {
    serveStream([
      frame('message.completed', { message_id: 'a-1', content: '知识库中没有检索到相关内容。' }),
    ])
    const chat = useChat()
    await chat.send('和资料完全无关的问题')

    expect(chat.messages.value[1].sources).toEqual([])
  })

  it('a late event from a cancelled run cannot cite into the next one', async () => {
    // The first stream stays open past the cancel, then delivers a citation —
    // exactly what a socket already in flight does after the user pressed stop.
    const orphan = lateDeliverySSEResponse()
    let streamCalls = 0
    globalThis.fetch = async (url, init = {}) => {
      const path = new URL(String(url), 'https://api.test').pathname
      if (path.endsWith('/stream')) {
        streamCalls += 1
        if (streamCalls === 1) return orphan.response
        return sseResponse([
          frame('message.completed', { message_id: 'b-2', content: '新答案' }),
        ])
      }
      if (path === '/api/v1/sessions' && init.method === 'POST') {
        return jsonResponse({ data: { id: 's-1', mode: 'chat' }, error: null })
      }
      return jsonResponse({ data: { items: [] }, error: null })
    }

    const chat = useChat()
    chat.activeId.value = 's-1'

    const first = chat.send('第一问')
    await settle()
    chat.cancel()
    const second = chat.send('第二问')
    await settle()

    orphan.push(
      frame('message.completed', {
        message_id: 'a-1',
        content: '旧答案',
        sources: [serverSource()],
      }),
    )
    orphan.finish()
    await first
    await second

    const answers = chat.messages.value.filter((m) => m.role === 'assistant')
    expect(answers).toHaveLength(2)
    expect(answers[0]).toMatchObject({ status: 'stopped', sources: [] })
    expect(answers[1]).toMatchObject({ content: '新答案', sources: [] })
  })
})

describe('citation rendering', () => {
  function mountList(sources) {
    return mount(CitationList, { props: { sources: normalizeSourceList(sources) } })
  }

  it('renders nothing at all when the message cites nothing', () => {
    const app = mountList([])
    expect(app.find('[data-test="citation-list"]').exists()).toBe(false)
  })

  it('starts collapsed so five bodies do not out-shout the answer', () => {
    const many = Array.from({ length: 4 }, (_, index) =>
      serverSource({ source_id: `s${index}`, chunk_id: `c${index}`, snippet: `正文 ${index}` }),
    )
    const app = mountList(many)

    expect(app.find('[data-test="citation-list"]').exists()).toBe(true)
    expect(app.find('[data-test="citation-toggle"]').attributes('aria-expanded')).toBe('false')
    expect(app.findAll('.sources__item')).toHaveLength(0)

    return app
      .find('[data-test="citation-toggle"]')
      .trigger('click')
      .then(async () => {
        await app.vm.$nextTick()
        expect(app.findAll('.sources__item')).toHaveLength(4)
      })
  })

  it('shows title, section and snippet once opened', async () => {
    const app = mountList([serverSource()])
    await app.find('[data-test="citation-toggle"]').trigger('click')

    expect(app.text()).toContain('参考来源')
    expect(app.text()).toContain('操作系统笔记.md')
    expect(app.text()).toContain('虚拟内存')
    expect(app.text()).toContain('页面置换算法决定哪一页被换出。')
  })

  it('omits the section marker when the server sent none', async () => {
    const withoutSection = serverSource()
    delete withoutSection.section
    const app = mountList([withoutSection])
    await app.find('[data-test="citation-toggle"]').trigger('click')

    expect(app.find('.sources__section').exists()).toBe(false)
  })

  it('never publishes a retrieval score', async () => {
    const app = mountList([serverSource({ score: 0.9123 })])
    await app.find('[data-test="citation-toggle"]').trigger('click')

    expect(app.text()).not.toContain('0.9123')
  })

  it('renders markup inside a citation as text, not as elements', async () => {
    globalThis.__pwned = undefined
    const app = mountList([
      serverSource({
        snippet: '<img src=x onerror="globalThis.__pwned=1">',
        title: '<script>globalThis.__pwned=2</script>',
      }),
    ])
    await app.find('[data-test="citation-toggle"]').trigger('click')

    expect(app.find('img').exists()).toBe(false)
    expect(app.find('script').exists()).toBe(false)
    expect(app.text()).toContain('<img src=x onerror=')
    expect(globalThis.__pwned).toBeUndefined()
  })

  it('reaches citations through the message bubble too', async () => {
    const app = mount(MessageBubble, {
      props: {
        message: {
          id: 'm-1',
          role: 'assistant',
          content: '答案',
          status: 'done',
          error: null,
          sources: normalizeSourceList([serverSource()]),
        },
      },
    })

    expect(app.find('[data-test="citation-list"]').exists()).toBe(true)
    await app.find('[data-test="citation-toggle"]').trigger('click')
    expect(app.text()).toContain('操作系统笔记.md')
  })

  it('keeps an ordinary message free of a citation block', () => {
    const app = mount(MessageBubble, {
      props: {
        message: {
          id: 'm-2',
          role: 'user',
          content: '问题',
          status: 'done',
          error: null,
          sources: [],
        },
      },
    })
    expect(app.text()).not.toContain('参考来源')
  })
})

async function settle(times = 4) {
  for (let i = 0; i < times; i += 1) {
    await new Promise((resolve) => setTimeout(resolve, 0))
  }
}
