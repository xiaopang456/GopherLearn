<script setup>
import { computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import AppIcon from './AppIcon.vue'
import { authStore } from '../auth/store.js'

/**
 * Primary navigation.
 *
 * A labelled rail, not a column of anonymous glyphs: the four destinations
 * are the whole map of the product, and an icon alone does not say which is
 * which. Each entry therefore carries its name under the glyph, and the
 * label is the accessible name too.
 */
const sections = [
  { name: 'chat', label: '对话', full: '对话', icon: 'chat', to: { name: 'chat' } },
  { name: 'v2-documents', label: '文档', full: '文档', icon: 'knowledge', to: { name: 'v2-documents' } },
  { name: 'v2-memory', label: '记忆', full: '记忆治理', icon: 'memory', to: { name: 'v2-memory' } },
  { name: 'v2-learning', label: '学习', full: '学习／测验', icon: 'learning', to: { name: 'v2-learning' } },
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
      <AppIcon name="logo" :size="22" label="GopherLearn" />
    </p>

    <ul class="rail__list">
      <li v-for="section in sections" :key="section.name">
        <RouterLink
          class="rail__link"
          :class="{ 'rail__link--active': active === section.name }"
          :to="section.to"
          :title="section.full"
          :aria-current="active === section.name ? 'page' : undefined"
        >
          <span class="rail__glyph">
            <AppIcon :name="section.icon" :size="20" />
          </span>
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
        <span class="rail__glyph">
          <AppIcon name="signout" :size="20" />
        </span>
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
  gap: var(--gl-space-4);
  padding: var(--gl-space-4) var(--gl-space-2);
  background: var(--gl-surface);
  border-right: 1px solid var(--gl-surface-line);
  min-height: 0;
}

.rail__brand {
  display: flex;
  align-items: center;
  justify-content: center;
  width: 40px;
  height: 40px;
  border-radius: var(--gl-radius-sm);
  background: var(--gl-accent-soft);
  color: var(--gl-accent-ink);
}

.rail__list {
  list-style: none;
  display: flex;
  flex: 1;
  flex-direction: column;
  gap: var(--gl-space-1);
  padding: 0;
  margin: 0;
}

.rail__link {
  display: flex;
  width: 60px;
  flex-direction: column;
  align-items: center;
  gap: 3px;
  padding: var(--gl-space-2) 0;
  border: 0;
  border-radius: var(--gl-radius-sm);
  background: none;
  color: var(--gl-ink-soft);
  text-decoration: none;
  transition: background-color 120ms ease, color 120ms ease;
}

.rail__link:hover {
  background: var(--gl-surface-soft);
  color: var(--gl-ink);
}

.rail__link--active {
  background: var(--gl-accent-soft);
  color: var(--gl-accent-ink);
}

.rail__glyph {
  display: flex;
  align-items: center;
  justify-content: center;
  height: 24px;
}

.rail__text {
  font-size: var(--gl-text-xs);
  font-weight: 500;
  line-height: 1.2;
  letter-spacing: 0.02em;
}

.rail__foot {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: var(--gl-space-2);
}

.rail__avatar {
  display: flex;
  width: 32px;
  height: 32px;
  align-items: center;
  justify-content: center;
  border-radius: 50%;
  background: var(--gl-ink);
  color: #fff;
  font-size: var(--gl-text-xs);
  font-weight: 600;
  letter-spacing: 0.02em;
}

.rail__signout:hover {
  background: var(--gl-danger-soft);
  color: var(--gl-danger);
}

@media (max-width: 700px) {
  .rail {
    flex-direction: row;
    justify-content: space-around;
    border-right: 0;
    border-top: 1px solid var(--gl-surface-line);
    padding: var(--gl-space-1) var(--gl-space-2);
    gap: var(--gl-space-1);
  }

  .rail__brand {
    display: none;
  }

  .rail__list {
    flex-direction: row;
    flex: none;
    gap: var(--gl-space-1);
  }

  .rail__link {
    width: 58px;
    padding: 5px 0;
  }

  .rail__foot {
    flex-direction: row;
  }

  .rail__avatar {
    display: none;
  }
}
</style>
