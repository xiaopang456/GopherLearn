import { beforeEach, describe, expect, it, vi } from 'vitest'
import { nextTick } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { createMemoryHistory } from 'vue-router'

import MemoryView from '../src/views/MemoryView.vue'
import { authStore } from '../src/auth/store.js'
import { createAppRouter } from '../src/router/index.js'
import {
  MEMORY_ERROR_COPY,
  resetMemoryAdapter,
  setMemoryAdapter,
} from '../src/api/memory.js'
import { createMockMemoryAdapter } from '../src/api/memoryMock.js'
import { resetMemoryState, useMemory } from '../src/composables/useMemory.js'
import { setTokenProvider } from '../src/api/http.js'

/**
 * The memory screen, driven end to end through the adapter seam.
 *
 * The mock adapter stands where a server will eventually stand, so every case
 * below exercises the real store, the real normalizers and the real component:
 * what is faked is only the network. The assertions that matter are the ones a
 * fake can hide nothing from — which row moved, how many requests it cost, and
 * exactly what the page said when it could not do the thing.
 */

const memory = useMemory()
let adapter

function mountPage(options = {}) {
  return mount(MemoryView, options)
}

async function openPage(options = {}) {
  const app = mountPage(options)
  await flushPromises()
  await flushPromises()
  return app
}

/** Let a latency-bearing adapter actually finish. */
async function rest(ms = 30) {
  await new Promise((resolve) => setTimeout(resolve, ms))
  await flushPromises()
  await flushPromises()
}

function draftCards(app) {
  return app.findAll('[data-test="draft-card"]')
}

function memoryCards(app) {
  return app.findAll('[data-test="memory-card"]')
}

function confirmButtons(app) {
  return app.findAll('[data-test="confirm-draft"]')
}

function contentsOf(nodes) {
  return nodes.map((card) => card.find('.mem__content').text())
}

function readsOf(operation) {
  return adapter.calls.filter((call) => call.operation === operation).length
}

beforeEach(() => {
  vi.stubEnv('VITE_API_BASE_URL', 'https://api.test')
  setTokenProvider(() => 'tok.memory')
  resetMemoryState()
  resetMemoryAdapter()
  adapter = createMockMemoryAdapter()
  setMemoryAdapter(adapter)
})

describe('the pending section', () => {
  it('says there is nothing waiting instead of showing sample rows', async () => {
    adapter.seedRows({ drafts: [] })
    const app = await openPage()

    expect(app.find('[data-test="drafts-empty"]').exists()).toBe(true)
    expect(draftCards(app)).toHaveLength(0)
    expect(app.text()).not.toContain('示例')
    app.unmount()
  })

  it('never calls the inbox empty before it has read the real one', async () => {
    const app = mountPage()

    // The first paint happens before any response exists, so the only honest
    // thing it can say is that it is still reading.
    expect(app.find('[data-test="drafts-loading"]').exists()).toBe(true)
    expect(app.find('[data-test="drafts-empty"]').exists()).toBe(false)
    expect(app.find('[data-test="memories-loading"]').exists()).toBe(true)
    expect(app.find('[data-test="memories-empty"]').exists()).toBe(false)

    await flushPromises()
    await flushPromises()

    expect(app.find('[data-test="drafts-loading"]').exists()).toBe(false)
    expect(draftCards(app)).toHaveLength(3)
    app.unmount()
  })

  it('shows every field a confirmation decision needs', async () => {
    const app = await openPage()
    const topic = draftCards(app).find((card) =>
      card.text().includes('有缓冲 channel 的丢弃语义还需要再确认一次。'),
    )
    const text = topic.text()

    expect(text).toContain('主题记忆')
    expect(text).toContain('channel-buffer')
    expect(text).toContain('置信度 0.64')
    // One citing session and every message id inside it, as the listing reports
    // them.
    expect(text).toContain('ses-118')
    expect(text).toContain('msg-118-3')
    expect(text).toContain('msg-118-4')
    app.unmount()
  })

  it('says a draft arrived without provenance rather than printing a blank', async () => {
    const app = await openPage()
    const episode = draftCards(app).find((card) =>
      card.text().includes('这一段对话里你两次要求把例子换成真实代码。'),
    )

    expect(episode.text()).toContain('未记录')
    expect(episode.text()).toContain('情景记忆')
    app.unmount()
  })

  it('labels the header with where the data actually comes from', async () => {
    const app = await openPage()

    expect(app.text()).toContain('演示数据')
    expect(app.find('[data-test="memory-demo-note"]').exists()).toBe(true)
    app.unmount()
  })

  it('reports a failed read in its own words and keeps the server text out', async () => {
    adapter.failNext('listDrafts', {
      status: 500,
      code: 'internal_error',
      message: 'redis: OOM command not allowed when used memory > maxmemory',
    })
    const app = await openPage()

    expect(app.text()).toContain(MEMORY_ERROR_COPY.unavailable)
    expect(app.text()).not.toContain('redis')
    expect(app.text()).not.toContain('maxmemory')
    expect(draftCards(app)).toHaveLength(0)
    app.unmount()
  })
})

