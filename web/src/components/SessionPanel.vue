<script setup>
import { ref } from 'vue'

import AppIcon from './AppIcon.vue'
import { modeLabel, SessionMode } from '../lib/modes.js'

const props = defineProps({
  sessions: { type: Array, default: () => [] },
  activeId: { type: String, default: '' },
  loading: { type: Boolean, default: false },
  error: { type: String, default: '' },
})

const emit = defineEmits(['select', 'create', 'remove'])

const choosing = ref(false)

/**
 * The two things a new session can be. Offered as an explicit choice rather than
 * one ambiguous "+", because the mode cannot be changed after creation.
 */
const CHOICES = [
  { mode: SessionMode.Chat, icon: 'chat', hint: '通用提问，可单轮挂知识库' },
  {
    mode: SessionMode.LearningAgent,
    icon: 'learning',
    hint: '由服务端能力决定如何处理问题',
  },
]

function modeGlyph(mode) {
  if (mode === SessionMode.LearningAgent) return 'learning'
  if (mode === SessionMode.Chat) return 'chat'
  // An unrecognised mode is surfaced rather than smoothed over.
  return 'alert'
}

function when(dateString) {
  if (!dateString) return ''
  const stamp = Date.parse(dateString)
  if (Number.isNaN(stamp)) return ''
  return new Intl.DateTimeFormat('zh-CN', {
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
  }).format(new Date(stamp))
}

function pick(mode) {
  choosing.value = false
  emit('create', mode)
}

function onHeadKeydown(event) {
  if (event.key === 'Escape') choosing.value = false
}

function askRemove(session) {
  // The list is the only place a session disappears from, so confirm here.
  if (typeof window === 'undefined' || typeof window.confirm !== 'function') {
    emit('remove', session.id)
    return
  }
  if (window.confirm(`删除会话「${session.title}」？该操作不可撤销。`)) {
    emit('remove', session.id)
  }
}
</script>

<template>
  <aside class="panel">
    <div class="panel__head" @keydown="onHeadKeydown">
      <h2 class="gl-eyebrow">会话</h2>
      <button
        type="button"
        class="gl-btn gl-btn--primary panel__new"
        data-test="new-session"
        aria-haspopup="menu"
        :aria-expanded="String(choosing)"
        @click="choosing = !choosing"
      >
        <AppIcon name="plus" :size="16" />
        <span>新建</span>
      </button>
    </div>

    <ul v-if="choosing" class="chooser" role="menu" data-test="new-session-menu">
      <li v-for="choice in CHOICES" :key="choice.mode">
        <button
          type="button"
          class="chooser__item"
          role="menuitem"
          data-test="new-session-choice"
          @click="pick(choice.mode)"
        >
          <span class="chooser__glyph">
            <AppIcon :name="choice.icon" :size="16" />
          </span>
          <span class="chooser__text">
            <span class="chooser__title">{{ modeLabel(choice.mode) }}</span>
            <span class="chooser__hint">{{ choice.hint }}</span>
          </span>
        </button>
      </li>
    </ul>

    <p v-if="props.loading" class="panel__state">正在读取会话列表…</p>
    <p v-else-if="props.error" class="panel__state panel__state--error">
      {{ props.error }}
    </p>
    <p v-else-if="props.sessions.length === 0 && !choosing" class="panel__state">
      还没有会话。新建一个即可开始提问。
    </p>

    <ul v-else-if="props.sessions.length > 0" class="panel__list">
      <li v-for="session in props.sessions" :key="session.id">
        <div
          class="panel__row"
          :class="{ 'panel__row--active': session.id === props.activeId }"
        >
          <button
            type="button"
            class="panel__pick"
            :aria-current="session.id === props.activeId ? 'true' : undefined"
            @click="emit('select', session.id)"
          >
            <span
              class="panel__mode"
              :class="{
                'panel__mode--agent': session.mode === SessionMode.LearningAgent,
              }"
              data-test="session-mode"
              :title="modeLabel(session.mode)"
            >
              <AppIcon :name="modeGlyph(session.mode)" :size="14" />
              <span class="sr-only">{{ modeLabel(session.mode) }}</span>
            </span>
            <span class="panel__text">
              <span class="panel__title">{{ session.title }}</span>
              <span class="panel__when">{{ when(session.updatedAt) }}</span>
            </span>
          </button>
          <button
            type="button"
            class="panel__remove"
            title="删除会话"
            :aria-label="`删除会话 ${session.title}`"
            @click="askRemove(session)"
          >
            <AppIcon name="trash" :size="16" />
          </button>
        </div>
      </li>
    </ul>
  </aside>
