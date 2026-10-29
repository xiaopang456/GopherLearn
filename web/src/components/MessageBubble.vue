<script setup>
import { computed, ref } from 'vue'

import AppIcon from './AppIcon.vue'
import AgentResult from './AgentResult.vue'
import CitationList from './CitationList.vue'
import MemoryDraftCard from './MemoryDraftCard.vue'
import V2MemoryDraftCard from './V2MemoryDraftCard.vue'
import MessageContent from './MessageContent.vue'
import ServerNotice from './ServerNotice.vue'

const props = defineProps({
  message: {
    type: Object,
    required: true, // { id, role, content, status, error, sources, memoryDraft, processNotes }
  },
})

const ROLE_LABEL = {
  user: '我',
  assistant: 'GopherLearn',
  system: '系统',
}

const isUser = () => props.message.role === 'user'
const streaming = () => props.message.status === 'pending'
const label = () => ROLE_LABEL[props.message.role] ?? '消息'

// A turn that failed before any token arrived shows only the error card; an
// empty-but-successful answer still renders its "（空回复）" placeholder.
const showBody = computed(
  () =>
    !!props.message.content ||
    (props.message.status !== 'error' && props.message.status !== 'stopped'),
)

/**
 * The draft this reply's own turn stored, waiting to be decided. It is a fact about one
 * assistant message — the server sent it on that turn's run — so a card can never appear
 * under somebody else's question, and the reply keeps its meaning even when the answer
 * later failed: the row was written either way.
 */
const draftCard = computed(() =>
  props.message.role === 'assistant' && !v2DraftCard.value ? props.message.memoryDraft ?? null : null,
)
const v2DraftCard = computed(() => props.message.role === 'assistant'
  ? props.message.originalV2?.memoryDraft ?? (props.message.memoryContract === 'v2' ? props.message.memoryDraft : null)
  : null)

/**
 * What the model said on its way to a tool, once this reply has an answer to be separate
 * from. They are the model's own published sentences, so they stay readable — the fold is
 * about not presenting a note as the reply, not about hiding it. Nothing here is open by
 * default: a learner who wants the reasoning asks for it, and one who does not reads an
 * answer that is no longer buried under two paragraphs of preamble.
 */
const processNotes = computed(() =>
  props.message.role === 'assistant' ? props.message.processNotes ?? [] : [],
)
const notesOpen = ref(false)
</script>

<template>
  <article
    class="turn"
    :class="[`turn--${props.message.role}`, `turn--${props.message.status}`]"
    :data-test="`message-${props.message.id}`"
  >
    <header class="turn__meta">
      <AppIcon
        v-if="!isUser()"
        name="logo"
        :size="15"
      />
      <span class="turn__who">{{ label() }}</span>
      <span v-if="streaming()" class="turn__flag">正在生成</span>
      <span v-else-if="props.message.status === 'stopped'" class="turn__flag">
        已停止
      </span>
    </header>

    <div class="turn__body">
      <section
        v-if="processNotes.length"
        class="turn__notes"
        data-test="process-notes"
      >
        <button
          type="button"
          class="turn__notes-toggle"
          data-test="process-notes-toggle"
          :aria-expanded="notesOpen ? 'true' : 'false'"
          @click="notesOpen = !notesOpen"
        >
          {{ notesOpen ? '收起本轮的说明' : `查看本轮的 ${processNotes.length} 条说明` }}
        </button>
        <ol
          v-if="notesOpen"
          class="turn__note-list"
          data-test="process-note-list"
        >
          <li
            v-for="(note, index) in processNotes"
            :key="note.blockId"
            class="turn__note"
            :data-test="`process-note-${index + 1}`"
          >
            <MessageContent
              :text="note.text"
              :caret="false"
              :rich="true"
            />
          </li>
        </ol>
      </section>
      <!-- Answers are formatted; the learner's own words stay literal. -->
      <MessageContent
        v-if="showBody"
        :text="props.message.content"
        :caret="streaming()"
        :rich="!isUser()"
      />
      <CitationList :sources="props.message.sources ?? []" />
      <AgentResult v-if="props.message.originalV2" :result="props.message.originalV2" :run-id="props.message.runId" :message-id="props.message.serverId" />
      <V2MemoryDraftCard v-if="v2DraftCard" :key="v2DraftCard.draftId" :draft="v2DraftCard" :run-id="props.message.runId || ''" />
      <MemoryDraftCard
        v-if="draftCard"
        :draft="draftCard"
      />
      <ServerNotice
        v-if="props.message.error"
        class="turn__notice"
        :variant="props.message.status === 'stopped' ? 'stopped' : 'error'"
        :message="props.message.error.message"
        :code="props.message.error.code"
      />
    </div>
  </article>
</template>

<style scoped>
.turn {
  display: flex;
  flex-direction: column;
  gap: 0.25rem;
  padding: 0.7rem 0.9rem;
  border: 1px solid var(--gl-surface-line);
  border-radius: var(--gl-radius);
  background: var(--gl-surface);
}

.turn--user {
  align-self: flex-end;
  width: min(100%, 62ch);
  background: var(--gl-accent-soft);
  border-color: #bfdcd7;
}

.turn--assistant {
  align-self: stretch;
  width: min(100%, 82ch);
}

.turn--system {
  align-self: center;
  background: var(--gl-surface-soft);
  font-size: 0.88rem;
}

.turn--error {
  border-color: #e6c9c6;
  box-shadow: inset 3px 0 0 var(--gl-danger);
}

.turn--stopped {
  box-shadow: inset 3px 0 0 var(--gl-warn);
}

.turn__meta {
  display: flex;
  align-items: center;
  gap: 0.4rem;
  font-size: 0.78rem;
  font-weight: 700;
  letter-spacing: 0.03em;
  color: var(--gl-ink-faint);
}

.turn__flag {
  padding: 0.05em 0.5em;
  border-radius: 999px;
  background: var(--gl-surface-soft);
  border: 1px solid var(--gl-surface-line);
  color: var(--gl-accent-ink);
  font-weight: 600;
}

.turn__body {
  font-size: 1rem;
}

.turn__notes {
  margin-bottom: 0.5rem;
}

.turn__notes-toggle {
  padding: 0.15rem 0.55rem;
  border: 1px solid var(--gl-surface-line);
  border-radius: 999px;
  background: var(--gl-surface-soft);
  color: var(--gl-ink-soft);
  font-size: 0.82rem;
  font-weight: 600;
  cursor: pointer;
}

.turn__note-list {
  display: flex;
  flex-direction: column;
  gap: 0.4rem;
  margin: 0.5rem 0 0;
  padding-left: 1.35rem;
  border-left: 2px solid var(--gl-surface-line);
  color: var(--gl-ink-soft);
  font-size: 0.92rem;
}

.turn__notice {
  margin-top: 0.6rem;
}
</style>
