<script setup>
import { computed } from 'vue'

import { NTag } from 'naive-ui'

/**
 * One place that turns a server state into something a person can read.
 *
 * The server speaks in machine words (`ready`, `failed`, `archived`); showing
 * those raw in the UI was one of the things that made the pages look like
 * debug output. Unknown states are passed through unchanged rather than
 * hidden, so a new server state is visible instead of silently dropped.
 */
const props = defineProps({
  status: { type: String, default: '' },
})

const TONES = {
  // document index states
  ready: { text: '就绪', tone: 'success' },
  failed: { text: '失败', tone: 'error' },
  pending: { text: '等待处理', tone: 'warning' },
  indexing: { text: '索引中', tone: 'info' },
  cleanup_pending: { text: '待清理', tone: 'warning' },
  deleting: { text: '删除中', tone: 'info' },
  deleted: { text: '已删除', tone: 'default' },
  // memory states
  active: { text: '生效中', tone: 'success' },
  archived: { text: '已归档', tone: 'default' },
  superseded: { text: '已被替代', tone: 'warning' },
}

const known = computed(() => TONES[props.status] ?? null)
const text = computed(() => known.value?.text ?? props.status)
const tone = computed(() => known.value?.tone ?? 'default')
</script>

<template>
  <n-tag :type="tone" size="small" round :bordered="false" data-test="status-tag">
    {{ text }}
  </n-tag>
</template>
