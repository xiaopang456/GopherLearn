import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'

import ComposerBox from '../src/components/ComposerBox.vue'

const COLLECTIONS = [
  { id: 'collection-1', title: '操作系统笔记' },
  { id: 'collection-2', title: '并发编程' },
]

const OFF = { enabled: false, selectedCollectionIds: [] }

function mountComposer(overrides = {}) {
  return mount(ComposerBox, {
    props: {
      modelValue: '一个问题',
      mode: 'chat',
      knowledge: OFF,
      collections: [],
      canUseKnowledge: false,
      ...overrides,
    },
  })
}

function tools(app) {
  return app.find('[data-test="knowledge-tools"]').exists()
}

describe('which composer gets the grounding toolbar', () => {
  it('shows it for a chat session that has collections', () => {
    const app = mountComposer({ mode: 'chat', collections: COLLECTIONS, canUseKnowledge: true })
    expect(tools(app)).toBe(true)
    expect(app.find('[data-test="knowledge-toggle"]').text()).toContain('使用知识库')
    expect(app.find('[data-test="knowledge-pick"]').exists()).toBe(true)
  })

  it('never shows it for a learning agent session', () => {
    const app = mountComposer({ mode: 'learning_agent', collections: COLLECTIONS, canUseKnowledge: true })
    expect(tools(app)).toBe(false)
    expect(app.text()).not.toContain('使用知识库')
  })

  it('withholds it while the mode is unrecognised', () => {
    // Guessing "agent" or guessing "chat with knowledge" would both be a claim
    // the client has no authority to make.
    expect(tools(mountComposer({ mode: '', collections: COLLECTIONS, canUseKnowledge: true }))).toBe(false)
    expect(tools(mountComposer({ mode: 'voice_beta', collections: COLLECTIONS, canUseKnowledge: true }))).toBe(false)
  })
})

describe('the toolbar with nothing to ground with', () => {
  it('is not rendered at all, rather than rendered dead', () => {
    const app = mountComposer({ mode: 'chat' })

    // The product does not serve knowledge collections yet. A switch that can
    // never be turned on — pointing at a page that cannot enable it — reads as
    // a broken screen, so the controls wait until there is something to ground
    // with. They come back on their own once the backend offers a collection.
    expect(tools(app)).toBe(false)
    expect(app.text()).not.toContain('使用知识库')
    expect(app.text()).not.toContain('添加知识库后可启用')
  })

  it('sends nothing about knowledge when it is absent', async () => {
    const app = mountComposer({ mode: 'chat' })
    expect(app.emitted('update:knowledge')).toBeUndefined()
  })
})

describe('the toolbar with real collections', () => {
  it('enables grounding and reports a selection', async () => {
    const app = mountComposer({
      mode: 'chat',
      collections: COLLECTIONS,
      canUseKnowledge: true,
    })

    await app.find('[data-test="knowledge-toggle"]').trigger('click')
    expect(app.emitted('update:knowledge').at(-1)[0]).toEqual({
      enabled: true,
      selectedCollectionIds: [],
    })
  })

  it('switching off clears the selection rather than parking it', async () => {
    const app = mountComposer({
      mode: 'chat',
      knowledge: { enabled: true, selectedCollectionIds: ['collection-1'] },
      collections: COLLECTIONS,
      canUseKnowledge: true,
    })

    await app.find('[data-test="knowledge-toggle"]').trigger('click')
    expect(app.emitted('update:knowledge').at(-1)[0]).toEqual(OFF)
  })

  it('lists exactly the collections it was given', async () => {
    const app = mountComposer({
      mode: 'chat',
      knowledge: { enabled: true, selectedCollectionIds: ['collection-1'] },
      collections: COLLECTIONS,
      canUseKnowledge: true,
    })

    await app.find('[data-test="knowledge-pick"]').trigger('click')
    const options = app.findAll('.tool__option')
    expect(options).toHaveLength(2)
    expect(options.map((node) => node.text()).join('|')).toContain('操作系统笔记')
    expect(app.html()).not.toContain('collection-forged')
  })

  it('reports a toggled collection without inventing an enabled flag', async () => {
    const app = mountComposer({
      mode: 'chat',
      knowledge: { enabled: true, selectedCollectionIds: ['collection-1'] },
      collections: COLLECTIONS,
      canUseKnowledge: true,
    })

    await app.find('[data-test="knowledge-pick"]').trigger('click')
    await app.findAll('.tool__option input[type="checkbox"]')[1].setValue(true)

    expect(app.emitted('update:knowledge').at(-1)[0]).toEqual({
      enabled: true,
      selectedCollectionIds: ['collection-1', 'collection-2'],
    })
  })

  it('tells the user a live toggle still needs a selection', () => {
    const app = mountComposer({
      mode: 'chat',
      knowledge: { enabled: true, selectedCollectionIds: [] },
      collections: COLLECTIONS,
      canUseKnowledge: true,
    })
    const note = app.find('[data-test="knowledge-no-selection"]')
    expect(note.exists()).toBe(true)
    expect(note.text()).toContain('不会引用资料')
    expect(app.find('[data-test="knowledge-manage-link"]').exists()).toBe(false)
  })

  it('shows the count once something is selected', () => {
    const app = mountComposer({
      mode: 'chat',
      knowledge: { enabled: true, selectedCollectionIds: ['collection-1'] },
      collections: COLLECTIONS,
      canUseKnowledge: true,
    })
    expect(app.find('[data-test="knowledge-no-selection"]').exists()).toBe(false)
    expect(app.find('.tool__count').text()).toBe('1')
  })
})

describe('sending behaviour is unchanged by the new toolbar', () => {
  it('keeps Enter-to-send and Shift+Enter for a newline', async () => {
    const app = mountComposer({ mode: 'chat' })
    const textarea = app.find('textarea')

    await textarea.trigger('keydown', { key: 'Enter', shiftKey: true })
    expect(app.emitted('submit')).toBeUndefined()

    await textarea.trigger('keydown', { key: 'Enter' })
    expect(app.emitted('submit')).toHaveLength(1)
  })

  it('ignores the Enter that commits a Chinese IME candidate', async () => {
    const app = mountComposer({ mode: 'chat' })
    await app.find('textarea').trigger('keydown', { key: 'Enter', isComposing: true })
    expect(app.emitted('submit')).toBeUndefined()
  })
})