describe('confirming a draft', () => {
  it('moves it out of pending and into the active list', async () => {
    const app = await openPage()
    const before = memoryCards(app).length

    await confirmButtons(app)[1].trigger('click')
    await flushPromises()
    await flushPromises()

    // The same sentence is now an active memory, so what has to be checked is
    // which section carries it, not whether the page still mentions it.
    expect(contentsOf(draftCards(app))).not.toEqual(
      expect.arrayContaining([
        expect.stringContaining('有缓冲 channel 的丢弃语义还需要再确认一次。'),
      ]),
    )
    expect(contentsOf(memoryCards(app)).join('|')).toContain('channel 的丢弃语义')
    expect(memoryCards(app)).toHaveLength(before + 1)
    app.unmount()
  })

  it('says which transition happened, in a live region', async () => {
    const app = await openPage()
    await confirmButtons(app)[0].trigger('click')
    await flushPromises()
    await flushPromises()

    const flash = app.find('[data-test="memory-flash"]')
    expect(flash.exists()).toBe(true)
    expect(flash.attributes('role')).toBe('status')
    expect(flash.text()).toBe('已确认，这条记忆现在生效。')
    app.unmount()
  })

  it('asks once even when the button is pressed twice, and locks while working', async () => {
    adapter = createMockMemoryAdapter({ latency: 10 })
    setMemoryAdapter(adapter)
    const app = await openPage()
    await rest()

    await confirmButtons(app)[0].trigger('click')
    expect(app.find('[data-test="confirm-draft"]').attributes('disabled')).toBeDefined()
    // A second press while the first is still in flight must be refused by the
    // store too, in case the DOM ever lets it through.
    expect(await memory.confirmDraft('drf-profile-1')).toBe(false)
    await rest()

    expect(readsOf('confirmDraft')).toBe(1)
    app.unmount()
  })

  it('keeps the draft when the server refuses, and explains it', async () => {
    adapter.failNext('confirmDraft', {
      status: 400,
      code: 'invalid_request',
      message: 'confidence below floor',
    })
    const app = await openPage()
    const before = draftCards(app).length

    await confirmButtons(app)[0].trigger('click')
    await flushPromises()
    await flushPromises()

    expect(app.find('[data-test="memory-action-error"]').text()).toContain(
      MEMORY_ERROR_COPY.invalidRequest,
    )
    expect(app.text()).not.toContain('below floor')
    expect(draftCards(app)).toHaveLength(before)
    expect(app.find('[data-test="confirm-draft"]').attributes('disabled')).toBeUndefined()
    app.unmount()
  })

  it('re-reads the lists when the row it was shown is gone', async () => {
    adapter = createMockMemoryAdapter({
      drafts: [
        { memory_id: 'drf-stale', scope: 'global', content: '已经被处理过了。', status: 'pending' },
      ],
      memories: [],
    })
    setMemoryAdapter(adapter)
    const app = await openPage()
    expect(draftCards(app)).toHaveLength(1)

    adapter.failNext('confirmDraft', { status: 404, code: 'not_found', message: 'gone' })
    await confirmButtons(app)[0].trigger('click')
    await flushPromises()
    await flushPromises()

    expect(app.find('[data-test="memory-action-error"]').text()).toContain(
      MEMORY_ERROR_COPY.notFound,
    )
    expect(readsOf('listDrafts')).toBe(2)
    expect(app.text()).not.toContain('gone')
    app.unmount()
  })

  it('names a retired row with the lifecycle sentence that fits it', async () => {
    adapter.failNext('confirmDraft', {
      status: 409,
      code: 'memory_already_confirmed',
      message: 'draft 42 confirmed at 09:00',
    })
    const app = await openPage()
    await confirmButtons(app)[0].trigger('click')
    await flushPromises()
    await flushPromises()

    expect(app.find('[data-test="memory-action-error"]').text()).toContain(
      MEMORY_ERROR_COPY.alreadyConfirmed,
    )
    expect(app.text()).not.toContain('draft 42')
    app.unmount()
  })
})

