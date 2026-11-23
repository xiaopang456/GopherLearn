import { flushPromises } from '@vue/test-utils'
import { nextTick, ref } from 'vue'
import { describe, expect, it, vi } from 'vitest'

import { useTopicSelection } from '../src/composables/useTopicSelection.js'

function fakeChat() {
  return {
    draft: ref('帮我复习鉴权'),
    streaming: ref(false),
    activeId: ref('s1'),
    isAgentSession: ref(true),
    send: vi.fn(() => Promise.resolve()),
  }
}

function fakeApi({ suggest, browse, issue, capability } = {}) {
  return {
    suggestTopics: vi.fn(suggest ?? (async () => ({ status_code: 1000, candidates: [], maxSelectable: 0 }))),
    browseTopicCatalogue: vi.fn(browse ?? (async () => ({ status_code: 1000, candidates: [] }))),
    issueTopicConsent: vi.fn(issue ?? (async () => ({ status_code: 1000, ticket: 'ticket-1' }))),
    topicConsentCapability: vi.fn(capability ?? (async () => ({ status_code: 1000, supported: true }))),
  }
}

const candidates = () => ({
  status_code: 1000,
  maxSelectable: 2,
  candidates: [
    { topicKey: 't1', displayName: '主题一' },
    { topicKey: 't2', displayName: '主题二' },
  ],
})

