<script setup>
import { computed, onMounted } from 'vue'
import { useRoute } from 'vue-router'

import NavRail from '../components/NavRail.vue'
import { authStore } from '../auth/store.js'

const route = useRoute()

const HEADINGS = {
  chat: '对话',
  session: '对话',
  knowledge: '知识库',
  memory: '记忆',
  'v2-documents': '文档',
  'v2-source': '文档原文',
  'v2-memory': '记忆治理',
  learning: '练习',
  'v2-learning': '学习／测验',
}

const heading = computed(() => HEADINGS[route.name] ?? 'GopherLearn')
const identity = computed(
  () => authStore.state.user.email || authStore.state.user.displayName || '已登录',
)

// The shell is where a restored token first gets exercised. It only asks the
// question; the shared unauthorized handler in the router decides what a "no"
// means, so every other endpoint enforces the same thing for free. A network
// failure is not a "no" and leaves the session alone.
//
// The integrated backend has no Collection surface (V2 knowledge is per-user
// documents), so the shell fetches no collection list: the composer's grounding
// toggle stays hidden rather than asking for a route this entry does not serve.
// A Collection list belongs to the baseline entry and loads on its own page.
onMounted(() => {
  authStore.refreshProfile().catch(() => {})
})
</script>

<template>
  <div class="shell">
    <header class="shell__bar">
      <p class="shell__title">{{ heading }}</p>
      <p class="shell__who">{{ identity }}</p>
    </header>

    <NavRail class="shell__rail" />

    <main class="shell__main">
      <RouterView />
    </main>
  </div>
</template>

<style scoped>
.shell {
  display: grid;
  grid-template-columns: var(--gl-rail) minmax(0, 1fr);
  grid-template-rows: auto minmax(0, 1fr);
  grid-template-areas:
    'top top'
    'rail main';
  height: 100%;
  min-height: 100dvh;
}

.shell__bar {
  grid-area: top;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--gl-space-4);
  height: 44px;
  padding: 0 var(--gl-space-5);
  background: var(--gl-surface);
  border-bottom: 1px solid var(--gl-surface-line);
}

.shell__title {
  font-size: var(--gl-text-base);
  font-weight: 600;
  letter-spacing: -0.01em;
  color: var(--gl-ink);
}

.shell__who {
  font-size: var(--gl-text-sm);
  color: var(--gl-ink-faint);
  overflow-wrap: anywhere;
}

.shell__rail {
  grid-area: rail;
  min-height: 0;
}

.shell__main {
  grid-area: main;
  min-width: 0;
  min-height: 0;
  overflow: hidden;
}

@media (max-width: 700px) {
  .shell {
    grid-template-columns: minmax(0, 1fr);
    grid-template-rows: auto minmax(0, 1fr) auto;
    grid-template-areas:
      'top'
      'main'
      'rail';
  }

  .shell__bar {
    padding: 0 var(--gl-space-4);
  }
}
</style>
