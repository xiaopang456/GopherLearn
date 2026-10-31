<script setup>
import { computed } from 'vue'

import { agentResultRows } from '../lib/agentResultText.js'
import { debugUiEnabled } from '../lib/debugUi.js'

/**
 * What one agent turn's answer was based on.
 *
 * The card answers two questions a learner actually has after reading an
 * answer: where did this come from, and what did my own material have to say
 * about it. The server records both, in its own words; `agentResultText.js`
 * turns them into Chinese, and this card shows them.
 *
 * The card is not shown when it would have nothing to say. A turn that recorded
 * no basis and no state would open onto an empty box, and a heading about a
 * fact that was never written down is noise, not information.
 *
 * What is *not* on this card: the run id, the message id and the raw result
 * object. Those are facts about the plumbing rather than about the answer, and
 * they are what a developer needs when something goes wrong — so they live
 * behind the debug switch below, which is on only in a development build.
 */
const props = defineProps({
  result: { type: Object, required: true },
  runId: { type: String, default: '' },
  messageId: { type: String, default: '' },
})

const rows = computed(() => agentResultRows(props.result))
const debug = computed(() => debugUiEnabled())
</script>

<template>
  <details v-if="rows.length" class="basis" data-test="agent-result">
    <summary class="basis__title">
      回答依据{{ props.result.replayed ? '（原请求重放）' : '' }}
    </summary>
    <p
      v-for="row in rows"
      :key="row.key"
      class="basis__row"
      :data-test="`agent-result-${row.key}`"
    >
      {{ row.label }}：{{ row.text }}
    </p>
  </details>

  <details v-if="debug" class="basis basis--debug" data-test="agent-result-debug">
    <summary class="basis__title">调试信息</summary>
    <p class="basis__row">Run：{{ props.runId }} · 消息：{{ props.messageId }}</p>
    <details>
      <summary class="basis__title">原始结果</summary>
      <pre class="basis__json">{{ JSON.stringify(props.result, null, 2) }}</pre>
    </details>
  </details>
</template>

<style scoped>
/* A quiet line under the answer: the same weight as the citation list above
   it, because it is the same kind of thing — where this answer came from. */
.basis {
  margin-top: 0.6rem;
  border-top: 1px dashed var(--gl-surface-line);
  padding-top: 0.45rem;
}

.basis__title {
  cursor: pointer;
  color: var(--gl-ink-soft);
  font-size: 0.8rem;
  font-weight: 600;
}

.basis__row {
  margin: 0.3rem 0 0;
  color: var(--gl-ink-soft);
  font-size: 0.82rem;
  line-height: 1.6;
}

.basis__json {
  margin: 0.35rem 0 0;
  font-size: 0.75rem;
  line-height: 1.5;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}
</style>
