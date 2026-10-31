import { beforeEach, afterEach, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { createMemoryHistory } from 'vue-router'
import { createAppRouter } from '../src/router/index.js'
import { createAuthStore, authStore } from '../src/auth/store.js'
import { useV2Registration, validateV2Password } from '../src/composables/useV2Registration.js'
import V2RegisterView from '../src/views/V2RegisterView.vue'
import { setUnauthorizedHandler } from '../src/api/http.js'
let calls, responder
beforeEach(() => {
  calls = []; authStore.signOut()
  responder = (url) => url.endsWith('/register') ? { status_code: 1000, token: 'synthetic-native-token', username: '00012345678' } : url.endsWith('/me') ? { data: { id: '9007199254740993123', email: 'u@example.com', username: '00012345678', display_name: '真实资料' }, error: null } : { status_code: 1000 }
  vi.stubGlobal('fetch', async (url, init) => { calls.push({ url: String(url), init }); return new Response(JSON.stringify(await responder(String(url), init))) })
})
afterEach(() => { authStore.signOut(); setUnauthorizedHandler(null); vi.unstubAllGlobals() })
function storage() { return { getItem: () => null, setItem: vi.fn(), removeItem: vi.fn() } }
async function page() {
  const router = createAppRouter({ history: createMemoryHistory(), authStore })
  await router.push('/register'); await router.isReady()
  const wrapper = mount(V2RegisterView, { global: { plugins: [router] } })
  return { wrapper, router }
}
async function fill(w, pwd = 'password-123') {
  for (const [name, value] of Object.entries({ email: 'u@example.com', captcha: '001234', password: pwd, confirm: pwd })) await w.get(`[name=${name}]`).setValue(value)
}
it('uses original password rune/byte boundaries independently of the clean validator', () => {
  for (const value of ['12345678', 'a'.repeat(64), '中'.repeat(24)]) expect(validateV2Password(value)).toBe('')
  for (const value of ['1234567', 'a'.repeat(65), '中'.repeat(25), '😀'.repeat(19)]) expect(validateV2Password(value)).not.toBe('')
})
it('the register button submits the form, and the captcha button does not', async () => {
  const { wrapper: w } = await page()
  // The control layer renders a real <button>; its HTML type decides whether a
  // click submits the form or merely fires a handler. The register action has
  // to submit, and "send the captcha" must not.
  const submit = w.get('form button[type="submit"]')
  expect(submit.exists()).toBe(true)
  expect(submit.text()).toContain('注册')
  expect(w.get('[data-test=send]').element.type).toBe('button')
})
it('binds exact native fields, preserves leading zero captcha/username and confirms real /me without login', async () => {
  const { wrapper: w } = await page(); await fill(w)
  await w.get('[data-test=send]').trigger('click'); await flushPromises()
  expect(w.text()).toContain('发送请求成功'); expect(w.text()).toContain('不保证邮件已送达')
  await w.get('[data-test=register-form]').trigger('submit'); await flushPromises()
  const registration = calls.find((c) => c.url.endsWith('/user/register'))
  expect(JSON.parse(registration.init.body)).toEqual({ email: 'u@example.com', captcha: '001234', password: 'password-123' })
  expect(calls.find((c) => c.url.endsWith('/me')).init.headers.authorization).toBe('Bearer synthetic-native-token')
  expect(calls.some((c) => c.url.endsWith('/login'))).toBe(false)
  expect(authStore.state.user).toEqual({ id: '9007199254740993123', email: 'u@example.com', displayName: '真实资料' })
  expect(authStore.state.token).toBe('synthetic-native-token')
  expect(w.get('[name=password]').element.value).toBe(''); expect(w.get('[name=captcha]').element.value).toBe('')
  expect(w.text()).toContain('00012345678'); w.unmount()
})
it('/me failure keeps registration success and retry requests only /me, no fabricated profile or re-registration', async () => {
  let fail = true
  const original = responder
  responder = (url) => url.endsWith('/me') && fail ? { data: null, error: { code: 'failed', message: 'unavailable' } } : original(url)
  const { wrapper: w } = await page(); await fill(w)
  await w.get('[data-test=register-form]').trigger('submit'); await flushPromises()
  expect(w.text()).toContain('注册成功，登录状态确认失败')
  expect(authStore.isAuthenticated()).toBe(false)
  expect(w.get('[data-test=registered]').text()).toContain('00012345678')
  fail = false
  await w.get('[data-test=confirm-retry]').trigger('click'); await flushPromises()
  expect(calls.filter((c) => c.url.endsWith('/register'))).toHaveLength(1)
  expect(calls.filter((c) => c.url.endsWith('/me'))).toHaveLength(2)
  expect(calls.some((c) => c.url.endsWith('/login'))).toBe(false)
  w.unmount()
})
it('rejects invalid captcha and native errors without retry, profile adoption or persistent form secrets', async () => {
  const { wrapper: w } = await page(); await fill(w)
  await w.get('[name=captcha]').setValue('12345'); await w.get('[data-test=register-form]').trigger('submit')
  expect(calls).toHaveLength(0); expect(w.text()).toContain('六位数字')
  await w.get('[name=captcha]').setValue('001234')
  responder = () => ({ status_code: 2014, status_msg: '原验证码错误' })
  await w.get('[data-test=register-form]').trigger('submit'); await flushPromises()
  expect(w.text()).toContain('原验证码错误'); expect(w.text()).toContain('结果未确认')
  expect(calls).toHaveLength(1); expect(authStore.isAuthenticated()).toBe(false)
  expect(w.find('[data-test=confirm-retry]').exists()).toBe(false); w.unmount()
})
it('suppresses send/register double clicks; cancellation and page disposal ignore late responses', async () => {
  const saved = storage(), store = createAuthStore({ storage: saved }), logic = useV2Registration(store)
  let finish
  responder = () => new Promise((resolve) => { finish = resolve })
  const sending = logic.send('u@example.com'); await logic.send('u@example.com'); expect(calls).toHaveLength(1)
  logic.abort(); expect(calls[0].init.signal.aborted).toBe(true)
  expect(logic.error.value).toContain('结果未确认')
  finish({ status_code: 1000 }); await sending; expect(logic.notice.value).toBe('')
  const registering = logic.register({ email: 'u@example.com', captcha: '001234', password: 'password-123' })
  await logic.register({}); expect(calls).toHaveLength(2)
  logic.dispose(); finish({ status_code: 1000, token: 'late-secret', username: '0001' }); await registering
  expect(store.isAuthenticated()).toBe(false); expect(saved.setItem).not.toHaveBeenCalled()
  expect(logic.registered.value).toBe(false)
})
it('late /me after disposal cannot adopt a token; only retry confirmation after an explicit cancellation', async () => {
  const store = createAuthStore({ storage: storage() }), logic = useV2Registration(store)
  let finish
  responder = (url) => url.endsWith('/me') ? new Promise((resolve) => { finish = resolve }) : { status_code: 1000, token: 'synthetic', username: '0001' }
  const pending = logic.register({ email: 'u@example.com', captcha: '001234', password: 'password-123' })
  await flushPromises(); expect(logic.registered.value).toBe(true)
  logic.dispose(); finish({ data: { id: '1', email: 'u@example.com', display_name: 'late' }, error: null }); await pending
  expect(store.isAuthenticated()).toBe(false)
  expect(logic.error.value).toContain('注册成功，登录状态确认失败')
})
it('send network failure and HTTP-200 business error never become sent or registration success', async () => {
  const logic = useV2Registration(createAuthStore({ storage: storage() }))
  responder = () => { throw new TypeError('offline') }
  await logic.send('u@example.com'); expect(logic.error.value).toContain('未确认'); expect(logic.notice.value).toBe('')
  responder = () => ({ status_code: 2015, status_msg: '原发送限流' })
  await logic.send('u@example.com'); expect(logic.error.value).toContain('原发送限流')
  expect(calls).toHaveLength(2); expect(logic.registered.value).toBe(false); logic.dispose()
})

it('cancelled /me can explicitly retry only confirmation; malformed profile is never adopted', async () => {
  const saved = storage(), store = createAuthStore({ storage: saved }), logic = useV2Registration(store)
  let finish
  responder = (url) => url.endsWith('/me') ? new Promise((resolve) => { finish = resolve }) : { status_code: 1000, token: 'synthetic', username: '0001' }
  const sending = logic.register({ email: 'u@example.com', captcha: '001234', password: 'password-123' })
  await flushPromises(); logic.abort()
  finish({ data: { id: '1', email: 'u@example.com' }, error: null }); await sending
  expect(store.isAuthenticated()).toBe(false)
  responder = () => ({ data: { username: '0001' }, error: null })
  await logic.retryConfirmation(); expect(store.isAuthenticated()).toBe(false)
  expect(saved.setItem).not.toHaveBeenCalled()
  responder = () => ({ data: { id: '9007199254740993123', email: 'u@example.com', display_name: 'server' }, error: null })
  await logic.retryConfirmation(); expect(store.state.user.id).toBe('9007199254740993123')
  expect(calls.filter((c) => c.url.endsWith('/register'))).toHaveLength(1)
  expect(calls.filter((c) => c.url.endsWith('/me'))).toHaveLength(3)
  expect(calls.some((c) => c.url.endsWith('/login'))).toBe(false)
  logic.dispose()
})

it('a stale candidate-token rejection cannot sign out a newer authenticated session', async () => {
  const store = createAuthStore({ storage: storage() }), logic = useV2Registration(store)
  let finish
  const globalReject = vi.fn(() => store.signOut())
  setUnauthorizedHandler(globalReject)
  responder = (url) => url.endsWith('/me') ? new Promise((resolve) => { finish = resolve }) : { status_code: 1000, token: 'candidate-token', username: '0001' }
  const sending = logic.register({ email: 'u@example.com', captcha: '001234', password: 'password-123' })
  await flushPromises(); logic.dispose()
  store.adopt('newer-session-token', { id: '2', email: 'other@example.com', displayName: 'newer' })
  finish({ data: null, error: { code: 'unauthorized', message: 'invalid candidate' } }); await sending
  expect(globalReject).not.toHaveBeenCalled()
  expect(store.getToken()).toBe('newer-session-token')
})