describe('rejecting a draft', () => {
  it('makes it disappear from pending without creating a memory', async () => {
    const app = await openPage()
    const draftsBefore = draftCards(app).length
    const memoriesBefore = memoryCards(app).length

    await app.findAll('[data-test="reject-draft"]')[0].trigger('click')
    await flushPromises()
    await flushPromises()

    expect(draftCards(app)).toHaveLength(draftsBefore - 1)
    expect(app.text()).not.toContain('解释概念时先给结论，再给一个最小例子。')
    expect(memoryCards(app)).toHaveLength(memoriesBefore)
    expect(app.find('[data-test="memory-flash"]').text()).toBe('已拒绝，这条草稿不会生效。')
    app.unmount()
  })
})

describe('the active section', () => {
  it('tells the three scopes apart', async () => {
    const app = await openPage()
    const scopes = memoryCards(app).map((card) => card.find('.mem__scope').text())

    expect(scopes).toEqual(['全局画像', '主题记忆', '情景记忆'])
    app.unmount()
  })

  it('shows confidence, provenance and a creation time for each one', async () => {
    const app = await openPage()
    const card = memoryCards(app).find((row) => row.text().includes('gin-routing'))

    expect(card.text()).toContain('置信度 0.74')
    expect(card.text()).toContain('ses-83')
    expect(card.text()).toContain('msg-83-11')
    // The stamp is rendered in the reader's own time zone, so only the year is a
    // fair assertion here.
    expect(card.text()).toMatch(/2026/)
    app.unmount()
  })

  it('offers archive as the only action and never an editor', async () => {
    const app = await openPage()
    const card = memoryCards(app)[0]

    expect(card.findAll('[data-test="archive-memory"]')).toHaveLength(1)
    expect(card.text()).not.toContain('编辑')
    // The claim is about the rows: no listed memory may carry an input of its own.
    // The page does have one form — the hand-written draft below — so this is
    // asserted per card, and the count of forms is asserted here too so an
    // "edit memory" form cannot arrive later and pass unnoticed.
    expect(card.find('input').exists()).toBe(false)
    expect(card.find('textarea').exists()).toBe(false)
    for (const draft of draftCards(app)) {
      expect(draft.find('input').exists()).toBe(false)
      expect(draft.find('textarea').exists()).toBe(false)
    }
    expect(app.findAll('form')).toHaveLength(1)
    expect(app.find('[data-test="memory-add-form"]').exists()).toBe(true)
    app.unmount()
  })

  it('removes an archived row from the active list', async () => {
    const app = await openPage()
    const before = memoryCards(app).length

    await app.find('[data-test="archive-memory"]').trigger('click')
    await flushPromises()
    await flushPromises()

    expect(memoryCards(app)).toHaveLength(before - 1)
    expect(app.text()).not.toContain('回答控制在一段以内，除非我让你展开。')
    app.unmount()
  })

  it('lists only rows it can name as active, and never guesses the rest in', async () => {
    adapter.seedRows({
      drafts: [],
      memories: [
        { memory_id: 'a', scope: 'global', status: 'active', content: '生效中的那条。' },
        { memory_id: 'b', scope: 'global', status: 'archived', content: '已归档的那条。' },
        { memory_id: 'c', scope: 'topic', status: 'pending_review', content: '服务端发明了状态。' },
        { memory_id: 'd', scope: 'topic', content: '根本没有状态。' },
      ],
    })
    const app = await openPage()

    expect(memoryCards(app)).toHaveLength(1)
    expect(app.text()).toContain('生效中的那条。')
    expect(app.text()).not.toContain('服务端发明了状态。')
    expect(app.text()).not.toContain('已归档的那条。')
    app.unmount()
  })
})

