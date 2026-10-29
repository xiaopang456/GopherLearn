<script setup>
import { computed, ref } from 'vue'

import AppIcon from './AppIcon.vue'
import { SessionMode } from '../lib/modes.js'
import { canSend, validateMessageContent } from '../lib/validation.js'

const props = defineProps({
  modelValue: { type: String, default: '' },
  streaming: { type: Boolean, default: false },
  disabled: { type: Boolean, default: false },
  placeholder: { type: String, default: '向 GopherLearn 提问…' },
  /** The server-owned mode of the open session; '' while still unknown. */
  mode: { type: String, default: '' },
  knowledge: {
    type: Object,
    default: () => ({ enabled: false, selectedCollectionIds: [] }),
  },
  collections: { type: Array, default: () => [] },
  canUseKnowledge: { type: Boolean, default: false },
})

const emit = defineEmits([
  'update:modelValue',
  'update:knowledge',
  'submit',
  'abort',
  'open-knowledge',
])

const localError = ref('')
const picking = ref(false)

const sendable = computed(
  () => canSend(props.modelValue) && !props.disabled && !props.streaming,
)

// Grounding is a per-turn Chat option. A Learning Agent session picks its own
// tools server-side, and an unrecognised mode gets no optional capability.
const showsKnowledge = computed(() => props.mode === SessionMode.Chat)
const selected = computed(() => (props.knowledge.selectedCollectionIds ?? []).map(String))

function update(event) {
  emit('update:modelValue', event.target.value)
  if (localError.value) localError.value = ''
}

function onKeydown(event) {
  if (event.key !== 'Enter') return
  // Chinese IME commits with Enter; that keystroke must not send a draft.
  if (event.isComposing || event.keyCode === 229) return
  if (event.shiftKey) return
  event.preventDefault()
  submit()
}

function submit() {
  if (props.streaming) return
  const tooLong = validateMessageContent(props.modelValue)
  if (tooLong) {
    localError.value = tooLong
    return
  }
  if (!sendable.value) return
  emit('submit')
}

function toggleKnowledge() {
  if (!props.canUseKnowledge) return
  picking.value = false
  const enabled = !props.knowledge.enabled
  if (!enabled) {
    emit('update:knowledge', { enabled: false, selectedCollectionIds: [] })
    return
  }
  // Switching on pre-selects nothing it can't name: with no collections the
  // toggle is disabled above, so an empty list here stays an empty list.
  emit('update:knowledge', { enabled: true, selectedCollectionIds: selected.value })
}

function toggleCollection(id) {
  const value = String(id)
  const next = selected.value.includes(value)
    ? selected.value.filter((item) => item !== value)
    : [...selected.value, value]
  emit('update:knowledge', {
    enabled: props.knowledge.enabled,
    selectedCollectionIds: next,
  })
}

function onToolsKeydown(event) {
  if (event.key === 'Escape') picking.value = false
}
</script>

<template>
  <form class="composer" data-test="composer" @submit.prevent="submit">
    <p v-if="localError" class="composer__error">{{ localError }}</p>

    <div
      v-if="showsKnowledge"
      class="composer__tools"
      data-test="knowledge-tools"
      @keydown="onToolsKeydown"
    >
      <button
        type="button"
        class="tool"
        :class="{ 'tool--on': props.knowledge.enabled }"
        data-test="knowledge-toggle"
        :aria-pressed="String(props.knowledge.enabled)"
        :disabled="!props.canUseKnowledge"
        @click="toggleKnowledge"
      >
        <AppIcon name="knowledge" :size="14" />
        <span>使用知识库</span>
      </button>

      <button
        type="button"
        class="tool"
        data-test="knowledge-pick"
        :disabled="!props.knowledge.enabled"
        :aria-expanded="String(picking)"
        @click="picking = !picking"
      >
        <span>选择知识库</span>
        <span v-if="props.knowledge.enabled" class="tool__count">{{ selected.length }}</span>
      </button>

      <ul v-if="picking && props.collections.length > 0" class="tool__list">
        <li v-for="item in props.collections" :key="item.id">
          <label class="tool__option">
            <input
              type="checkbox"
              :checked="selected.includes(String(item.id))"
              @change="toggleCollection(item.id)"
            />
            <span>{{ item.title }}</span>
          </label>
        </li>
      </ul>

      <p v-if="!props.canUseKnowledge" class="composer__note">
        <button
          type="button"
          class="composer__note-link"
          data-test="knowledge-manage-link"
          @click="emit('open-knowledge')"
        >
          添加知识库后可启用
        </button>
      </p>
      <p
        v-else-if="props.knowledge.enabled && selected.length === 0"
        class="composer__note"
        data-test="knowledge-no-selection"
      >
        已开启，但还没选择知识库；本轮不会引用资料。
      </p>
    </div>

    <div class="composer__box">
      <textarea
        class="composer__input"
        rows="1"
        :value="props.modelValue"
        :placeholder="props.streaming ? '正在接收回答，可停止生成…' : props.placeholder"
        :disabled="props.disabled"
        aria-label="消息输入框"
        @input="update"
        @keydown="onKeydown"
      ></textarea>

      <div class="composer__actions">
        <span class="composer__hint">
          Enter 发送 · Shift + Enter 换行
        </span>
        <button
          v-if="props.streaming"
          type="button"
          class="gl-btn gl-btn--danger"
          data-test="abort"
          @click="emit('abort')"
        >
          <AppIcon name="stop" :size="15" />
          <span>停止</span>
        </button>
        <button
          v-else
          type="submit"
          class="gl-btn gl-btn--primary"
          data-test="send"
          :disabled="!sendable"
        >
          <AppIcon name="send" :size="15" />
          <span>发送</span>
        </button>
      </div>
    </div>
  </form>
