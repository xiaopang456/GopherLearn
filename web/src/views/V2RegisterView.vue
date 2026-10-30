<script setup>
import { ref, onBeforeUnmount } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { NButton } from 'naive-ui'
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
  <AuthLayout eyebrow="欢迎加入" title="创建账号" description="用邮箱验证码注册，验证码两分钟内有效。">
    <form class="gl-stack" data-test="register-form" novalidate @submit.prevent="submit">
      <ServerNotice v-if="formError || page.error.value" :message="formError || page.error.value" />
      <p v-if="page.notice.value" class="register__notice" role="status">{{ page.notice.value }}</p>
      <p v-if="page.registered.value" class="register__done" data-test="registered">
        注册成功，你的用户名是 {{ page.username.value }}
      </p>

      <div class="register__email">
        <FormField
          v-model="email"
          name="email"
          label="邮箱"
          type="email"
          autocomplete="username"
          :disabled="page.busy.value || page.registered.value"
        />
        <n-button
          class="register__send"
          secondary
          :disabled="page.busy.value || page.registered.value || !email.trim()"
          data-test="send"
          @click="page.send(email)"
        >
          发送验证码
        </n-button>
      </div>

      <FormField
        v-model="captcha"
        name="captcha"
        label="六位数字验证码"
        autocomplete="one-time-code"
        :disabled="page.busy.value || page.registered.value"
      />
      <FormField
        v-model="password"
        name="password"
        label="密码"
        type="password"
        autocomplete="new-password"
        hint="8–64 个 Unicode 字符，UTF-8 不超过 72 字节。"
        :disabled="page.busy.value || page.registered.value"
      />
      <FormField
        v-model="confirm"
        name="confirm"
        label="确认密码"
        type="password"
        autocomplete="new-password"
        :disabled="page.busy.value || page.registered.value"
      />

      <n-button
        type="primary"
        attr-type="submit"
        block
        :disabled="page.busy.value || page.registered.value"
      >
        注册
      </n-button>

      <div class="register__foot">
        <n-button
          v-if="page.busy.value"
          attr-type="button"
          secondary
          data-test="cancel"
          @click="cancel"
        >
          取消本次请求
        </n-button>
        <n-button
          v-if="page.registered.value && page.error.value"
          attr-type="button"
          secondary
          data-test="confirm-retry"
          :disabled="page.busy.value"
          @click="retry"
        >
          重试登录状态确认
        </n-button>
        <RouterLink class="register__login" :to="{ name: 'login', query: route.query }">
          已有账号，登录
        </RouterLink>
      </div>
    </form>
  </AuthLayout>
</template>

<style scoped>
.register__notice {
  margin: 0;
  padding: var(--gl-space-2) var(--gl-space-3);
  border-radius: var(--gl-radius-sm);
  background: var(--gl-accent-soft);
  color: var(--gl-accent-ink);
  font-size: var(--gl-text-sm);
}

.register__done {
  margin: 0;
  padding: var(--gl-space-2) var(--gl-space-3);
  border-radius: var(--gl-radius-sm);
  background: var(--gl-accent-soft);
  color: var(--gl-accent-ink);
  font-size: var(--gl-text-sm);
  font-weight: 600;
}

/* The captcha is sent for this address, so the button belongs to the field. */
.register__email {
  display: flex;
  align-items: flex-end;
  gap: var(--gl-space-2);
}

.register__email :deep(.field) {
  flex: 1;
}

.register__send {
  flex: none;
  margin-bottom: 1px;
}

.register__foot {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--gl-space-3);
}

.register__login {
  font-size: var(--gl-text-sm);
  color: var(--gl-accent-ink);
  text-decoration: underline;
  text-underline-offset: 2px;
}
</style>
