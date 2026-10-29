import { utf8ByteLength } from './text.js'

export const PASSWORD_MIN_BYTES = 8
export const PASSWORD_MAX_BYTES = 128
export const DISPLAY_NAME_MAX_CHARS = 64

// Deliberately simple: reject obvious shape problems client-side, and let the
// server remain the authority. Overly strict regexes lock real users out.
const EMAIL_SHAPE = /^[^\s@]+@[^\s@]+\.[^\s@]+$/

export function validateEmail(value) {
  const email = String(value ?? '').trim()
  if (email === '') return '请输入邮箱地址。'
  if (email.length > 254) return '邮箱地址过长。'
  if (!EMAIL_SHAPE.test(email)) return '邮箱格式不正确，应形如 name@example.com。'
  return null
}

export function validatePassword(value) {
  const password = String(value ?? '')
  if (password === '') return '请输入密码。'
  const bytes = utf8ByteLength(password)
  if (bytes < PASSWORD_MIN_BYTES) {
    return `密码至少需要 ${PASSWORD_MIN_BYTES} 字节，当前 ${bytes} 字节。`
  }
  if (bytes > PASSWORD_MAX_BYTES) {
    return `密码最多 ${PASSWORD_MAX_BYTES} 字节，当前 ${bytes} 字节。`
  }
  return null
}

export function validateDisplayName(value) {
  const name = String(value ?? '').trim()
  if (name === '') return '请填写昵称。'
  if (name.length > DISPLAY_NAME_MAX_CHARS) {
    return `昵称最多 ${DISPLAY_NAME_MAX_CHARS} 个字符。`
  }
  return null
}

export function validateMessageContent(value) {
  const content = String(value ?? '').trim()
  if (content === '') return null // empty simply means "nothing to send"
  if (content.length > 8000) return '单条消息请控制在 8000 字以内。'
  return null
}

export function canSend(content) {
  const trimmed = String(content ?? '').trim()
  return trimmed !== '' && !validateMessageContent(content)
}
