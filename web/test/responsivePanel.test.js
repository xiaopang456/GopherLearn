import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import ChatView from '../src/views/ChatView.vue'
import { setTokenProvider } from '../src/api/http.js'
import { DESKTOP_QUERY, installMedia } from './helpers/media.js'
import { jsonResponse } from './helpers/transport.js'

/**
 * `window.innerWidth` is a plain number, so a computed that reads it evaluates
 * once and never re-runs: crossing a breakpoint used to leave the session column
 * in the wrong state until a reload. These cases drive `matchMedia` change
 * events the way a real resize does.
 */

const mocks = vi.hoisted(() => ({
  route: { params: {}, query: {}, path: '/chat', name: 'chat' },
  push: vi.fn(),
  replace: vi.fn(),
}))

vi.mock('vue-router', () => ({
  useRoute: () => mocks.route,
  useRouter: () => ({ push: mocks.push, replace: mocks.replace }),
}))

let media

async function toggleDrawer(app) {
  await app.find('[data-test="drawer"]').trigger('click')
  await flushPromises()
}

function expanded(app) {
  return app.find('[data-test="drawer"]').attributes('aria-expanded')
}

function panelShown(app) {
  return app.find('.panel').exists()
}

async function mountChat() {
  const app = mount(ChatView, { attachTo: document.body })
  await flushPromises()
  return app
}

beforeEach(() => {
  vi.stubEnv('VITE_API_BASE_URL', 'https://api.test')
  setTokenProvider(() => 'tok.responsive')
  media = installMedia({ [DESKTOP_QUERY]: false })
  globalThis.fetch = async () => jsonResponse({ data: { items: [] }, error: null })
})

afterEach(() => {
  media.restore()
})

describe('the session column across breakpoints', () => {
  it('is resident on a desktop viewport without any interaction', async () => {
    media.set(DESKTOP_QUERY, true)
    const app = await mountChat()

    expect(panelShown(app)).toBe(true)
    expect(expanded(app)).toBe('true')
    app.unmount()
  })

  it('starts closed on a small viewport', async () => {
    const app = await mountChat()

    expect(panelShown(app)).toBe(false)
    expect(expanded(app)).toBe('false')
    app.unmount()
  })

  it('opens with a single click and closes with a single click', async () => {
    const app = await mountChat()

    await toggleDrawer(app)
    expect(panelShown(app)).toBe(true)
    expect(expanded(app)).toBe('true')

    await toggleDrawer(app)
    expect(panelShown(app)).toBe(false)
    expect(expanded(app)).toBe('false')
    app.unmount()
  })

  it('appears automatically when the window widens', async () => {
    const app = await mountChat()
    expect(panelShown(app)).toBe(false)

    media.set(DESKTOP_QUERY, true)
    await flushPromises()

    expect(panelShown(app)).toBe(true)
    app.unmount()
  })

  it('returns to the closed default when the window narrows again', async () => {
    media.set(DESKTOP_QUERY, true)
    const app = await mountChat()
    expect(panelShown(app)).toBe(true)

    media.set(DESKTOP_QUERY, false)
    await flushPromises()
    expect(panelShown(app)).toBe(false)

    // Still exactly one click to open after the round trip through desktop.
    await toggleDrawer(app)
    expect(panelShown(app)).toBe(true)
    app.unmount()
  })

  it('forgets a drawer left open once the layout grew to desktop', async () => {
    const app = await mountChat()
    await toggleDrawer(app)
    expect(panelShown(app)).toBe(true)

    media.set(DESKTOP_QUERY, true)
    await flushPromises()
    expect(panelShown(app)).toBe(true)

    media.set(DESKTOP_QUERY, false)
    await flushPromises()
    expect(panelShown(app)).toBe(false)
    app.unmount()
  })
})

describe('subscription hygiene', () => {
  it('subscribes once per mounted view and unsubscribes on unmount', async () => {
    expect(media.count(DESKTOP_QUERY)).toBe(0)

    const app = await mountChat()
    expect(media.count(DESKTOP_QUERY)).toBe(1)

    app.unmount()
    expect(media.count(DESKTOP_QUERY)).toBe(0)
  })

  it('leaks nothing across repeated mounts', async () => {
    for (let round = 0; round < 3; round += 1) {
      const app = await mountChat()
      expect(media.count(DESKTOP_QUERY)).toBe(1)
      app.unmount()
    }
    expect(media.count(DESKTOP_QUERY)).toBe(0)
  })

  it('stops reacting once the view is gone', async () => {
    const app = await mountChat()
    app.unmount()

    expect(() => media.set(DESKTOP_QUERY, true)).not.toThrow()
    expect(media.count(DESKTOP_QUERY)).toBe(0)
  })

  it('survives a host with no matchMedia at all', async () => {
    media.restore()
    window.matchMedia = undefined

    const app = mount(ChatView, { attachTo: document.body })
    await flushPromises()

    expect(panelShown(app)).toBe(false)
    await toggleDrawer(app)
    expect(panelShown(app)).toBe(true)
    app.unmount()
  })
})