describe('the history section', () => {
  it('says what the listing cannot show, without inventing a total', async () => {
    const app = await openPage()
    const note = app.find('[data-test="history-note"]').text().replace(/\s+/g, '')

    expect(note).toContain('归档与被替代的记录不在这个列表里')
    expect(note).not.toMatch(/已归档\s*\d+\s*条/)
    expect(app.find('[data-test="history-count"]').exists()).toBe(false)
    // The retired seed row is in the table but never on screen.
    expect(app.text()).not.toContain('这条已经不再带入回答。')
    app.unmount()
  })

  it('counts the archives this page performed, and only those', async () => {
    const app = await openPage()
    await app.find('[data-test="archive-memory"]').trigger('click')
    await flushPromises()
    await flushPromises()

    expect(app.find('[data-test="history-count"]').text()).toContain('你在本页归档了 1 条。')
    app.unmount()
  })

  it('says it cannot speak about history when the list did not read', async () => {
    adapter.failNext('listMemories', {
      status: 500,
      code: 'internal_error',
      times: 4,
      message: 'db down',
    })
    const app = await openPage()

    expect(app.find('[data-test="history-note"]').text()).toContain('给不出任何归档说明')
    app.unmount()
  })
})

describe('a rejected credential', () => {
  async function openWithRouter() {
    const router = createAppRouter({ authStore, history: createMemoryHistory() })
    await router.push('/memory')
    await router.isReady()
    const app = mount(MemoryView, { global: { plugins: [router] } })
    return { app, router }
  }

  it('ends the session on a failed read and moves to the sign-in screen', async () => {
    authStore.adopt('tok.dead', { id: '1', email: 'who@example.test', displayName: '' })
    adapter.failNext('listDrafts', {
      status: 401,
      code: 'unauthorized',
      message: 'token has expired',
    })

    const { app, router } = await openWithRouter()
    await flushPromises()
    await rest()

    expect(authStore.isAuthenticated()).toBe(false)
    expect(router.currentRoute.value.name).toBe('login')
    expect(router.currentRoute.value.query.redirect).toBe('/memory')
    expect(app.text()).not.toContain('token has expired')
    app.unmount()
  })

  it('ends the session on a rejected write too', async () => {
    authStore.adopt('tok.dead', { id: '1', email: 'who@example.test', displayName: '' })
    adapter.failNext('confirmDraft', {
      status: 401,
      code: 'unauthorized',
      message: 'bearer rejected',
    })

    const { app, router } = await openWithRouter()
    await rest()
    await confirmButtons(app)[0].trigger('click')
    await rest()

    expect(authStore.isAuthenticated()).toBe(false)
    expect(router.currentRoute.value.name).toBe('login')
    expect(app.text()).not.toContain('bearer rejected')
    app.unmount()
  })

  it('is not what a foreign record looks like', async () => {
    authStore.adopt('tok.alive', { id: '1', email: 'who@example.test', displayName: '' })
    adapter.failNext('listDrafts', {
      status: 403,
      code: 'forbidden',
      message: 'draft belongs to user 7',
    })
    const app = await openPage()

    expect(authStore.isAuthenticated()).toBe(true)
    expect(app.text()).toContain(MEMORY_ERROR_COPY.crossAccount)
    expect(app.text()).not.toContain('user 7')
    app.unmount()
  })
})

