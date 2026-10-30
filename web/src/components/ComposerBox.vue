<script setup>
import { computed, ref } from 'vue'

import AppIcon from './AppIcon.vue'
import { SessionMode } from '../lib/modes.js'
import { canSend, validateMessageContent } from '../lib/validation.js'

const props = defineProps({
  modelValue: { type: String, default: '' },
  streaming: { type: Boolean, default: false },
  disabled: { type: Boolean, default: false },
  placeholder: { type: String, default: '输入你的问题…' },
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

        <div class="composer__right">
          <span class="composer__hint">Enter 发送 · Shift + Enter 换行</span>
          <button
            v-if="props.streaming"
            type="button"
            class="composer__send composer__send--stop"
            data-test="abort"
            title="停止生成"
            @click="emit('abort')"
          >
            <AppIcon name="stop" :size="14" />
          </button>
          <button
            v-else
            type="submit"
            class="composer__send"
            data-test="send"
            :disabled="!sendable"
            title="发送"
          >
            <AppIcon name="send" :size="15" />
          </button>
        </div>
      </div>
    </div>
  </form>
</template>

<style scoped>
.composer {
  padding: 0 var(--gl-space-6) var(--gl-space-5);
  background: var(--gl-paper);
}

.composer__error {
  margin: 0 0 var(--gl-space-2);
  font-size: var(--gl-text-sm);
  color: var(--gl-danger);
  font-weight: 600;
}

.composer__box {
  max-width: 760px;
  margin: 0 auto;
  border: 1px solid var(--gl-surface-line);
  border-radius: 14px;
  background: var(--gl-surface);
  transition: border-color 120ms ease, box-shadow 120ms ease;
}

.composer__box:focus-within {
  border-color: var(--gl-accent);
  box-shadow: var(--gl-focus-ring);
}

.composer__input {
  display: block;
  width: 100%;
  max-height: 40vh;
  min-height: 2.5rem;
  padding: var(--gl-space-3) var(--gl-space-4) var(--gl-space-2);
  border: 0;
  background: none;
  resize: none;
  font-size: var(--gl-text-md);
  line-height: var(--gl-leading-normal);
  color: var(--gl-ink);
}

.composer__input:focus {
  outline: none;
}

.composer__input::placeholder {
  color: var(--gl-ink-faint);
}

.composer__actions {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--gl-space-3);
  padding: var(--gl-space-2) var(--gl-space-2) var(--gl-space-2) var(--gl-space-3);
  border-top: 1px solid var(--gl-surface-line);
}

/* A tool strip, not a third navigation tier: flat, quiet, and it inherits the
   composer's own padding rather than adding chrome above it. */
.composer__tools {
  position: relative;
  display: flex;
  align-items: center;
  gap: var(--gl-space-1);
  flex-wrap: wrap;
  min-width: 0;
}

.tool {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  padding: 5px var(--gl-space-2);
  border: 0;
  border-radius: var(--gl-radius-xs);
  background: none;
  color: var(--gl-ink-soft);
  font-size: var(--gl-text-sm);
  font-weight: 500;
  transition: background-color 120ms ease, color 120ms ease;
}

.tool:hover:not(:disabled) {
  background: var(--gl-surface-soft);
  color: var(--gl-ink);
}

.tool:disabled {
  opacity: 0.45;
  cursor: not-allowed;
}

.tool--on {
  background: var(--gl-accent-soft);
  color: var(--gl-accent-ink);
}

.tool__count {
  min-width: 1.5em;
  padding: 0 0.35em;
  border-radius: var(--gl-radius-pill);
  background: var(--gl-surface);
  color: var(--gl-accent-ink);
  font-size: var(--gl-text-xs);
  text-align: center;
}

.tool__list {
  position: absolute;
  bottom: calc(100% + 0.4rem);
  left: 0;
  z-index: 20;
  list-style: none;
  margin: 0;
  padding: var(--gl-space-1);
  min-width: 220px;
  display: flex;
  flex-direction: column;
  gap: 2px;
  background: var(--gl-surface);
  border: 1px solid var(--gl-surface-line);
  border-radius: var(--gl-radius-sm);
  box-shadow: var(--gl-shadow-2);
}

.tool__option {
  display: flex;
  align-items: center;
  gap: var(--gl-space-2);
  padding: 6px var(--gl-space-2);
  border-radius: var(--gl-radius-xs);
  font-size: var(--gl-text-sm);
  cursor: pointer;
}

.tool__option:hover {
  background: var(--gl-surface-soft);
}

.composer__note {
  margin: 0;
  padding-left: var(--gl-space-2);
  font-size: var(--gl-text-xs);
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

.composer__right {
  display: flex;
  flex: none;
  align-items: center;
  gap: var(--gl-space-3);
}

.composer__hint {
  font-size: var(--gl-text-xs);
  color: var(--gl-ink-faint);
}

.composer__send {
  display: flex;
  width: 32px;
  height: 32px;
  align-items: center;
  justify-content: center;
  padding: 0;
  border: 0;
  border-radius: 50%;
  background: var(--gl-ink);
  color: #fff;
  transition: background-color 120ms ease, opacity 120ms ease;
}

.composer__send:hover:not(:disabled) {
  background: #000;
}

.composer__send:disabled {
  background: var(--gl-surface-soft);
  color: var(--gl-ink-faint);
  cursor: not-allowed;
}

.composer__send--stop {
  background: var(--gl-danger);
}

.composer__send--stop:hover {
  background: #9c211a;
}

@media (max-width: 700px) {
  .composer {
    padding: 0 var(--gl-space-3) var(--gl-space-3);
  }

  .composer__hint {
    display: none;
  }
}
</style>
