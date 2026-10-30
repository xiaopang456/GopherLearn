<script setup>
import { NEmpty } from 'naive-ui'

/**
 * The shared empty state.
 *
 * Every list in the app says the same three things when it has nothing in
 * it: what this list is for, why it is empty, and the one action that fills
 * it. A bare white area used to be the only thing a user saw.
 */
defineProps({
  description: { type: String, default: '这里还没有内容' },
})
</script>

<template>
  <n-empty class="gl-empty" size="small" :description="description">
    <template v-if="$slots.icon" #icon>
      <slot name="icon" />
    </template>
    <template v-if="$slots.action" #extra>
      <div class="gl-empty__action">
        <slot name="action" />
      </div>
    </template>
  </n-empty>
</template>

<style scoped>
.gl-empty {
  padding: var(--gl-space-7) var(--gl-space-4);
  background: var(--gl-surface);
  border: 1px solid var(--gl-surface-line);
  border-radius: var(--gl-radius);
}

.gl-empty__action {
  padding-top: var(--gl-space-4);
}
</style>
