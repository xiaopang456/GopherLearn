import { ApiError, buildUrl, isAbort } from './http.js'

// Public native endpoints. No bearer, clean profile/defaults or automatic retry.
async function registrationRequest(action, body, signal) {
  const path = `/api/v1/user/${action}`
  let response
  try {
    response = await fetch(buildUrl(path), { method: 'POST', signal,
      headers: { accept: 'application/json', 'content-type': 'application/json' },
      body: JSON.stringify(body) })
  } catch (cause) {
    if (isAbort(cause)) throw cause
    throw new ApiError('请求未确认；不自动重试注册或验证码发送。', { code: 'network_error', path })
  }
  let result
  try { result = JSON.parse(await response.text()) } catch { result = null }
  if (!response.ok || result?.status_code !== 1000) {
    const error = new ApiError(result?.status_msg || '原注册操作未完成；账号是否已提交需另行确认。', {
      status: response.status, code: `v2_${result?.status_code ?? response.status}`, path })
    error.nativeCode = result?.status_code
    throw error
  }
  return result
}

// Success means the original send function returned successfully, not receipt.
export const sendV2Captcha = (email, signal) => registrationRequest('captcha', { email }, signal)
export const registerV2User = ({ email, captcha, password }, signal) => registrationRequest('register', { email, captcha, password }, signal)
