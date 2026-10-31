<script setup>
import { computed } from 'vue'

import AppIcon from './AppIcon.vue'
import { errorCodeText } from '../lib/displayText.js'
import { debugUiEnabled } from '../lib/debugUi.js'

/**
 * The one component allowed to render a server-supplied string. It is safe
 * because callers pass `safeErrorMessage()`, which whitelists machine codes and
 * substitutes a generic sentence for anything else — and because the text is
 * interpolated into a text node, never parsed as markup.
 *
 * The machine code itself is not shown to a learner: `agent_run_busy` is a word
 * for a program, and the sentence above it already says what happened. Where
 * this client knows the code, it says what the code means in Chinese instead —
 * "上一轮回答还没结束，等它结束再发" tells a learner what to do, where
 * `agent_run_busy` tells them nothing. The raw code is kept for the developer
 * and is shown only when the debug switch is on.
 */
const props = defineProps({
  code: { type: String, default: '' },
  message: { type: String, default: '' },
  variant: { type: String, default: 'error' }, // error | stopped | info
})

const explanation = computed(() => errorCodeText(props.code))
const debug = computed(() => debugUiEnabled())
</script>

<template>
  <div class="notice" :class="`notice--${variant}`" role="status">
    <AppIcon name="alert" :size="18" />
    <div class="notice__body">
      <p class="notice__message">{{ message }}</p>
      <p v-if="explanation" class="notice__detail">{{ explanation }}</p>
      <p v-if="code && debug" class="notice__code">{{ code }}</p>
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

/* What the machine code means, said in Chinese. */
.notice__detail {
  margin: var(--gl-space-1) 0 0;
  overflow-wrap: anywhere;
}

/* The code itself: for a developer, and only when the debug switch is on. */
.notice__code {
  margin: var(--gl-space-1) 0 0;
  font-family: var(--gl-mono);
  font-size: var(--gl-text-xs);
  opacity: 0.8;
}
</style>