describe('failure copy is one sentence per kind of failure', () => {
  const cases = [
    [
      '400',
      { status: 400, code: 'invalid_request', message: 'topic_key is required for scope topic' },
      MEMORY_ERROR_COPY.invalidRequest,
    ],
    ['404', { status: 404, code: 'not_found', message: 'SELECT returned no rows' }, MEMORY_ERROR_COPY.notFound],
    [
      '500',
      { status: 500, code: 'internal_error', message: 'dial tcp 10.0.3.7:3306: connect refused' },
      MEMORY_ERROR_COPY.unavailable,
    ],
  ]

  it.each(cases)('%s reads as its own sentence and leaks nothing', async (_status, fault, copy) => {
    adapter.failNext('listMemories', fault)
    const app = await openPage()

    expect(app.text()).toContain(copy)
    expect(app.text()).not.toContain(fault.message)
    expect(memoryCards(app)).toHaveLength(0)
    app.unmount()
  })

  it('keeps a list failure and an action failure apart on screen', async () => {
    // `times: 3` holds the read in a failed state across the silent refresh that
    // follows the write, so the two notices genuinely coexist: one belongs to the
    // section that could not be read, the other to the button that was refused.
    adapter.failNext('listMemories', {
      status: 500,
      code: 'internal_error',
      message: 'boom',
      times: 3,
    })
    adapter.failNext('confirmDraft', { status: 403, code: 'forbidden', message: 'not yours' })
    const app = await openPage()

    await confirmButtons(app)[0].trigger('click')
    await flushPromises()
    await flushPromises()

    expect(app.find('[data-test="memory-action-error"]').text()).toContain(
      MEMORY_ERROR_COPY.crossAccount,
    )
    expect(app.text()).toContain(MEMORY_ERROR_COPY.unavailable)
    expect(app.text()).not.toContain('boom')
    expect(app.text()).not.toContain('not yours')
    app.unmount()
  })
})

describe('an environment with no memory service', () => {
  it('shows no records and says the interface is not open', async () => {
    resetMemoryAdapter()
    const app = await openPage()

    expect(app.text()).toContain(MEMORY_ERROR_COPY.notWired)
    expect(app.text()).toContain('接口未开通')
    expect(draftCards(app)).toHaveLength(0)
    expect(memoryCards(app)).toHaveLength(0)
    expect(app.text()).toContain('给不出任何归档说明')
    app.unmount()
  })

  it('does not reach the network for it', async () => {
    let reached = 0
    globalThis.fetch = async () => {
      reached += 1
      throw new Error('must not be called')
    }
    resetMemoryAdapter()
    await openPage()

    expect(reached).toBe(0)
  })
})

describe('leaving an account', () => {
  it('wipes the lists, and a response that arrives later cannot refill them', async () => {
    adapter = createMockMemoryAdapter({ latency: 10 })
    setMemoryAdapter(adapter)
    authStore.adopt('tok.a', { id: '1', email: 'a@example.test', displayName: '' })
    const app = await openPage()
    await rest()
    expect(draftCards(app)).toHaveLength(3)

    authStore.signOut()
    await flushPromises()

    expect(memory.pendingDrafts.value).toEqual([])
    expect(memory.activeMemories.value).toEqual([])
    expect(memory.mutating.value).toBe(false)
    expect(memory.flash.value).toBe('')
    expect(memory.actionError.value).toBe('')
    // A sign-out stops asking, so nothing is re-read once the account is gone.
    expect(readsOf('listDrafts')).toBe(1)
    app.unmount()
  })

  it('refuses to finish a mutation started by the account that just left', async () => {
    adapter = createMockMemoryAdapter({ latency: 10 })
    setMemoryAdapter(adapter)
    authStore.adopt('tok.a', { id: '1', email: 'a@example.test', displayName: '' })
    const app = await openPage()
    await rest()

    const running = memory.confirmDraft('drf-profile-1')
    authStore.signOut()
    const applied = await running

    expect(applied).toBe(false)
    expect(memory.pendingDrafts.value).toEqual([])
    expect(memory.activeMemories.value).toEqual([])
    app.unmount()
  })

  it('hands a fresh account an empty screen until it loads its own data', async () => {
    const app = await openPage()
    expect(memoryCards(app)).toHaveLength(3)
    expect(app.text()).toContain('Gin 的路由分组和中间件顺序你已经讲清楚了。')
    app.unmount()

    authStore.signOut()
    expect(memory.pendingDrafts.value).toEqual([])
    expect(memory.activeMemories.value).toEqual([])

    setMemoryAdapter(
      createMockMemoryAdapter({
        drafts: [{ memory_id: 'drf-b', scope: 'episodic', content: '下一个账号的草稿' }],
        memories: [],
      }),
    )

    const second = await openPage()
    expect(memoryCards(second)).toHaveLength(0)
    expect(second.text()).not.toContain('Gin 的路由分组')
    expect(draftCards(second)).toHaveLength(1)
    second.unmount()
  })
})

