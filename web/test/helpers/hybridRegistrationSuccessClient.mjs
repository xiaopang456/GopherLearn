import assert from 'node:assert/strict'
import process from 'node:process'
import { sendV2Captcha, registerV2User } from '../../src/api/v2Registration.js'
import { fetchMe } from '../../src/api/auth.js'
const realFetch = globalThis.fetch
const calls = []
globalThis.fetch = (url, init) => { calls.push(String(url)); return realFetch(new URL(url, process.env.HYBRID_TEST_URL), init) }
await sendV2Captcha(' FRONTEND@INVALID.EXAMPLE ')
// Test-only observation of the mail boundary, never a production captcha route.
const { captcha } = await (await realFetch(new URL('/test-mail-boundary', process.env.HYBRID_TEST_URL))).json()
assert.match(captcha, /^[0-9]{6}$/)
const result = await registerV2User({ email: 'frontend@invalid.example', captcha, password: 'password-123' })
assert.match(result.username, /^[0-9]{11}$/)
const me = await fetchMe(result.token)
assert.equal(me.username, result.username)
assert.equal(me.email, 'frontend@invalid.example')
assert.equal(typeof me.id, 'string')
assert.match(me.id, /^[1-9][0-9]*$/)
assert.deepEqual(calls, ['/api/v1/user/captcha', '/api/v1/user/register', '/api/v1/me'])
console.log('Native API -> real registration Service/DAO/JWT -> authenticated me, local socket only.')
