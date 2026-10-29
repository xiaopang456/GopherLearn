<script setup>
import { ref, onBeforeUnmount } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import AuthLayout from '../components/AuthLayout.vue'
import FormField from '../components/FormField.vue'
import ServerNotice from '../components/ServerNotice.vue'
import { useV2Registration, validateV2Password } from '../composables/useV2Registration.js'
import { safeRedirectTarget } from '../lib/redirect.js'
const router = useRouter(), route = useRoute(), page = useV2Registration()
const email = ref(''), captcha = ref(''), password = ref(''), confirm = ref(''), formError = ref('')
function clearSecrets() { captcha.value = ''; password.value = ''; confirm.value = '' }
async function submit() {
  if (page.busy.value || page.registered.value) return
  formError.value = !email.value.trim() ? '请填写邮箱。' : !/^[0-9]{6}$/.test(captcha.value) ? '验证码必须为六位数字，保留前导零。' : validateV2Password(password.value) || (password.value !== confirm.value ? '两次密码不一致。' : '')
  if (formError.value) return
  const body = { email: email.value, captcha: captcha.value, password: password.value }
  clearSecrets()
  if (await page.register(body)) router.replace(safeRedirectTarget(route.query.redirect))
}
async function retry() { if (await page.retryConfirmation()) router.replace(safeRedirectTarget(route.query.redirect)) }
function cancel() { page.abort(); clearSecrets() }
onBeforeUnmount(() => { page.dispose(); clearSecrets() })
</script>
<template>
  <AuthLayout eyebrow="原 V2 合同" title="V2 邮箱注册" description="独立入口：验证码与服务端生成用户名。旧注册页及合同保留。">
    <form class="gl-stack" data-test="register-form" novalidate @submit.prevent="submit">
      <ServerNotice v-if="formError || page.error.value" :message="formError || page.error.value" />
      <p v-if="page.notice.value" role="status">{{ page.notice.value }}</p>
      <p v-if="page.registered.value" data-test="registered">注册成功 · 原生成用户名 {{ page.username.value }}</p>
      <FormField v-model="email" name="email" label="邮箱" type="email" autocomplete="username" :disabled="page.busy.value || page.registered.value" />
      <button type="button" class="gl-btn" data-test="send" :disabled="page.busy.value || page.registered.value || !email.trim()" @click="page.send(email)">发送验证码</button>
      <FormField v-model="captcha" name="captcha" label="六位数字验证码" autocomplete="one-time-code" :disabled="page.busy.value || page.registered.value" />
      <FormField v-model="password" name="password" label="密码" type="password" autocomplete="new-password" hint="8–64 个 Unicode 字符，UTF-8 不超过 72 字节。" :disabled="page.busy.value || page.registered.value" />
      <FormField v-model="confirm" name="confirm" label="确认密码（仅前端）" type="password" autocomplete="new-password" :disabled="page.busy.value || page.registered.value" />
      <button class="gl-btn gl-btn--primary" :disabled="page.busy.value || page.registered.value">注册并确认登录状态</button>
      <button v-if="page.busy.value" type="button" class="gl-btn" data-test="cancel" @click="cancel">取消本次请求</button>
      <button v-if="page.registered.value && page.error.value" type="button" class="gl-btn" data-test="confirm-retry" :disabled="page.busy.value" @click="retry">仅重试 /me 确认</button>
      <RouterLink :to="{ name: 'register', query: route.query }">旧注册页（原合同）</RouterLink>
      <RouterLink :to="{ name: 'login', query: route.query }">已有账号，登录</RouterLink>
    </form>
  </AuthLayout>
</template>