</template>

<style scoped>
.panel {
  display: flex;
  flex-direction: column;
  gap: 0.5rem;
  min-height: 0;
  padding: 0.75rem;
  background: var(--gl-surface-soft);
  border-right: 1px solid var(--gl-surface-line);
  overflow: hidden;
}

.panel__head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 0.5rem;
}

.panel__new {
  padding: 0.3em 0.6em;
  font-size: 0.82rem;
}

.chooser {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 0.25rem;
}

.chooser__item {
  display: flex;
  align-items: flex-start;
  gap: 0.5rem;
  width: 100%;
  padding: 0.45rem 0.5rem;
  border: 1px solid var(--gl-surface-line);
  border-radius: var(--gl-radius-sm);
  background: var(--gl-surface);
  text-align: left;
}

.chooser__item:hover {
  border-color: var(--gl-accent);
  background: #fff;
}

.chooser__glyph {
  display: flex;
  width: 24px;
  height: 24px;
  flex: none;
  align-items: center;
  justify-content: center;
  border-radius: 6px;
  background: var(--gl-accent-soft);
  color: var(--gl-accent-ink);
}

.chooser__text {
  display: flex;
  flex-direction: column;
  min-width: 0;
}

.chooser__title {
  font-size: 0.9rem;
  font-weight: 700;
}

.chooser__hint {
  font-size: 0.74rem;
  color: var(--gl-ink-faint);
}

.panel__state {
  margin: 0.6rem 0 0;
  font-size: 0.86rem;
  color: var(--gl-ink-faint);
}

.panel__state--error {
  color: var(--gl-danger);
}

.panel__list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 0.3rem;
  overflow-y: auto;
  min-height: 0;
}

.panel__row {
  display: flex;
  align-items: stretch;
  border-radius: var(--gl-radius-sm);
  background: var(--gl-surface);
  border: 1px solid var(--gl-surface-line);
}

.panel__row--active {
  border-color: var(--gl-accent);
  background: #fff;
  box-shadow: inset 3px 0 0 var(--gl-accent);
}

.panel__pick {
  flex: 1;
  min-width: 0;
  display: flex;
  align-items: flex-start;
  gap: 0.45rem;
  padding: 0.5rem 0.55rem;
  background: none;
  border: 0;
  text-align: left;
}

/* Quiet by design: the marker identifies the mode without competing with the
   title for the eye. */
.panel__mode {
  display: flex;
  flex: none;
  align-items: center;
  padding-top: 0.2rem;
  color: var(--gl-ink-faint);
}

.panel__mode--agent {
  color: var(--gl-accent);
}

.panel__text {
  display: flex;
  flex-direction: column;
  min-width: 0;
}

.panel__title {
  font-size: 0.92rem;
  font-weight: 600;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.panel__when {
  font-size: 0.74rem;
  color: var(--gl-ink-faint);
}

.panel__remove {
  display: flex;
  align-items: center;
  padding: 0 0.55rem;
  background: none;
  border: 0;
  border-left: 1px solid var(--gl-surface-line);
  color: var(--gl-ink-faint);
}

.panel__remove:hover {
  color: var(--gl-danger);
  background: var(--gl-danger-soft);
}

@media (max-width: 1020px) {
  .panel {
    position: absolute;
    z-index: 30;
    inset: 0 auto 0 var(--gl-rail);
    width: min(78vw, 300px);
    box-shadow: var(--gl-shadow);
  }
}
</style>
