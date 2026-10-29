import { beforeEach, describe, expect, it, vi } from 'vitest'

import { authStore } from '../src/auth/store.js'
import {
  MEMORY_ERROR_COPY,
  MEMORY_FORM_COPY,
  resetMemoryAdapter,
  setMemoryAdapter,
} from '../src/api/memory.js'
import { createMockMemoryAdapter } from '../src/api/memoryMock.js'
import { resetMemoryState, useMemory } from '../src/composables/useMemory.js'
import { setTokenProvider } from '../src/api/http.js'

/**
 * The store on its own, with no component in the way: the sequencing rules that
 * the screen cannot reach by clicking live here.
 *
 * A superseded response is the one class of bug this page can show to the *wrong
 * person*, so it is the main subject: reads carry a generation and an abort
 * controller, writes carry an epoch, and a sign-out advances both.
 */

const memory = useMemory()
let adapter

function draftsOf(...ids) {
  return ids.map((id) => ({
    memory_id: id,
    scope: 'global',
    content: `草稿 ${id}`,
    confidence: 0.7,
    source_session_id: 'ses-1',
    source_message_ids: ['msg-1'],
    status: 'pending',
  }))
}

beforeEach(() => {
  vi.stubEnv('VITE_API_BASE_URL', 'https://api.test')
  setTokenProvider(() => 'tok.store')
  resetMemoryState()
  resetMemoryAdapter()
  adapter = createMockMemoryAdapter()
  setMemoryAdapter(adapter)
})

describe('reads', () => {
  it('loads both listings and partitions them by status', async () => {
    await memory.loadAll()

    expect(memory.pendingDrafts.value).toHaveLength(3)
    expect(memory.activeMemories.value).toHaveLength(3)
    // The retired row exists in the table but is not on the list, which is the
    // only reason the page can say it does not know the total.
    expect(adapter.snapshot().memories).toHaveLength(4)
  })

  it('keeps rows it already has when a refresh fails', async () => {
    await memory.loadAll()
    const loaded = memory.pendingDrafts.value

    adapter.failNext('listDrafts', { status: 500, code: 'internal_error', message: 'db down' })
    await memory.loadDrafts({ silent: true })

    expect(memory.pendingDrafts.value).toBe(loaded)
    expect(memory.draftsError.value).not.toBe('')
  })

  it('lets the later read win when two overlap', async () => {
    adapter = createMockMemoryAdapter({ latency: 12 })
    setMemoryAdapter(adapter)
    adapter.seedRows({ drafts: draftsOf('drf-second') })

    const first = memory.loadDrafts()
    await new Promise((resolve) => setTimeout(resolve, 2))
    const second = memory.loadDrafts()
    await Promise.all([first, second])

    expect(memory.pendingDrafts.value).toHaveLength(1)
    expect(memory.pendingDrafts.value[0].id).toBe('drf-second')
  })

  it('drops a read that was still running when the account changed', async () => {
    authStore.adopt('tok.a', { id: '1', email: 'a@example.test', displayName: '' })
    adapter = createMockMemoryAdapter({ latency: 12 })
    setMemoryAdapter(adapter)

    const running = memory.loadDrafts()
    await new Promise((resolve) => setTimeout(resolve, 2))
    authStore.signOut()
    await running

    expect(memory.pendingDrafts.value).toEqual([])
    expect(memory.draftsLoading.value).toBe(false)
  })
})

