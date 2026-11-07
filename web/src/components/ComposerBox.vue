<script setup>
import { computed, ref } from 'vue'
import { NSwitch } from 'naive-ui'

import AppIcon from './AppIcon.vue'
import { isKnownMode, SessionMode } from '../lib/modes.js'
import { canSend, validateMessageContent } from '../lib/validation.js'

const props = defineProps({
  modelValue: { type: String, default: '' },
  streaming: { type: Boolean, default: false },
  thinking: { type: Boolean, default: false },
  disabled: { type: Boolean, default: false },
  placeholder: { type: String, default: '输入你的问题…' },
  /** The server-owned mode of the open session; '' while still unknown. */
  mode: { type: String, default: '' },
  /** Whether the learner wants this turn to reach their own documents. */
  knowledgeEnabled: { type: Boolean, default: false },
  /** The server's ceiling for this session; the switch is not offered without it. */
  canUseKnowledge: { type: Boolean, default: false },
})

const emit = defineEmits([
  'update:modelValue',
  'update:thinking',
  'update:knowledgeEnabled',
  'submit',
  'abort',
])

const localError = ref('')

const sendable = computed(
  () => canSend(props.modelValue) && !props.disabled && !props.streaming,
)

// The server states the ceiling per session, but the composer still needs a
// recognised mode to render a control for it. An unrecognised mode gets no
// optional capability rather than a guess in either direction.
const showsKnowledge = computed(
  () => props.canUseKnowledge && isKnownMode(props.mode),
)

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
  emit('update:knowledgeEnabled', !props.knowledgeEnabled)
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
        <div v-if="props.mode === SessionMode.LearningAgent" class="composer__thinking">
          <n-switch :value="props.thinking" size="small" aria-label="深度思考" data-test="thinking-toggle" @update:value="emit('update:thinking', $event)" />
          <span>深度思考</span><span class="composer__thinking-hint">回答更仔细，但会慢一些</span>
        </div>
        <div
          v-if="showsKnowledge"
          class="composer__tools"
          data-test="knowledge-tools"
        >
          <button
            type="button"
            class="tool"
            :class="{ 'tool--on': props.knowledgeEnabled }"
            data-test="knowledge-toggle"
            :aria-pressed="String(props.knowledgeEnabled)"
            @click="toggleKnowledge"
          >
            <AppIcon name="knowledge" :size="14" />
            <span>使用知识库</span>
          </button>
          <span class="composer__note">
            {{ props.knowledgeEnabled ? '本轮可检索你上传的资料。' : '本轮不检索你的资料。' }}
          </span>
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

.tool:hover {
  background: var(--gl-surface-soft);
  color: var(--gl-ink);
}

.tool--on {
  background: var(--gl-accent-soft);
  color: var(--gl-accent-ink);
}

.composer__note {
  margin: 0;
  padding-left: var(--gl-space-2);
  font-size: var(--gl-text-xs);
  color: var(--gl-ink-faint);
}

.composer__thinking { display: flex; align-items: center; gap: 6px; flex-wrap: wrap; font-size: var(--gl-text-sm); color: var(--gl-ink-soft); }
.composer__thinking-hint { font-size: var(--gl-text-xs); color: var(--gl-ink-faint); }
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