</template>

<style scoped>
.composer {
  display: flex;
  flex-direction: column;
  gap: 0.35rem;
  padding: 0.75rem clamp(0.75rem, 3vw, 1.5rem) 1rem;
  background: var(--gl-surface);
  border-top: 1px solid var(--gl-surface-line);
}

.composer__error {
  margin: 0;
  font-size: 0.85rem;
  color: var(--gl-danger);
  font-weight: 600;
}

/* A tool strip, not a third navigation tier: low contrast, pill-shaped, and it
   inherits the composer's own padding rather than adding chrome above it. */
.composer__tools {
  position: relative;
  display: flex;
  align-items: center;
  gap: 0.45rem;
  flex-wrap: wrap;
}

.tool {
  display: inline-flex;
  align-items: center;
  gap: 0.35em;
  padding: 0.2em 0.6em;
  border: 1px solid var(--gl-surface-line);
  border-radius: 999px;
  background: var(--gl-surface-soft);
  color: var(--gl-ink-soft);
  font-size: 0.8rem;
  font-weight: 600;
}

.tool:hover:not(:disabled) {
  border-color: var(--gl-accent);
  color: var(--gl-accent-ink);
}

.tool:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}

.tool--on {
  background: var(--gl-accent-soft);
  border-color: #bfdcd7;
  color: var(--gl-accent-ink);
}

.tool__count {
  min-width: 1.6em;
  padding: 0 0.25em;
  border-radius: 999px;
  background: var(--gl-surface);
  color: var(--gl-accent-ink);
  font-size: 0.72rem;
  text-align: center;
}

.tool__list {
  position: absolute;
  bottom: calc(100% + 0.3rem);
  left: 0;
  z-index: 20;
  list-style: none;
  margin: 0;
  padding: 0.35rem;
  min-width: 220px;
  display: flex;
  flex-direction: column;
  gap: 0.1rem;
  background: var(--gl-surface);
  border: 1px solid var(--gl-surface-line);
  border-radius: var(--gl-radius-sm);
  box-shadow: var(--gl-shadow);
}

.tool__option {
  display: flex;
  align-items: center;
  gap: 0.5rem;
  padding: 0.3rem 0.4rem;
  border-radius: var(--gl-radius-sm);
  font-size: 0.86rem;
  cursor: pointer;
}

.tool__option:hover {
  background: var(--gl-surface-soft);
}

.composer__note {
  margin: 0;
  font-size: 0.78rem;
  color: var(--gl-ink-faint);
}

.composer__note-link {
  padding: 0;
  border: 0;
  background: none;
  font-size: inherit;
  color: var(--gl-accent-ink);
  text-decoration: underline;
  text-underline-offset: 2px;
}

.composer__box {
  display: flex;
  align-items: flex-end;
  gap: 0.6rem;
  padding: 0.5rem 0.6rem;
  border: 1px solid var(--gl-surface-line);
  border-radius: var(--gl-radius);
  background: var(--gl-surface-soft);
}

.composer__box:focus-within {
  border-color: var(--gl-accent);
  background: var(--gl-surface);
}

.composer__input {
  flex: 1;
  min-width: 0;
  max-height: 40vh;
  min-height: 2.6rem;
  padding: 0.35rem 0.2rem;
  border: 0;
  background: none;
  resize: vertical;
  line-height: 1.6;
  overflow-y: auto;
}

.composer__input:focus {
  outline: none;
}

.composer__actions {
  display: flex;
  align-items: center;
  gap: 0.6rem;
}

.composer__hint {
  font-size: 0.75rem;
  color: var(--gl-ink-faint);
}

@media (max-width: 700px) {
  .composer__hint {
    display: none;
  }
}
</style>
