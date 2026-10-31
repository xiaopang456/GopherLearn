<script setup>
// Unknown paths stay behind whatever auth state the visitor has; nothing here
// reveals whether a route exists for other users.
//
// This screen lives inside the application shell, so it carries the same
// navigation as every other page — an unknown address should feel like a
// wrong turn inside the product, not a separate website.
import { useRouter } from 'vue-router'
import { NButton } from 'naive-ui'

const router = useRouter()
</script>

<template>
  <div class="gl-page missing">
    <div class="missing__card">
      <p class="gl-eyebrow">404</p>
      <h1 class="missing__title">这个地址没有对应的页面</h1>
      <p class="missing__text">
        链接可能已经失效，或者地址拼错了。下面两个入口可能正是你要找的。
      </p>
      <div class="missing__actions">
        <n-button type="primary" @click="router.push({ name: 'chat' })">回到对话</n-button>
        <n-button @click="router.push({ name: 'documents' })">看看文档</n-button>
      </div>
    </div>
  </div>
</template>

<style scoped>
.missing {
  display: flex;
  align-items: center;
  justify-content: center;
}

.missing__card {
  display: flex;
  max-width: 460px;
  flex-direction: column;
  align-items: flex-start;
  gap: var(--gl-space-3);
  padding: var(--gl-space-6);
  border: 1px solid var(--gl-surface-line);
  border-radius: var(--gl-radius);
  background: var(--gl-surface);
}

.missing__title {
  font-size: var(--gl-text-xl);
  font-weight: 650;
  letter-spacing: -0.02em;
}

.missing__text {
  font-size: var(--gl-text-base);
  line-height: var(--gl-leading-normal);
  color: var(--gl-ink-soft);
}

.missing__actions {
  display: flex;
  flex-wrap: wrap;
  gap: var(--gl-space-2);
  padding-top: var(--gl-space-2);
}
</style>
