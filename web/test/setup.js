import { beforeEach, vi } from 'vitest'

// jsdom gives us localStorage, but tests must not bleed state into each other.
beforeEach(() => {
  localStorage.clear()
  vi.unstubAllGlobals()
  // A test run stands for the shipped build, and the shipped build is not a
  // development build: the debug switch is off. A test that wants the
  // developer view says so with `vi.stubEnv('DEV', true)`.
  vi.stubEnv('DEV', false)
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
