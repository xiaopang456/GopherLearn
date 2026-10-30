<script setup>
import { computed, ref } from 'vue'

import { formatStepDuration, stepStatusText } from '../lib/agentSteps.js'

/**
 * The steps a finished turn took, shown under the answer it produced.
 *
 * The rows come from the same reducer the live panel reads, so a turn that is opened
 * again shows what the turn showed while it ran: the same kinds, the same words, the
 * same status. Nothing here adds a fact — the rows say which step was which, whether
 * it succeeded, and how long the server measured it.
 *
 * Closed by default. A trail is there to be asked for, not to shout over the answer
 * it belongs to, and a conversation with twenty turns would otherwise open with twenty
 * lists of steps.
 */
const props = defineProps({
  steps: { type: Array, default: () => [] },
})

const open = ref(false)
const hasSteps = computed(() => Array.isArray(props.steps) && props.steps.length > 0)

function duration(step) {
  return formatStepDuration(step?.durationMillis)
}
</script>

<template>
  <section v-if="hasSteps" class="trail" data-test="agent-history-steps">
    <button
      type="button"
      class="trail__toggle"
      data-test="agent-history-steps-toggle"
      :aria-expanded="String(open)"
      @click="open = !open"
    >
      {{ open ? '收起执行过程' : `展开执行过程（${props.steps.length} 步）` }}
    </button>

    <ol v-if="open" class="trail__list" data-test="agent-history-step-list">
      <li
        v-for="(step, index) in props.steps"
        :key="step.id ?? index"
        class="trail__step"
      >
        <span class="trail__index">{{ index + 1 }}</span>
        <span class="trail__label">{{ step.label ?? '步骤' }}</span>
        <span class="trail__state" data-test="agent-history-step-state">
          {{ stepStatusText(step.status) }}
        </span>
        <span v-if="step.detail" class="trail__detail">{{ step.detail }}</span>
        <span v-if="duration(step)" class="trail__time" data-test="agent-history-step-time">
          {{ duration(step) }}
        </span>
      </li>
    </ol>
  </section>
</template>

<style scoped>
/* As quiet as the live strip it repeats: a hairline list under the answer, with
   the same wording and the same reading order. */
.trail {
  margin-top: var(--gl-space-3);
}

.trail__toggle {
  padding: 3px var(--gl-space-2);
  border: 1px solid var(--gl-surface-line);
  border-radius: var(--gl-radius-pill);
  background: var(--gl-surface);
  color: var(--gl-ink-soft);
  font-size: var(--gl-text-sm);
  font-weight: 500;
}

.trail__toggle:hover {
  border-color: var(--gl-line-strong);
  color: var(--gl-ink);
}

.trail__list {
  list-style: none;
  margin: var(--gl-space-2) 0 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.trail__step {
  display: flex;
  align-items: baseline;
  gap: var(--gl-space-2);
  padding: 5px 0;
  border-top: 1px solid var(--gl-surface-line);
  font-size: var(--gl-text-sm);
  color: var(--gl-ink-soft);
}

.trail__index {
  font-family: var(--gl-mono);
  font-size: var(--gl-text-xs);
  color: var(--gl-ink-faint);
}

.trail__label {
  overflow-wrap: anywhere;
}

.trail__state {
  flex: none;
  font-size: var(--gl-text-xs);
  color: var(--gl-ink-faint);
  white-space: nowrap;
}

.trail__detail {
  flex: 1 1 auto;
  color: var(--gl-ink-soft);
  overflow-wrap: anywhere;
}

.trail__time {
  flex: none;
  font-family: var(--gl-mono);
  font-size: var(--gl-text-xs);
  color: var(--gl-ink-faint);
  white-space: nowrap;
}
</style>
