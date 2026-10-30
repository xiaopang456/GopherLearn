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
  gap: var(--gl-space-2);
  align-items: flex-start;
  padding: var(--gl-space-3);
  border-radius: var(--gl-radius-sm);
  background: var(--gl-danger-soft);
  color: var(--gl-danger);
  font-size: var(--gl-text-sm);
  line-height: var(--gl-leading-normal);
}

.notice--stopped {
  background: var(--gl-warn-soft);
  color: var(--gl-warn);
}

.notice--info {
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
  margin: var(--gl-space-1) 0 0;
  font-family: var(--gl-mono);
  font-size: var(--gl-text-xs);
  opacity: 0.8;
}
</style>
