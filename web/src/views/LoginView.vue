<script setup>
import { computed, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import AuthLayout from '../components/AuthLayout.vue'
import FormField from '../components/FormField.vue'
import ServerNotice from '../components/ServerNotice.vue'
import { authStore } from '../auth/store.js'
import { GENERIC_ERROR, safeErrorMessage } from '../api/http.js'
import { safeRedirectTarget } from '../lib/redirect.js'
import { validateEmail, validatePassword } from '../lib/validation.js'

const router = useRouter()
const route = useRoute()

const email = ref('')
const password = ref('')
const showPassword = ref(false)
const errors = ref({ email: '', password: '' })
const serverError = ref('')
const submitting = ref(false)

const busy = computed(() => submitting.value)

function validate() {
  errors.value = {
    email: validateEmail(email.value) ?? '',
    password: validatePassword(password.value) ?? '',
  }
  return !errors.value.email && !errors.value.password
}

async function submit() {
  if (busy.value) return
  serverError.value = ''
  if (!validate()) return

  submitting.value = true
  try {
    await authStore.login({
      email: email.value.trim(),
      password: password.value,
    })
    router.replace(safeRedirectTarget(route.query.redirect))
  } catch (error) {
    serverError.value = safeErrorMessage(error, GENERIC_ERROR)
  } finally {
    submitting.value = false
    // Never leave the password sitting in the field after a failed attempt.
    password.value = ''
  }
}
</script>

<template>
  <AuthLayout
    eyebrow="欢迎回来"
    title="登录 GopherLearn"
    description="登录后可以继续已有的会话，或开始一次新的提问。"
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

      <div class="login__password">
        <FormField
          v-model="password"
          name="password"
          label="密码"
          :type="showPassword ? 'text' : 'password'"
          autocomplete="current-password"
          :error="errors.password"
          :disabled="busy"
          @blur="validate"
        />
        <button
          type="button"
          class="gl-btn gl-btn--quiet login__toggle"
          :aria-pressed="showPassword"
          :disabled="busy"
          @click="showPassword = !showPassword"
        >
          {{ showPassword ? '隐藏' : '显示' }}
        </button>
      </div>

      <button class="gl-btn gl-btn--primary" type="submit" :disabled="busy">
        {{ busy ? '正在登录…' : '登录' }}
      </button>

      <p class="login__aside">
        还没有账号？
        <RouterLink :to="{ name: 'register', query: route.query }">
          用邮箱验证码注册
        </RouterLink>
      </p>
    </form>
  </AuthLayout>
</template>

<style scoped>
.login__password {
  position: relative;
}

.login__toggle {
  position: absolute;
  top: 1.55rem;
  right: 0.35rem;
  font-size: 0.8rem;
  padding: 0.2em 0.5em;
}

.login__aside {
  margin: 0.2rem 0 0;
  font-size: 0.9rem;
  color: var(--gl-ink-soft);
}
</style>