describe('writes', () => {
  it('refuses a second write while one is in flight', async () => {
    await memory.loadAll()
    const started = memory.confirmDraft(memory.pendingDrafts.value[0].id)

    expect(memory.mutating.value).toBe(true)
    const refused = await memory.rejectDraft(memory.pendingDrafts.value[1].id)
    expect(refused).toBe(false)

    await started
    expect(adapter.calls.filter((call) => call.operation === 'confirmDraft')).toHaveLength(1)
    expect(adapter.calls.filter((call) => call.operation === 'rejectDraft')).toHaveLength(0)
  })

  it('refuses a repeated click on the same row', async () => {
    await memory.loadAll()
    const id = memory.pendingDrafts.value[0].id
    const started = Promise.all([memory.confirmDraft(id), memory.confirmDraft(id)])

    expect((await started).filter(Boolean)).toHaveLength(1)
    expect(adapter.calls.filter((call) => call.operation === 'confirmDraft')).toHaveLength(1)
  })

  it('refreshes both listings after a write, without a spinner', async () => {
    await memory.loadAll()
    adapter.seedRows({ drafts: draftsOf('drf-x') })
    await memory.rejectDraft(memory.pendingDrafts.value[0].id)

    const writes = adapter.calls.filter((call) => call.operation === 'rejectDraft')
    const reads = adapter.calls.filter((call) => call.operation === 'listDrafts')
    expect(writes).toHaveLength(1)
    // initial load + the refresh the write asked for
    expect(reads).toHaveLength(2)
    expect(memory.pendingDrafts.value).toHaveLength(1)
    expect(memory.draftsLoading.value).toBe(false)
    expect(memory.mutating.value).toBe(false)
  })

  it('re-reads both listings instead of pretending after a not-found', async () => {
    await memory.loadAll()
    adapter.failNext('archiveMemory', { status: 404, code: 'not_found', message: 'gone' })
    const id = memory.activeMemories.value[0].id
    const before = adapter.calls.filter((call) => call.operation === 'listMemories').length
    const done = await memory.archiveMemory(id)

    expect(done).toBe(false)
    expect(memory.actionError.value).toContain('已经不在了')
    // The page did not decide the row's fate for the server: it asked again, and
    // whatever the listing says now is what stays on screen.
    expect(adapter.calls.filter((call) => call.operation === 'listMemories').length).toBe(
      before + 1,
    )
    expect(memory.memoriesLoading.value).toBe(false)
  })

  it('reports a retired row as the lifecycle answer it is', async () => {
    await memory.loadAll()
    const id = memory.activeMemories.value[0].id
    expect(await memory.archiveMemory(id)).toBe(true)

    // A second attempt at the same row is not a second success and not a generic
    // failure: the server says "that row is no longer in a state you can act on".
    const again = await memory.archiveMemory(id)
    expect(again).toBe(false)
    expect(memory.actionError.value).toBe(MEMORY_ERROR_COPY.notActiveRow)
  })

  it('clears the busy flag even when the write fails', async () => {
    await memory.loadAll()
    adapter.failNext('confirmDraft', { status: 500, code: 'internal_error', message: 'boom' })
    const done = await memory.confirmDraft(memory.pendingDrafts.value[0].id)

    expect(done).toBe(false)
    expect(memory.mutating.value).toBe(false)
    expect(memory.busyId.value).toBe('')
  })

  it('names the row being written so only its own button changes label', async () => {
    adapter = createMockMemoryAdapter({ latency: 8 })
    setMemoryAdapter(adapter)
    await memory.loadAll()
    const id = memory.pendingDrafts.value[1].id

    const running = memory.rejectDraft(id)
    expect(memory.busyId.value).toBe(id)
    await running
    expect(memory.busyId.value).toBe('')
  })

  it('counts an archive on this page rather than claiming a history total', async () => {
    await memory.loadAll()
    expect(memory.archivedHere.value).toBe(0)

    await memory.archiveMemory(memory.activeMemories.value[0].id)
    expect(memory.archivedHere.value).toBe(1)

    await memory.archiveMemory(memory.activeMemories.value[0].id)
    expect(memory.archivedHere.value).toBe(2)
  })

  it('refuses a write for a row it has never seen', async () => {
    await memory.loadAll()
    const calls = adapter.calls.length

    expect(await memory.confirmDraft('')).toBe(false)
    expect(await memory.archiveMemory(undefined)).toBe(false)
    expect(adapter.calls).toHaveLength(calls)
  })
})

describe('leaving an account', () => {
  it('wipes every part of the state, not just the arrays', async () => {
    await memory.loadAll()
    await memory.confirmDraft(memory.pendingDrafts.value[0].id)
    expect(memory.flash.value).not.toBe('')
    memory.clearFlash()
    memory.resetMemoryState()

    expect(memory.pendingDrafts.value).toEqual([])
    expect(memory.activeMemories.value).toEqual([])
    expect(memory.draftsError.value).toBe('')
    expect(memory.memoriesError.value).toBe('')
    expect(memory.draftsLoaded.value).toBe(false)
    expect(memory.memoriesLoaded.value).toBe(false)
    expect(memory.mutating.value).toBe(false)
    expect(memory.busyId.value).toBe('')
    expect(memory.flash.value).toBe('')
    expect(memory.archivedHere.value).toBe(0)
  })

  it('reloads cleanly for the next account', async () => {
    await memory.loadAll()
    expect(memory.pendingDrafts.value).toHaveLength(3)

    authStore.signOut()
    setMemoryAdapter(createMockMemoryAdapter({ drafts: draftsOf('drf-next'), memories: [] }))

    await memory.loadAll()
    expect(memory.pendingDrafts.value).toHaveLength(1)
    expect(memory.activeMemories.value).toEqual([])
  })
})