describe('topic selection flow', () => {
  it('shares one requestId across the suggestion, the ticket and the send', async () => {
    const chat = fakeChat()
    const api = fakeApi({ suggest: async () => candidates() })
    const flow = useTopicSelection(chat, api)
    flow.enabled.value = true

    await flow.submit()
    expect(flow.prompt.value.visible).toBe(true)
    expect(flow.prompt.value.candidates).toHaveLength(2)

    const suggestion = api.suggestTopics.mock.calls[0][0]
    expect(suggestion.sessionId).toBe('s1')
    expect(suggestion.question).toBe('帮我复习鉴权')
    expect(typeof suggestion.requestId).toBe('string')
    expect(suggestion.requestId).not.toBe('')

    flow.toggleSelection('t1')
    expect(flow.prompt.value.selected).toEqual(['t1'])
    await flow.confirmSelection()

    expect(api.issueTopicConsent).toHaveBeenCalledTimes(1)
    const consent = api.issueTopicConsent.mock.calls[0][0]
    // Same session, same question, same requestId the suggestion carried.
    expect(consent).toEqual({
      sessionId: 's1',
      question: '帮我复习鉴权',
      requestId: suggestion.requestId,
      selectedTopicKeys: ['t1'],
    })

    // The send reuses that id and carries the ticket: the server binds the
    // ticket to (session, requestId, question) before it reads anything.
    expect(chat.send).toHaveBeenCalledTimes(1)
    expect(chat.send).toHaveBeenCalledWith('帮我复习鉴权', null, {
      requestId: suggestion.requestId,
      topicConsent: { ticket: 'ticket-1', selectedTopicKeys: ['t1'] },
    })
    expect(flow.prompt.value.visible).toBe(false)
  })

  it('“本次不使用” still asks with the same requestId and no ticket', async () => {
    const chat = fakeChat()
    const api = fakeApi({ suggest: async () => candidates() })
    const flow = useTopicSelection(chat, api)
    flow.enabled.value = true

    await flow.submit()
    expect(flow.prompt.value.visible).toBe(true)
    const suggestion = api.suggestTopics.mock.calls[0][0]

    flow.skipSelection()
    expect(flow.prompt.value.visible).toBe(false)
    expect(api.issueTopicConsent).not.toHaveBeenCalled()
    expect(chat.send).toHaveBeenCalledTimes(1)
    const [content, retry, fields] = chat.send.mock.calls[0]
    expect(content).toBe('帮我复习鉴权')
    expect(retry).toBeNull()
    expect(fields.requestId).toBe(suggestion.requestId)
    expect(fields.topicConsent).toBeUndefined()
  })

  it('a session switch retires the question, its requestId and stale catalogue pages', async () => {
    let resolveBrowse
    const chat = fakeChat()
    const api = fakeApi({
      suggest: async () => candidates(),
      browse: () => new Promise((resolve) => { resolveBrowse = resolve }),
    })
    const flow = useTopicSelection(chat, api)
    flow.enabled.value = true

    await flow.submit()
    const firstRequest = api.suggestTopics.mock.calls[0][0]
    expect(flow.prompt.value.visible).toBe(true)

    // A catalogue page is in flight when the learner switches sessions.
    flow.openCatalogue()
    chat.activeId.value = 's2'
    await nextTick()
    expect(flow.prompt.value.visible).toBe(false)

    // The stale page must not repopulate the new session's empty prompt.
    resolveBrowse({ status_code: 1000, candidates: [{ topicKey: 'old-topic' }], nextOffset: 0, hasMore: false })
    await flushPromises()
    expect(flow.prompt.value.visible).toBe(false)
    expect(flow.prompt.value.candidates).toEqual([])

    // The next question is asked in the new session with a fresh requestId,
    // never the retired one.
    chat.draft.value = '第二个问题'
    await flow.submit()
    const secondRequest = api.suggestTopics.mock.calls[1][0]
    expect(secondRequest.sessionId).toBe('s2')
    expect(secondRequest.question).toBe('第二个问题')
    expect(secondRequest.requestId).not.toBe(firstRequest.requestId)
    expect(flow.prompt.value.visible).toBe(true)
    expect(flow.prompt.value.selected).toEqual([])
  })

  it('an empty suggestion list sends the question directly with no ticket', async () => {
    const chat = fakeChat()
    const api = fakeApi({ suggest: async () => ({ status_code: 1000, candidates: [], maxSelectable: 0 }) })
    const flow = useTopicSelection(chat, api)
    flow.enabled.value = true

    await flow.submit()
    expect(flow.prompt.value.visible).toBe(false)
    const suggestion = api.suggestTopics.mock.calls[0][0]
    expect(chat.send).toHaveBeenCalledWith('帮我复习鉴权', null, { requestId: suggestion.requestId })
    expect(api.issueTopicConsent).not.toHaveBeenCalled()
  })

  it('discards a suggestion response retired by A -> B -> A', async () => {
    let resolveSuggest
    const chat = fakeChat()
    const api = fakeApi({
      suggest: () => new Promise((resolve) => { resolveSuggest = resolve }),
    })
    const flow = useTopicSelection(chat, api)
    flow.enabled.value = true

    // The suggestion stays in flight; the attempt is retired underneath it.
    void flow.submit()
    await flushPromises()
    const staleRequest = api.suggestTopics.mock.calls[0][0]
    expect(staleRequest.sessionId).toBe('s1')

    // The learner leaves and comes back: the session id matches again, but the
    // attempt that asked for this suggestion is retired.
    chat.activeId.value = 's2'
    await nextTick()
    chat.activeId.value = 's1'
    await nextTick()

    resolveSuggest({ status_code: 1000, maxSelectable: 2, candidates: [{ topicKey: 't1' }] })
    await flushPromises()
    // The old panel must not come back, and the old question must not be sent.
    expect(flow.prompt.value.visible).toBe(false)
    expect(flow.prompt.value.candidates).toEqual([])
    expect(chat.send).not.toHaveBeenCalled()

    // A fresh question in the same session starts a new attempt.
    chat.draft.value = '回来以后的新问题'
    resolveSuggest = null
    api.suggestTopics.mockImplementation(async () => ({ status_code: 1000, maxSelectable: 2, candidates: [{ topicKey: 't1' }] }))
    await flow.submit()
    expect(flow.prompt.value.visible).toBe(true)
    expect(api.suggestTopics.mock.calls[1][0].requestId).not.toBe(staleRequest.requestId)
  })

  it('a retiring suggestion that resolves empty sends nothing', async () => {
    let resolveSuggest
    const chat = fakeChat()
    const api = fakeApi({
      suggest: () => new Promise((resolve) => { resolveSuggest = resolve }),
    })
    const flow = useTopicSelection(chat, api)
    flow.enabled.value = true

    void flow.submit()
    await flushPromises()
    chat.activeId.value = 's2'
    await nextTick()
    chat.activeId.value = 's1'
    await nextTick()
    resolveSuggest({ status_code: 1000, candidates: [], maxSelectable: 0 })
    await flushPromises()
    // An empty answer usually sends the question directly; a retired attempt
    // must not send it at all.
    expect(chat.send).not.toHaveBeenCalled()
    expect(flow.prompt.value.visible).toBe(false)
  })

  it('does not offer the flow on a deployment that never asks (default read mode)', async () => {
    const chat = fakeChat()
    const api = fakeApi({ capability: async () => ({ status_code: 1000, supported: false }) })
    const flow = useTopicSelection(chat, api)
    flow.enabled.value = true

    await flow.submit()
    expect(api.suggestTopics).not.toHaveBeenCalled()
    expect(chat.send).toHaveBeenCalledTimes(1)
  })

  it('offers the flow only on an agent session', async () => {
    const chat = fakeChat()
    chat.isAgentSession.value = false
    const api = fakeApi()
    const flow = useTopicSelection(chat, api)
    flow.enabled.value = true

    await flow.submit()
    expect(api.suggestTopics).not.toHaveBeenCalled()
    expect(chat.send).toHaveBeenCalledTimes(1)
    expect(chat.send.mock.calls[0]).toEqual([])
  })
})
