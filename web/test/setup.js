import { beforeEach, vi } from 'vitest'

// jsdom gives us localStorage, but tests must not bleed state into each other.
beforeEach(() => {
  localStorage.clear()
  vi.unstubAllGlobals()
})

// `SessionPanel` confirms destructive actions; keep the default answer so
// component tests do not have to know about it.
window.confirm = () => true

if (!window.matchMedia) {
  window.matchMedia = () => ({
    matches: false,
    addEventListener() {},
    removeEventListener() {},
  })
}
