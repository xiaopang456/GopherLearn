<script setup>
import { computed } from 'vue'

import { NTag } from 'naive-ui'

import { statusTag } from '../lib/displayText.js'
import { debugUiEnabled } from '../lib/debugUi.js'

/**
 * One place that turns a server state into something a person can read.
 *
 * The server speaks in machine words (`ready`, `failed`, `archived`); showing
 * those raw in the UI was one of the things that made the pages look like
 * debug output. The wording and its colour now come from the shared status
 * table in `displayText.js`, so the document list and the memory list cannot
 * drift apart — and a state neither of them knows is called "状态未识别"
 * rather than shown as a raw key.
 *
 * A developer reading an unknown state still wants to know which state it was,
 * so the raw word is shown next to the tag when the debug switch is on.
 */
const props = defineProps({
  status: { type: String, default: '' },
})

const tag = computed(() => statusTag(props.status))
const debug = computed(() => debugUiEnabled())
</script>

<template>
  <n-tag :type="tag.tone" size="small" round :bordered="false" data-test="status-tag">
    {{ tag.text }}
    <span v-if="debug" class="status-tag__raw">{{ props.status }}</span>
  </n-tag>
</template>

<style scoped>
.status-tag__raw {
  margin-left: 0.35em;
  opacity: 0.7;
  font-size: 0.72rem;
  font-weight: 500;
}
</style>