describe('the store as the single source', () => {
  it('hands every consumer the same lists and the same actions', async () => {
    await memory.loadAll()
    const second = useMemory()

    expect(second.pendingDrafts.value).toBe(memory.pendingDrafts.value)
    expect(second.loadAll).toBe(memory.loadAll)
    expect(second.archiveMemory).toBe(memory.archiveMemory)
  })
})

describe('the hand-written memory', () => {
  const VALID = { scope: 'global', content: '以后讲代码用 Go 语法。' }

  function callsOf(operation) {
    return adapter.calls.filter((call) => call.operation === operation)
  }

  it('does not spend a request on input the server would refuse', async () => {
    const outcomes = await Promise.all([
      memory.addMemory({ scope: 'global', content: '   ' }),
      memory.addMemory({ scope: 'topic', content: '要记住的事' }),
      memory.addMemory({ scope: 'topic', content: '要记住的事', topicKey: '  ' }),
    ])

    for (const outcome of outcomes) {
      expect(outcome.added).toBe(false)
      expect(Object.keys(outcome.errors).length).toBeGreaterThan(0)
    }
    expect(callsOf('addMemory')).toHaveLength(0)
  })

  it('lands in the active list, not the pending one, and confirms nothing', async () => {
    adapter.seedRows({ drafts: [], memories: [] })
    const draftsBefore = memory.pendingDrafts.value.length

    const outcome = await memory.addMemory(VALID)

    expect(outcome).toEqual({ added: true, errors: {} })
    expect(memory.pendingDrafts.value).toHaveLength(draftsBefore)
    expect(memory.activeMemories.value.map((item) => item.content)).toEqual([
      '以后讲代码用 Go 语法。',
    ])
    expect(memory.activeMemories.value[0]).toMatchObject({ status: 'active', confidence: 1 })
    expect(memory.flash.value).toBe(MEMORY_FORM_COPY.added)
    // The gate is the agent's path, not this one: nothing here may confirm a draft.
    expect(callsOf('confirmDraft')).toHaveLength(0)
  })

  it('replaces the previous active statement about the same subject', async () => {
    adapter.seedRows({ drafts: [], memories: [] })
    await memory.addMemory({ scope: 'topic', topicKey: 'redis', content: 'AOF 用 everysec' })
    await memory.addMemory({ scope: 'topic', topicKey: 'redis', content: 'AOF 改用 always' })

    // One live row for that subject, and the server's list — not the client's hope
    // — is what says so.
    expect(memory.activeMemories.value.map((item) => item.content)).toEqual([
      'AOF 改用 always',
    ])
    expect(callsOf('addMemory')).toHaveLength(2)
  })

  it('shares one flight with the transitions, so an add and a confirm cannot interleave', async () => {
    adapter = createMockMemoryAdapter({ drafts: draftsOf('drf-a'), memories: [], latency: 15 })
    setMemoryAdapter(adapter)
    const adding = memory.addMemory(VALID)
    await Promise.resolve()

    expect(await memory.confirmDraft('drf-a')).toBe(false)
    expect(memory.busyId.value).toBe('__add__')

    await adding
    expect(memory.mutating.value).toBe(false)
    expect(callsOf('confirmDraft')).toHaveLength(0)
  })

  it('reports the refusal and leaves the lists alone', async () => {
    adapter.seedRows({ drafts: [], memories: [] })
    adapter.failNext('addMemory', { status: 503, code: 'unavailable', message: 'db down' })

    const outcome = await memory.addMemory(VALID)

    expect(outcome.added).toBe(false)
    expect(memory.actionError.value).toBe(MEMORY_ERROR_COPY.unavailable)
    expect(memory.activeMemories.value).toEqual([])
    expect(memory.flash.value).toBe('')
  })

  it('drops a memory added by the account that just signed out', async () => {
    adapter = createMockMemoryAdapter({ drafts: [], memories: [], latency: 15 })
    setMemoryAdapter(adapter)
    authStore.adopt('tok.a', { id: '1', email: 'a@example.test', displayName: '' })
    await memory.loadAll()

    const adding = memory.addMemory(VALID)
    await Promise.resolve()
    authStore.signOut()
    const outcome = await adding

    expect(outcome.added).toBe(false)
    expect(memory.activeMemories.value).toEqual([])
    expect(memory.flash.value).toBe('')
  })

  it('reaches no conversation API at all: the form asks for no source', async () => {
    let reached = 0
    globalThis.fetch = async () => {
      reached += 1
      throw new Error('the memory page must not read sessions or messages')
    }
    resetMemoryAdapter()
    setMemoryAdapter(createMockMemoryAdapter())

    await memory.loadAll()

    expect(reached).toBe(0)
  })
})
