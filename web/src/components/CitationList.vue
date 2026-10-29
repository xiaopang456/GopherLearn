<script setup>
import { computed, ref } from 'vue'
import { useRouter } from 'vue-router'

import AppIcon from './AppIcon.vue'

/**
 * Citations for one grounded answer.
 *
 * Collapsed by default: up to five chunks of quoted text, all permanently open,
 * would out-shout the answer they support. The title and section identify where
 * a snippet came from at a glance; the body is one click away.
 *
 * Every field is interpolated as text, so a snippet containing markup renders as
 * that markup written out — the same rule the transcript follows.
 */
const props = defineProps({
  sources: { type: Array, default: () => [] },
})

const router = useRouter()
const open = ref(false)

const items = computed(() =>
  (props.sources ?? [])
    .map((source) => ({
      key: source.sourceId || source.chunkId || source.documentId || '',
      title: source.title || '未命名文档',
      section: source.section || '',
      snippet: source.snippet || '',
      version: source.documentVersion || '',
      document: source.documentId || '',
      original: source.originalV2 || null,
      unavailable: source.navigationUnavailable === true,
    }))
    .filter((item) => item.title || item.snippet),
)

const count = computed(() => items.value.length)
</script>

<template>
  <section v-if="count > 0" class="sources" data-test="citation-list">
    <button
      type="button"
      class="sources__toggle"
      :aria-expanded="String(open)"
      data-test="citation-toggle"
      @click="open = !open"
    >
      <AppIcon name="knowledge" :size="14" />
      <span class="sources__label">参考来源</span>
      <span class="sources__count">{{ count }}</span>
      <span class="sources__chevron" :class="{ 'sources__chevron--open': open }">
        {{ open ? '收起' : '展开' }}
      </span>
    </button>

    <ol v-if="open" class="sources__list">
      <li v-for="item in items" :key="item.key" class="sources__item">
        <p class="sources__title">
          {{ item.title }}
          <span v-if="item.section" class="sources__section">{{ item.section }}</span>
        </p>
        <p v-if="item.unavailable" class="gl-muted">文档 {{ item.document }}，版本 {{ item.version }}；Collection 尚未接通。</p>
        <button v-if="router && item.original && item.document && item.version" type="button" class="gl-btn" data-test="read-v2-source" @click="router.push({ name: 'v2-source', params: { documentId: item.document }, query: { version: item.version, start_line: String(item.original.start_line), end_line: String(item.original.end_line) } })">按引用版本及行范围读取原文</button>
        <p v-if="item.snippet" class="sources__snippet">{{ item.snippet }}</p>
      </li>
    </ol>
  </section>
</template>

<style scoped>
.sources {
  margin-top: 0.6rem;
  border-top: 1px dashed var(--gl-surface-line);
  padding-top: 0.45rem;
}

.sources__toggle {
  display: flex;
  align-items: center;
  gap: 0.4rem;
  width: 100%;
  padding: 0.1rem 0;
  background: none;
  border: 0;
  color: var(--gl-ink-soft);
  font-size: 0.8rem;
  font-weight: 600;
  text-align: left;
}

.sources__toggle:hover {
  color: var(--gl-accent-ink);
}

.sources__label {
  letter-spacing: 0.02em;
}

.sources__count {
  min-width: 1.6em;
  padding: 0 0.25em;
  border-radius: 999px;
  background: var(--gl-accent-soft);
  color: var(--gl-accent-ink);
  font-size: 0.72rem;
  text-align: center;
}

.sources__chevron {
  margin-left: auto;
  font-size: 0.74rem;
  font-weight: 500;
  color: var(--gl-ink-faint);
}

.sources__list {
  list-style: none;
  margin: 0.35rem 0 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 0.4rem;
}

.sources__item {
  padding: 0.45rem 0.6rem;
  border-radius: var(--gl-radius-sm);
  background: var(--gl-surface-soft);
  border-left: 2px solid var(--gl-accent-soft);
}

.sources__title {
  display: flex;
  flex-wrap: wrap;
  align-items: baseline;
  gap: 0.4rem;
  margin: 0;
  font-size: 0.82rem;
  font-weight: 700;
  color: var(--gl-ink);
}

.sources__section {
  padding: 0.05em 0.45em;
  border-radius: 999px;
  background: var(--gl-surface);
  border: 1px solid var(--gl-surface-line);
  font-size: 0.7rem;
  font-weight: 600;
  color: var(--gl-ink-soft);
}

.sources__snippet {
  margin: 0.25rem 0 0;
  font-size: 0.82rem;
  line-height: 1.6;
  color: var(--gl-ink-soft);
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}
</style>
