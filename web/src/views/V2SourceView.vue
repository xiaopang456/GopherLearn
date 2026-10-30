<script setup>
import { computed, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { NButton } from 'naive-ui'

import GlPageHeader from '../components/GlPageHeader.vue'
import ServerNotice from '../components/ServerNotice.vue'
import { readV2Source } from '../api/v2Documents.js'

const route = useRoute()
const router = useRouter()
const source = ref(null)
const error = ref('')
let generation = 0

watch(
  () => route.fullPath,
  async () => {
    const current = ++generation
    source.value = null
    error.value = ''
    try {
      const result = await readV2Source(
        route.params.documentId,
        route.query.version,
        route.query.start_line,
        route.query.end_line,
      )
      if (current === generation) source.value = result
    } catch (cause) {
      if (current === generation) error.value = `${cause.message} (${cause.code})`
    }
  },
  { immediate: true },
)

// The cited range is the reason a reader came here, so it is marked rather
// than left for them to count lines.
const from = computed(() => Number(route.query.start_line ?? 0))
const to = computed(() => Number(route.query.end_line ?? 0))

function inRange(line) {
  return from.value > 0 && line >= from.value && line <= to.value
}

function back() {
  router.push({ name: 'v2-documents' })
}
</script>

<template>
  <div class="gl-page source">
    <GlPageHeader
      title="文档原文"
      :description="source
        ? `${source.document_name} · 版本 ${source.document_version}`
        : '正在读取指定版本的原文。'"
    >
      <template #actions>
        <n-button quaternary size="small" @click="back">← 返回文档</n-button>
      </template>
    </GlPageHeader>

    <ServerNotice v-if="error" :message="error" />

    <p v-else-if="!source" class="source__loading">正在读取原文…</p>

    <div v-else class="source__body">
      <p v-if="from > 0" class="source__hint">
        高亮的是这次引用到的第 {{ from }}–{{ to }} 行。
      </p>
      <ol class="source__lines">
        <li
          v-for="line in source.lines"
          :key="line.number"
          :id="`L${line.number}`"
          class="source__line"
          :class="{ 'source__line--cited': inRange(line.number) }"
        >
          <span class="source__no" aria-hidden="true">{{ line.number }}</span>
          <code class="source__text">{{ line.text }}</code>
        </li>
      </ol>
    </div>
  </div>
</template>

<style scoped>
.source {
  max-width: 960px;
}

.source__loading {
  color: var(--gl-ink-faint);
  font-size: var(--gl-text-sm);
}

.source__hint {
  margin-bottom: var(--gl-space-3);
  font-size: var(--gl-text-sm);
  color: var(--gl-accent-ink);
}

.source__lines {
  list-style: none;
  margin: 0;
  padding: 0;
  border: 1px solid var(--gl-surface-line);
  border-radius: var(--gl-radius);
  background: var(--gl-surface);
  overflow: hidden;
}

.source__line {
  display: grid;
  grid-template-columns: 48px minmax(0, 1fr);
  align-items: baseline;
  gap: var(--gl-space-3);
  padding: 1px var(--gl-space-4) 1px 0;
  font-size: var(--gl-text-sm);
  line-height: 1.7;
}

.source__line--cited {
  background: var(--gl-accent-soft);
}

.source__no {
  padding-right: var(--gl-space-3);
  border-right: 1px solid var(--gl-surface-line);
  font-family: var(--gl-mono);
  font-size: var(--gl-text-xs);
  color: var(--gl-ink-faint);
  text-align: right;
  user-select: none;
}

.source__line--cited .source__no {
  color: var(--gl-accent-ink);
  border-right-color: #bfdcd7;
}

.source__text {
  font-family: var(--gl-mono);
  font-size: var(--gl-text-sm);
  white-space: pre-wrap;
  overflow-wrap: anywhere;
  color: var(--gl-ink);
}
</style>