describe('what this page must not claim', () => {
  const FORBIDDEN = [
    '自动抽取',
    '自动总结',
    '自动保存',
    '会自动记住',
    '会自动写入',
    '替你决定',
    '掌握度',
    '主题授权',
    '跨会话',
    '出题',
    '判分',
    '薄弱点',
    '尚未接入记忆读写接口',
    'Phase 2 待接入',
  ]

  it('says nothing about capabilities this build does not have', async () => {
    const app = await openPage()
    const text = app.text()

    for (const phrase of FORBIDDEN) expect(text).not.toContain(phrase)
    app.unmount()
  })

  it('still has to be a real document: headings, labels and a button per action', async () => {
    const app = await openPage()

    expect(app.findAll('h2').length).toBeGreaterThanOrEqual(3)
    expect(app.find('h1').exists()).toBe(true)
    expect(app.findAll('dt').length).toBeGreaterThan(0)
    for (const button of app.findAll('button')) {
      // Exactly one button on this page may submit: the create form's own, because
      // the form exists to be submitted and Enter has to work. Every other action
      // must be an explicit `type="button"` so no click can silently ride a form.
      if (button.attributes('data-test') === 'add-submit') {
        expect(button.attributes('type')).toBe('submit')
        expect(button.text().length).toBeGreaterThan(0)
        continue
      }
      expect(button.attributes('type')).toBe('button')
      const spoken = button.text() + (button.attributes('aria-label') ?? '')
      expect(spoken.length).toBeGreaterThan(0)
    }
    // Every action a screen reader can reach must say what it acts on — and say
    // it once, without the scope label and the noun both spelling "memory".
    expect(confirmButtons(app)[0].attributes('aria-label')).toBe('确认这条全局画像草稿')
    expect(
      app.findAll('[data-test="archive-memory"]').map((button) => button.attributes('aria-label')),
    ).toEqual(['归档全局画像', '归档主题记忆', '归档情景记忆'])
    app.unmount()
  })

  it('marks the lists busy while a write is in flight', async () => {
    adapter = createMockMemoryAdapter({ latency: 10 })
    setMemoryAdapter(adapter)
    const app = await openPage()
    await rest()

    const running = memory.confirmDraft('drf-profile-1')
    await nextTick()
    expect(app.find('[data-test="draft-list"]').attributes('aria-busy')).toBe('true')

    await running
    await nextTick()
    expect(app.find('[data-test="draft-list"]').attributes('aria-busy')).toBeUndefined()
    app.unmount()
  })
})

