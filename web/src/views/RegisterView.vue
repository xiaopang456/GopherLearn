<script setup>
import { computed, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import AuthLayout from '../components/AuthLayout.vue'
import FormField from '../components/FormField.vue'
import ServerNotice from '../components/ServerNotice.vue'
import { authStore } from '../auth/store.js'
import { GENERIC_ERROR, safeErrorMessage } from '../api/http.js'
import { safeRedirectTarget } from '../lib/redirect.js'
import { utf8ByteLength } from '../lib/text.js'
import {
  validateDisplayName,
  validateEmail,
  validatePassword,
} from '../lib/validation.js'

const router = useRouter()
const route = useRoute()

const email = ref('')
const displayName = ref('')
const password = ref('')
const confirm = ref('')
const errors = ref({ email: '', displayName: '', password: '', confirm: '' })
const serverError = ref('')
const submitting = ref(false)

const busy = computed(() => submitting.value)
const passwordStrength = computed(() => describePassword(password.value))

function describePassword(value) {
  if (value === '') return '至少 8 字节；中英文都按 UTF-8 字节数计算。'
  const bytes = utf8ByteLength(value)
  if (bytes < 8) return `当前 ${bytes} 字节，还差 ${8 - bytes} 字节。`
  if (bytes > 128) return `当前 ${bytes} 字节，超出 128 字节上限。`
  return `当前 ${bytes} 字节，符合要求。`
}

function validate() {
  const next = {
    email: validateEmail(email.value) ?? '',
    displayName: validateDisplayName(displayName.value) ?? '',
    password: validatePassword(password.value) ?? '',
    confirm: '',
  }
  if (!next.password && confirm.value !== password.value) {
    next.confirm = '两次输入的密码不一致。'
  }
  errors.value = next
  return !Object.values(next).some(Boolean)
}

async function submit() {
  if (busy.value) return
  serverError.value = ''
  if (!validate()) return

  submitting.value = true
  try {
    await authStore.register({
      email: email.value.trim(),
      password: password.value,
      displayName: displayName.value.trim(),
    })
    router.replace(safeRedirectTarget(route.query.redirect))
  } catch (error) {
    // A failed follow-up sign-in means the account *was* created, so say that
    // and leave the email in place instead of implying a rejected form.
    serverError.value =
      error?.code === 'login_after_register'
        ? error.message
        : safeErrorMessage(error, GENERIC_ERROR)
  } finally {
    submitting.value = false
    password.value = ''
    confirm.value = ''
  }
}
</script>

<template>
  <AuthLayout
    eyebrow="开始使用"
    title="创建账号"
    description="注册即登录，会话与后续的学习记录都挂在这个账号下。"
  >
    <form class="gl-stack" novalidate @submit.prevent="submit">
      <ServerNotice v-if="serverError" :message="serverError" />

      <FormField
        v-model="email"
        name="email"
        label="邮箱"
        type="email"
        autocomplete="username"
        placeholder="you@example.com"
        :error="errors.email"
        :disabled="busy"
        @blur="validate"
      />

      <FormField
        v-model="displayName"
        name="displayName"
        label="昵称"
        autocomplete="nickname"
        placeholder="如何称呼你"
        :error="errors.displayName"
        :disabled="busy"
        @blur="validate"
      />

      <FormField
        v-model="password"
        name="password"
        label="密码"
        type="password"
        autocomplete="new-password"
        :error="errors.password"
        :hint="passwordStrength"
        :disabled="busy"
        @blur="validate"
        @update:model-value="validate"
      />

      <FormField
        v-model="confirm"
        name="confirm"
        label="确认密码"
        type="password"
        autocomplete="new-password"
        :error="errors.confirm"
        :disabled="busy"
        @blur="validate"
      />

      <button class="gl-btn gl-btn--primary" type="submit" :disabled="busy">
        {{ busy ? '正在创建…' : '注册并登录' }}
      </button>

      <p class="register__aside">
        已经有账号了？
        <RouterLink :to="{ name: 'login', query: route.query }">直接登录</RouterLink>
      </p>
    </form>
  </AuthLayout>
</template>

<style scoped>
.register__aside {
  margin: 0.2rem 0 0;
  font-size: 0.9rem;
  color: var(--gl-ink-soft);
}
</style>
