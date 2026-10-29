<script setup>
import { computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import AppIcon from './AppIcon.vue'
import { authStore } from '../auth/store.js'

/**
 * Primary navigation. Four destinations, one of which (chat) owns a second
 * contextual column; the rail therefore stays icon-led so the column next to it
 * has room for the session list.
 *
 * There is no processing-monitor entry: this build processes documents in-process and has no
 * operations surface to monitor.
 */
const sections = [
  { name: 'chat', label: '对话', icon: 'chat', to: { name: 'chat' } },
  { name: 'v2-documents', label: '文档', icon: 'knowledge', to: { name: 'v2-documents' } },
  { name: 'v2-memory', label: '记忆治理', icon: 'memory', to: { name: 'v2-memory' } },
  { name: 'v2-learning', label: '学习／测验', icon: 'learning', to: { name: 'v2-learning' } },
]

const route = useRoute()
const router = useRouter()

const initials = computed(() => {
  const name = authStore.state.user.displayName || authStore.state.user.email
  return name ? String(name).trim().slice(0, 2).toUpperCase() : 'GL'
})

const active = computed(() => {
  const matched = route.name
  if (matched === 'session') return 'chat'
  return sections.some((item) => item.name === matched) ? matched : ''
})

function signOut() {
  authStore.signOut()
  router.replace({ name: 'login' })
}
</script>

<template>
  <nav class="rail" aria-label="主导航">
    <p class="rail__brand">
      <AppIcon name="logo" :size="24" label="GopherLearn" />
    </p>

    <ul class="rail__list">
      <li v-for="section in sections" :key="section.name">
        <RouterLink
          class="rail__link"
          :class="{ 'rail__link--active': active === section.name }"
          :to="section.to"
          :aria-current="active === section.name ? 'page' : undefined"
        >
          <AppIcon :name="section.icon" :size="21" />
          <span class="rail__text">{{ section.label }}</span>
        </RouterLink>
      </li>
    </ul>

    <div class="rail__foot">
      <p class="rail__avatar" :title="authStore.state.user.email">
        {{ initials }}
      </p>
      <button
        type="button"
        class="rail__link rail__signout"
        title="退出登录"
        @click="signOut"
      >
        <AppIcon name="signout" :size="21" />
        <span class="rail__text">退出</span>
      </button>
    </div>
  </nav>
</template>

<style scoped>
.rail {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 0.9rem;
  padding: 0.9rem 0;
  background: var(--gl-surface);
  border-right: 1px solid var(--gl-surface-line);
  min-height: 0;
}

.rail__brand {
  color: var(--gl-accent);
  padding-bottom: 0.5rem;
  border-bottom: 1px solid var(--gl-surface-line);
  width: 100%;
  display: flex;
  justify-content: center;
}

.rail__list {
  list-style: none;
  display: flex;
  flex: 1;
  flex-direction: column;
  gap: 0.35rem;
  padding: 0;
  margin: 0;
}

.rail__link {
  display: flex;
  width: 44px;
  height: 44px;
  align-items: center;
  justify-content: center;
  border-radius: var(--gl-radius-sm);
  color: var(--gl-ink-soft);
  text-decoration: none;
  background: none;
  border: 0;
  padding: 0;
}

.rail__link:hover {
  background: var(--gl-surface-soft);
  color: var(--gl-accent-ink);
}

.rail__link--active {
  background: var(--gl-accent-soft);
  color: var(--gl-accent-ink);
  box-shadow: inset 3px 0 0 var(--gl-accent);
}

.rail__foot {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 0.4rem;
}

.rail__avatar {
  display: flex;
  width: 34px;
  height: 34px;
  align-items: center;
  justify-content: center;
  border-radius: 50%;
  background: var(--gl-ink);
  color: #f4f2ee;
  font-size: 0.72rem;
  font-weight: 700;
  letter-spacing: 0.02em;
}

.rail__signout:hover {
  color: var(--gl-danger);
  background: var(--gl-danger-soft);
}

/* Icon-only: reveal the label on wide screens where the rail has slack. */
.rail__text {
  position: absolute;
  width: 1px;
  height: 1px;
  overflow: hidden;
  clip-path: inset(50%);
  white-space: nowrap;
}

@media (max-width: 700px) {
  .rail {
    flex-direction: row;
    justify-content: space-around;
    border-right: 0;
    border-top: 1px solid var(--gl-surface-line);
    padding: 0.3rem 0.4rem;
  }

  .rail__brand {
    display: none;
  }

  .rail__foot {
    flex-direction: row;
  }

  .rail__avatar {
    display: none;
  }

  .rail__list {
    flex-direction: row;
    flex: none;
  }
}
</style>
