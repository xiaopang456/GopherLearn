import { ref } from 'vue'
import { sendV2Captcha, registerV2User } from '../api/v2Registration.js'
import { authStore } from '../auth/store.js'

export function validateV2Password(value) {
  const runes = Array.from(value).length
  const bytes = new TextEncoder().encode(value).length
  return runes < 8 || runes > 64 || bytes > 72 ? '原 V2 密码要求 8–64 个 Unicode 字符，且 UTF-8 不超过 72 字节。' : ''
}

// Secrets live only in request/local memory, never in saved form or logs.
export function useV2Registration(store = authStore) {
  const busy = ref(false), error = ref(''), notice = ref(''), registered = ref(false), username = ref('')
  let generation = 0, controller, pendingToken = ''
  function abort() {
    ++generation; controller?.abort()
    if (busy.value) error.value = registered.value ? '注册成功，登录状态确认失败；请求已取消，可仅重试 /me。' : '请求已取消，结果未确认；不能据此判断账号是否已创建。'
    busy.value = false
  }
  function dispose() { abort(); pendingToken = '' }
  async function confirm(current) {
    try {
      const user = await store.confirmNativeToken(pendingToken, { signal: controller.signal, isCurrent: () => current === generation })
      if (current !== generation || !user) return null
      pendingToken = ''; notice.value = '注册成功，登录状态已确认。'
      return user
    } catch {
      if (current === generation) error.value = '注册成功，登录状态确认失败；可仅重试 /me，不重新注册。'
      return null
    }
  }
  async function send(email) {
    if (busy.value || registered.value) return null
    const current = ++generation; controller = new AbortController(); busy.value = true; error.value = ''; notice.value = ''
    try {
      const result = await sendV2Captcha(email, controller.signal)
      if (current !== generation) return null
      notice.value = '发送请求成功，不保证邮件已送达。'
      return result
    } catch (cause) {
      if (current === generation) error.value = `${cause.name === 'AbortError' ? '请求已取消' : cause.message}；发送结果未确认，不自动重试。`
      return null
    } finally { if (current === generation) busy.value = false }
  }
  async function register(body) {
    if (busy.value || registered.value) return null
    const current = ++generation; controller = new AbortController(); busy.value = true; error.value = ''; notice.value = ''
    try {
      const result = await registerV2User(body, controller.signal)
      if (current !== generation) return null
      registered.value = true; username.value = result.username
      pendingToken = typeof result.token === 'string' ? result.token : ''
      return await confirm(current)
    } catch (cause) {
      if (current === generation) error.value = `${cause.name === 'AbortError' ? '请求已取消' : cause.message}；结果未确认，不能据此判断账号是否已创建。不自动重试。`
      return null
    } finally { if (current === generation) busy.value = false }
  }
  async function retryConfirmation() {
    if (busy.value || !registered.value) return null
    const current = ++generation; controller = new AbortController(); busy.value = true; error.value = ''
    try { return await confirm(current) } finally { if (current === generation) busy.value = false }
  }
  return { busy, error, notice, registered, username, send, register, retryConfirmation, abort, dispose }
}
