import assert from 'node:assert/strict'
import process from 'node:process'
import { sendV2Captcha, registerV2User } from '../../src/api/v2Registration.js'
const realFetch = globalThis.fetch
globalThis.fetch = (url, init) => realFetch(new URL(url, process.env.HYBRID_TEST_URL), init)
await assert.rejects(() => sendV2Captcha('not-an-email'), { nativeCode: 2001, status: 200 })
await assert.rejects(() => registerV2User({ email: 'new@invalid.example', captcha: 'abc123', password: 'password-123' }), { nativeCode: 2001 })
await assert.rejects(() => registerV2User({ email: ' LEARNER@INVALID.EXAMPLE ', captcha: '123456', password: 'password-123' }), { nativeCode: 2002 })
console.log('Negative native API/socket only; no successful registration, Redis or email acceptance.')
