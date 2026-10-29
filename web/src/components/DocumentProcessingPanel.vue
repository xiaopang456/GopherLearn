<script setup>
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { getDocumentProcessing } from '../api/docOps.js'

/**
 * The owner's view of how one document is being processed. It shows stages, times and a progress
 * count - nothing about nodes or attempts' internals (the interface does not carry them).
 *
 * Polling: only while this panel exists (the parent renders it when expanded). It asks once on
 * mount, then every second while the document is pending or indexing; once the answer says ready
 * or failed it stops. A change of the `documentStatus` prop (the list refreshed) triggers one
 * more request. Unmounting cancels the timer and the request in flight.
 */
const props = defineProps({
  documentId: { type: String, required: true },
  documentStatus: { type: String, default: '' },
  intervalMs: { type: Number, default: 1000 },
})

const timeline = ref(null)
const failure = ref('')
const loading = ref(true)
let controller = null
let timer = null
let alive = true

const IN_PROGRESS = new Set(['pending', 'indexing'])

function clock(iso) {
  const at = new Date(iso)
  if (Number.isNaN(at.getTime())) return ''
  const two = (n) => String(n).padStart(2, '0')
  return `${two(at.getHours())}:${two(at.getMinutes())}:${two(at.getSeconds())}.${String(at.getMilliseconds()).padStart(3, '0')}`
}

const rows = computed(() => {
  const stages = timeline.value?.stages ?? []
  return stages.map((stage, index) => {
    const previous = index > 0 ? new Date(stages[index - 1].at).getTime() : null
    const delta = previous === null ? null : Math.max(0, new Date(stage.at).getTime() - previous) / 1000
    return {
      key: `${stage.stage}-${index}`,
      stage: stage.stage,
      label: stage.label,
      time: clock(stage.at),
      delta: delta === null ? '' : `+${delta.toFixed(2)}s`,
    }
  })
})

const showProgress = computed(() => {
  const t = timeline.value
  return !!t?.progress && t.progress.total > 0 && IN_PROGRESS.has(t.status)
})
const percent = computed(() => {
  const p = timeline.value?.progress
  return p && p.total > 0 ? Math.min(100, Math.round((p.completed / p.total) * 100)) : 0
})

function stop() {
  if (timer !== null) { clearTimeout(timer); timer = null }
  if (controller) { controller.abort(); controller = null }
}

async function load() {
  if (!alive) return
  if (controller) controller.abort()
  controller = new AbortController()
  const mine = controller
  try {
    const next = await getDocumentProcessing(props.documentId, { signal: mine.signal })
    if (!alive || mine.signal.aborted) return
    timeline.value = next
    failure.value = ''
    loading.value = false
    if (timer !== null) { clearTimeout(timer); timer = null }
    if (IN_PROGRESS.has(next.status)) {
      timer = setTimeout(load, props.intervalMs)
    }
  } catch (cause) {
    if (!alive || mine.signal.aborted) return
    loading.value = false
    failure.value = cause?.message || '无法读取处理详情。'
    if (timer !== null) clearTimeout(timer)
    timer = setTimeout(load, Math.max(props.intervalMs, 3000)) // a failed read is retried, slowly
  }
}

watch(() => props.documentStatus, (now, before) => {
  if (now !== before) {
    if (timer !== null) { clearTimeout(timer); timer = null }
    load()
  }
})
onMounted(load)
onBeforeUnmount(() => { alive = false; stop() })
</script>

<template>
  <section class="proc" data-test="processing-panel" aria-label="处理详情">
    <p v-if="loading" class="gl-muted" data-test="processing-loading">正在读取处理详情…</p>
    <p v-else-if="failure && !timeline" class="proc__error" role="alert" data-test="processing-error">{{ failure }}</p>

    <template v-if="timeline">
      <p v-if="timeline.mode === 'legacy'" data-test="processing-legacy">
        当前为本地处理模式 · 状态：{{ timeline.status }}
      </p>

      <template v-else>
        <h4 class="proc__title">
          处理过程 · {{ timeline.status }}
          <span v-if="timeline.attempts > 1" class="proc__attempts" data-test="processing-attempts">共 {{ timeline.attempts }} 次尝试</span>
        </h4>

        <div v-if="showProgress" class="proc__progress" data-test="processing-progress">
          <div class="proc__bar" role="progressbar" :aria-valuenow="timeline.progress.completed" aria-valuemin="0" :aria-valuemax="timeline.progress.total">
            <span :style="{ width: percent + '%' }" />
          </div>
          <span>{{ timeline.progress.completed }} / {{ timeline.progress.total }}</span>
        </div>

        <ol class="proc__list" data-test="processing-stages">
          <li v-for="row in rows" :key="row.key" :class="['proc__item', { 'proc__item--retried': row.stage === 'retried', 'proc__item--failed': row.stage === 'failed' }]" :data-stage="row.stage">
            <span class="proc__label">{{ row.label }}</span>
            <span class="proc__time">{{ row.time }}</span>
            <span class="proc__delta">{{ row.delta }}</span>
          </li>
        </ol>

        <p v-if="timeline.status === 'failed' && timeline.error" class="proc__error" role="alert" data-test="processing-failure">
          {{ timeline.error.label }}
        </p>
      </template>
    </template>
  </section>
</template>

<style scoped>
.proc { margin-top: 0.6rem; padding: 0.6rem 0.8rem; border: 1px solid var(--gl-surface-line); border-radius: var(--gl-radius-sm); background: var(--gl-surface-soft); }
.proc__title { margin: 0 0 0.4rem; font-size: 0.95rem; }
.proc__attempts { margin-left: 0.5rem; padding: 0.05rem 0.45rem; border-radius: 999px; background: var(--gl-accent-soft); color: var(--gl-accent-ink); font-size: 0.78rem; font-weight: 600; }
.proc__list { list-style: none; margin: 0; padding: 0 0 0 0.9rem; border-left: 2px solid var(--gl-surface-line); }
.proc__item { display: grid; grid-template-columns: 1fr auto auto; gap: 0.6rem; padding: 0.25rem 0; position: relative; }
.proc__item::before { content: ''; position: absolute; left: -1.15rem; top: 0.62rem; width: 0.55rem; height: 0.55rem; border-radius: 50%; background: var(--gl-accent); }
.proc__item--retried { color: var(--gl-ink); background: var(--gl-accent-soft); border-radius: 4px; padding-left: 0.3rem; }
.proc__item--retried::before { background: #c9922f; }
.proc__item--failed::before { background: var(--gl-danger); }
.proc__time { font-variant-numeric: tabular-nums; color: var(--gl-ink-soft); }
.proc__delta { min-width: 4.2rem; text-align: right; font-variant-numeric: tabular-nums; color: var(--gl-ink-soft); }
.proc__progress { display: flex; align-items: center; gap: 0.6rem; margin-bottom: 0.4rem; }
.proc__bar { flex: 1; height: 0.5rem; border-radius: 999px; background: var(--gl-surface-line); overflow: hidden; }
.proc__bar span { display: block; height: 100%; background: var(--gl-accent); transition: width 0.3s; }
.proc__error { color: var(--gl-danger); margin: 0.4rem 0 0; }
</style>
