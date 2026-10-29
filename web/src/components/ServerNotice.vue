<script setup>
import AppIcon from './AppIcon.vue'

/**
 * The one component allowed to render a server-supplied string. It is safe
 * because callers pass `safeErrorMessage()`, which whitelists machine codes and
 * substitutes a generic sentence for anything else — and because the text is
 * interpolated into a text node, never parsed as markup.
 */
defineProps({
  code: { type: String, default: '' },
  message: { type: String, default: '' },
  variant: { type: String, default: 'error' }, // error | stopped | info
})
</script>

<template>
  <div class="notice" :class="`notice--${variant}`" role="status">
    <AppIcon name="alert" :size="18" />
    <div class="notice__body">
      <p class="notice__message">{{ message }}</p>
      <p v-if="code" class="notice__code">{{ code }}</p>
    </div>
  </div>
</template>

<style scoped>
.notice {
  display: flex;
  gap: 0.6rem;
  align-items: flex-start;
  padding: 0.7rem 0.85rem;
  border-radius: var(--gl-radius-sm);
  border: 1px solid #e6c9c6;
  background: var(--gl-danger-soft);
  color: var(--gl-danger);
  font-size: 0.92rem;
}

.notice--stopped {
  border-color: #e4d7bd;
  background: var(--gl-warn-soft);
  color: var(--gl-warn);
}

.notice--info {
  border-color: var(--gl-surface-line);
  background: var(--gl-surface-soft);
  color: var(--gl-ink-soft);
}

.notice__body {
  flex: 1;
}

.notice__message {
  margin: 0;
  font-weight: 600;
  overflow-wrap: anywhere;
}

.notice__code {
  margin: 0.2rem 0 0;
  font-family: var(--gl-mono);
  font-size: 0.78rem;
  opacity: 0.85;
}
</style>
