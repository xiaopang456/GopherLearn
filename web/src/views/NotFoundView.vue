<script setup>
// Unknown paths stay behind whatever auth state the visitor has; nothing here
// reveals whether a route exists for other users.
</script>

<template>
  <div class="missing">
    <div class="gl-card missing__card">
      <p class="gl-eyebrow">404</p>
      <h1>这个地址没有对应的页面</h1>
      <p class="gl-muted">
        地址可能拼错了，或者该功能属于尚未开放的阶段。
      </p>
      <RouterLink class="gl-btn gl-btn--primary" :to="{ name: 'chat' }">
        回到对话
      </RouterLink>
    </div>
  </div>
</template>

<style scoped>
.missing {
  display: flex;
  min-height: 100dvh;
  align-items: center;
  justify-content: center;
  padding: 1.5rem;
}

.missing__card {
  display: flex;
  max-width: 420px;
  padding: 1.8rem;
  flex-direction: column;
  gap: 0.7rem;
  align-items: flex-start;
}

.missing__card .gl-btn {
  text-decoration: none;
}
</style>
