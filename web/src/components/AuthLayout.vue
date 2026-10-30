<script setup>
import AppIcon from './AppIcon.vue'

/**
 * Split sign-in frame: a statement panel that explains what the product is
 * for, and the form column. Below 860px the statement collapses to a heading
 * so the form keeps the whole viewport.
 */
defineProps({
  eyebrow: { type: String, default: '' },
  title: { type: String, required: true },
  description: { type: String, default: '' },
})
</script>

<template>
  <div class="auth">
    <section class="auth__statement">
      <p class="auth__mark">
        <AppIcon name="logo" :size="26" />
        <span>GopherLearn</span>
      </p>
      <div class="auth__pitch">
        <h2>把你的笔记变成会追问的学习伙伴</h2>
        <ul>
          <li>基于你自己的资料回答，并指出依据来自哪里。</li>
          <li>会话分普通对话与学习 Agent 两种，随时可以只聊天。</li>
          <li>知识库由你自己上传和删除，带不带资料提问每一轮单独选。</li>
        </ul>
      </div>
    </section>

    <main class="auth__form">
      <div class="auth__card gl-card">
        <p v-if="eyebrow" class="gl-eyebrow">{{ eyebrow }}</p>
        <h1>{{ title }}</h1>
        <p v-if="description" class="gl-muted auth__desc">{{ description }}</p>
        <slot />
      </div>
    </main>
  </div>
</template>

<style scoped>
.auth {
  display: grid;
  grid-template-columns: minmax(0, 1fr) minmax(0, 480px);
  min-height: 100%;
  /* A viewport height of its own: this page is not inside the app shell, so
     it cannot rely on a percentage height from there. */
  min-height: 100dvh;
}

.auth__statement {
  display: flex;
  flex-direction: column;
  justify-content: center;
  gap: var(--gl-space-6);
  padding: clamp(2rem, 5vw, 4rem);
  background: linear-gradient(165deg, #1c1c20 0%, #101012 60%, #17171a 100%);
  color: #f3f3f5;
}

.auth__mark {
  display: flex;
  align-items: center;
  gap: var(--gl-space-2);
  font-size: var(--gl-text-lg);
  font-weight: 700;
  letter-spacing: 0.01em;
}

.auth__pitch h2 {
  font-size: clamp(1.4rem, 2.4vw, 1.9rem);
  line-height: var(--gl-leading-tight);
  letter-spacing: -0.02em;
  max-width: 22ch;
  margin-bottom: var(--gl-space-4);
}

.auth__pitch ul {
  margin: 0;
  padding-left: 1.1rem;
  display: flex;
  flex-direction: column;
  gap: var(--gl-space-3);
  color: #b8b8c2;
  font-size: var(--gl-text-base);
  max-width: 36ch;
}

.auth__form {
  display: flex;
  align-items: center;
  justify-content: center;
  padding: clamp(1rem, 4vw, 3rem);
}

.auth__card {
  width: 100%;
  max-width: 380px;
  padding: clamp(1.5rem, 3vw, 2.25rem);
  display: flex;
  flex-direction: column;
  gap: var(--gl-space-4);
}

.auth__desc {
  font-size: var(--gl-text-base);
}

@media (max-width: 860px) {
  .auth {
    grid-template-columns: minmax(0, 1fr);
  }

  .auth__statement {
    padding: var(--gl-space-6) var(--gl-space-5) 0;
    background: none;
    color: inherit;
    gap: var(--gl-space-3);
  }

  .auth__pitch {
    display: none;
  }

  .auth__mark {
    color: var(--gl-ink);
  }

  .auth__form {
    padding: var(--gl-space-5);
  }
}
</style>
