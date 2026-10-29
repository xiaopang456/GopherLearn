import { afterEach, expect, it, vi } from 'vitest'
import { sendV2Captcha, registerV2User } from '../src/api/v2Registration.js'
import { setTokenProvider } from '../src/api/http.js'
afterEach(() => vi.unstubAllGlobals())
it('sends only original explicit fields and returns native token/username without login or profile fabrication', async () => {
  setTokenProvider(() => 'existing-token-must-not-travel')
  const calls = []
  vi.stubGlobal('fetch', async (url, init) => { calls.push({ url, init }); return new Response('{"status_code":1000,"token":"synthetic-token","username":"00012345678"}') })
  await sendV2Captcha(' User@Example.COM ')
  const response = await registerV2User({ email: ' User@Example.COM ', captcha: '001234', password: 'password-123', displayName: 'not-native' })
  expect(JSON.parse(calls[0].init.body)).toEqual({ email: ' User@Example.COM ' })
  expect(JSON.parse(calls[1].init.body)).toEqual({ email: ' User@Example.COM ', captcha: '001234', password: 'password-123' })
  expect(String(calls[0].url)).toBe('/api/v1/user/captcha')
  expect(String(calls[1].url)).toBe('/api/v1/user/register')
  expect(calls[1].init.headers).not.toHaveProperty('authorization')
  expect(response.username).toBe('00012345678'); expect(response.token).toBe('synthetic-token')
  expect(response).not.toHaveProperty('user'); expect(calls).toHaveLength(2)
})
it('rejects HTTP-200 original errors and never retries or treats send success as email receipt', async () => {
  const transport = vi.fn(async () => new Response('{"status_code":2001,"status_msg":"原公开错误"}'))
  vi.stubGlobal('fetch', transport)
  await expect(registerV2User({ email: 'u@example.com', captcha: '123456', password: 'password-123' })).rejects.toMatchObject({ nativeCode: 2001, message: '原公开错误', status: 200 })
  expect(transport).toHaveBeenCalledTimes(1)
  transport.mockResolvedValue(new Response('{"status_code":1000}'))
  expect(await sendV2Captcha('u@example.com')).toEqual({ status_code: 1000 })
})
it('preserves cancellation and missing required fields without manufacturing defaults', async () => {
  const aborted = new DOMException('cancelled', 'AbortError')
  vi.stubGlobal('fetch', async () => { throw aborted })
  await expect(sendV2Captcha('u@example.com')).rejects.toBe(aborted)
  vi.stubGlobal('fetch', async (_url, init) => {
    expect(JSON.parse(init.body)).toEqual({ email: 'u@example.com' })
    return new Response('{"status_code":2000,"status_msg":"缺少验证码"}')
  })
  await expect(registerV2User({ email: 'u@example.com' })).rejects.toMatchObject({ nativeCode: 2000 })
})
