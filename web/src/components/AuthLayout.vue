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
      <p class="auth__phase">Phase 1：认证、会话与流式对话。</p>
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
}

.auth__statement {
  display: flex;
  flex-direction: column;
  justify-content: space-between;
  gap: 2rem;
  padding: clamp(1.5rem, 5vw, 4rem);
  background: linear-gradient(160deg, #12403b 0%, #1d2b2a 62%, #24332f 100%);
  color: #eef3f1;
}

.auth__mark {
  display: flex;
  align-items: center;
  gap: 0.6rem;
  font-size: 1.15rem;
  font-weight: 700;
  letter-spacing: 0.02em;
}

.auth__pitch h2 {
  font-size: clamp(1.4rem, 2.4vw, 2rem);
  line-height: 1.35;
  max-width: 22ch;
  margin-bottom: 1rem;
}

.auth__pitch ul {
  margin: 0;
  padding-left: 1.1rem;
  display: flex;
  flex-direction: column;
  gap: 0.55rem;
  color: #c8d8d4;
  font-size: 0.98rem;
  max-width: 34ch;
}

.auth__phase {
  font-size: 0.82rem;
  letter-spacing: 0.06em;
  text-transform: uppercase;
  color: #8fb0aa;
}

.auth__form {
  display: flex;
  align-items: center;
  justify-content: center;
  padding: clamp(1rem, 4vw, 3rem);
}

.auth__card {
  width: 100%;
  max-width: 400px;
  padding: clamp(1.4rem, 3vw, 2.2rem);
  display: flex;
  flex-direction: column;
  gap: 0.9rem;
}

.auth__desc {
  font-size: 0.95rem;
}

@media (max-width: 860px) {
  .auth {
    grid-template-columns: minmax(0, 1fr);
  }

  .auth__statement {
    padding: 1.25rem 1.25rem 0;
    background: none;
    color: inherit;
    gap: 0.5rem;
  }

  .auth__pitch {
    display: none;
  }

  .auth__mark {
    color: var(--gl-accent-ink);
  }

  .auth__phase {
    color: var(--gl-ink-faint);
  }

  .auth__form {
    padding: 1.25rem;
  }
}
</style>