describe('the hand-written memory form', () => {
  async function fill(app, { scope, topicKey, content } = {}) {
    if (scope) {
      const radio = app
        .findAll('[data-test="add-scope"]')
        .find((input) => input.element.value === scope)
      await radio.setValue(true)
    }
    if (topicKey !== undefined) await app.find('[data-test="add-topic-key"]').setValue(topicKey)
    if (content !== undefined) await app.find('[data-test="add-content"]').setValue(content)
  }

  async function submit(app) {
    await app.find('[data-test="memory-add-form"]').trigger('submit')
    await rest()
  }

  it('asks for nothing but the type, the key and the sentence', async () => {
    const app = await openPage()

    expect(app.findAll('[data-test="add-scope"]').map((input) => input.element.value)).toEqual([
      'global',
      'topic',
    ])
    expect(app.find('[data-test="add-topic-key"]').exists()).toBe(false)
    await fill(app, { scope: 'topic' })
    expect(app.find('[data-test="add-topic-key"]').exists()).toBe(true)

    // The form asks for no source. Scoped to the form on purpose: an agent's draft
    // still shows the conversation it came from, and that label belongs there.
    const form = app.find('[data-test="memory-add-form"]')
    expect(form.find('select').exists()).toBe(false)
    expect(form.text()).not.toContain('来源会话')
    expect(form.text()).not.toContain('来源消息')
    expect(readsOf('addMemory')).toBe(0)
    app.unmount()
  })

  it('refuses an incomplete form without spending a request', async () => {
    adapter.seedRows({ drafts: [], memories: [] })
    const app = await openPage()

    await submit(app)
    expect(readsOf('addMemory')).toBe(0)
    expect(app.find('[data-test="add-error-content"]').text()).toContain('请填写要记住的内容')

    await fill(app, { scope: 'topic', content: '这个主题用 Go 示例。' })
    await submit(app)
    expect(readsOf('addMemory')).toBe(0)
    expect(app.find('[data-test="add-error-topic"]').text()).toContain('主题标识')
    app.unmount()
  })

  it('adds a global memory straight into the live list', async () => {
    adapter.seedRows({ drafts: [], memories: [] })
    const app = await openPage()

    await fill(app, { content: '以后讲代码时使用 Go 语法。' })
    await submit(app)

    const cards = memoryCards(app)
    expect(cards).toHaveLength(1)
    expect(cards[0].find('[data-test="memory-content"]').text()).toBe(
      '以后讲代码时使用 Go 语法。',
    )
    expect(cards[0].text()).toContain('全局画像')
    expect(cards[0].text()).toContain('1.00')
    expect(app.find('[data-test="memory-flash"]').text()).toContain('已添加')
    // It is in force, so it never appears where a decision is still owed.
    expect(draftCards(app)).toHaveLength(0)
    expect(app.find('[data-test="add-content"]').element.value).toBe('')
    app.unmount()
  })

  it('sends the topic key for a topic memory and shows it on the row', async () => {
    adapter.seedRows({ drafts: [], memories: [] })
    const app = await openPage()

    await fill(app, { scope: 'topic', topicKey: 'go', content: '这个主题使用 Go 示例。' })
    await submit(app)

    const card = memoryCards(app)[0]
    expect(card.text()).toContain('主题记忆')
    expect(card.findAll('.mem__mono').map((node) => node.text())).toContain('go')
    expect(readsOf('addMemory')).toBe(1)
    app.unmount()
  })

  it('costs one request however many times the form is submitted', async () => {
    adapter = createMockMemoryAdapter({ drafts: [], memories: [], latency: 20 })
    setMemoryAdapter(adapter)
    const app = await openPage()
    await fill(app, { content: '只提交一次。' })

    const form = app.find('[data-test="memory-add-form"]')
    const first = form.trigger('submit')
    await nextTick()
    const second = form.trigger('submit')
    await Promise.all([first, second])
    await rest(60)

    expect(readsOf('addMemory')).toBe(1)
    expect(memoryCards(app)).toHaveLength(1)
    app.unmount()
  })

  it('keeps every keystroke when the server refuses', async () => {
    adapter.seedRows({ drafts: [], memories: [] })
    adapter.failNext('addMemory', { status: 503, code: 'unavailable', message: 'db down' })
    const app = await openPage()

    await fill(app, { content: '这条写不出去。' })
    await submit(app)

    expect(app.find('[data-test="add-content"]').element.value).toBe('这条写不出去。')
    expect(app.find('[data-test="memory-action-error"]').text()).toContain(
      MEMORY_ERROR_COPY.unavailable,
    )
    expect(memoryCards(app)).toHaveLength(0)
    app.unmount()
  })

  it('says the memory is live and keeps the agent draft path honest', async () => {
    const app = await openPage()

    expect(app.text()).toContain('添加后立即生效')
    // The pending section still belongs to the agent's proposals, and this form
    // must not claim that those arrive live.
    expect(app.find('[data-test="drafts-empty"], [data-test="draft-list"]').exists()).toBe(true)
    expect(app.text()).not.toContain('自动抽取')
    app.unmount()
  })

  it('is not offered when the memory service is unbound', async () => {
    resetMemoryAdapter()
    const app = await openPage()

    expect(app.find('[data-test="memory-add-form"]').exists()).toBe(false)
    app.unmount()
  })
})
