import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'

import ComposerBox from '../src/components/ComposerBox.vue'

function mountComposer(overrides = {}) {
  return mount(ComposerBox, {
    props: {
      modelValue: '一个问题',
      mode: 'learning_agent',
      knowledgeEnabled: false,
      canUseKnowledge: false,
      ...overrides,
    },
  })
}

function tools(app) {
  return app.find('[data-test="knowledge-tools"]')
}

describe('which composer gets the knowledge switch', () => {
  it('shows it for a session the server says may use the library', () => {
    const app = mountComposer({ mode: 'learning_agent', canUseKnowledge: true })
    expect(tools(app).exists()).toBe(true)
    expect(app.find('[data-test="knowledge-toggle"]').text()).toContain('使用知识库')
  })

  it('never shows it for a chat session', () => {
    // The server states this session's ceiling as false; the composer must not
    // offer a switch that no turn from this session could honour.
    const app = mountComposer({ mode: 'chat', canUseKnowledge: false })
    expect(tools(app).exists()).toBe(false)
    expect(app.text()).not.toContain('使用知识库')
  })

  it('withholds it while the mode is unrecognised', () => {
    // Guessing "agent" or guessing "chat with knowledge" would both be a claim
    // the client has no authority to make.
    expect(tools(mountComposer({ mode: '', canUseKnowledge: true })).exists()).toBe(false)
    expect(tools(mountComposer({ mode: 'voice_beta', canUseKnowledge: true })).exists()).toBe(false)
  })

  it('has no collection picker left to show', () => {
    const app = mountComposer({ mode: 'learning_agent', canUseKnowledge: true })
    expect(app.find('[data-test="knowledge-pick"]').exists()).toBe(false)
    expect(app.find('.tool__option').exists()).toBe(false)
  })
})

describe('the switch reports only the on/off choice', () => {
  it('is off unless the caller says it is on', () => {
    const app = mountComposer({ canUseKnowledge: true })
    expect(app.find('[data-test="knowledge-toggle"]').attributes('aria-pressed')).toBe('false')
  })

  it('turns on from off', async () => {
    const app = mountComposer({ canUseKnowledge: true, knowledgeEnabled: false })
    await app.find('[data-test="knowledge-toggle"]').trigger('click')
    expect(app.emitted('update:knowledgeEnabled').at(-1)).toEqual([true])
  })

  it('turns off from on', async () => {
    const app = mountComposer({ canUseKnowledge: true, knowledgeEnabled: true })
    await app.find('[data-test="knowledge-toggle"]').trigger('click')
    expect(app.emitted('update:knowledgeEnabled').at(-1)).toEqual([false])
  })

  it('does nothing where the ceiling forbids it', async () => {
    const app = mountComposer({ mode: 'chat', canUseKnowledge: false })
    expect(app.find('[data-test="knowledge-toggle"]').exists()).toBe(false)
    expect(app.emitted('update:knowledgeEnabled')).toBeUndefined()
  })
})

describe('sending behaviour is unchanged by the switch', () => {
  it('keeps Enter-to-send and Shift+Enter for a newline', async () => {
    const app = mountComposer()
    const textarea = app.find('textarea')

    await textarea.trigger('keydown', { key: 'Enter', shiftKey: true })
    expect(app.emitted('submit')).toBeUndefined()

    await textarea.trigger('keydown', { key: 'Enter' })
    expect(app.emitted('submit')).toHaveLength(1)
  })

  it('ignores the Enter that commits a Chinese IME candidate', async () => {
    const app = mountComposer()
    await app.find('textarea').trigger('keydown', { key: 'Enter', isComposing: true })
    expect(app.emitted('submit')).toBeUndefined()
  })
})

it('shows the thinking control only in learning agent sessions', async () => {
  const app = mountComposer({ mode: 'chat', thinking: true })
  expect(app.text()).not.toContain('深度思考')
  await app.setProps({ mode: 'learning_agent' })
  expect(app.text()).toContain('深度思考')
  expect(app.text()).toContain('回答更仔细，但会慢一些')
  expect(app.text()).not.toMatch(/enableThinking|true|false/)
  await app.setProps({ mode: 'chat' })
  expect(app.find('[data-test="thinking-toggle"]').exists()).toBe(false)
})
