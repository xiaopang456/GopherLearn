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
    <header v-if="!isUser()" class="turn__meta">
      <span class="turn__glyph">
        <AppIcon name="logo" :size="13" />
      </span>
      <span class="turn__who">{{ label() }}</span>
      <span v-if="streaming()" class="turn__flag">正在生成</span>
      <span v-else-if="props.message.status === 'stopped'" class="turn__flag">
        已停止
      </span>
    </header>

    <footer v-if="isUser()" class="turn__meta turn__meta--user">
      <span class="turn__who">{{ label() }}</span>
    </footer>

    <div class="turn__body" :class="{ 'turn__body--bubble': isUser() }">
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
/* An answer is text on a page, not a card inside a card: it reads as running
   prose, and only the learner's own turn is a filled block. */
.turn {
  display: flex;
  flex-direction: column;
  gap: var(--gl-space-2);
}

.turn--assistant {
  align-self: stretch;
}

.turn--user {
  align-self: flex-end;
  max-width: 78%;
}

.turn__body--bubble {
  padding: var(--gl-space-3) var(--gl-space-4);
  border-radius: 14px;
  background: var(--gl-surface-soft);
}

.turn--system {
  align-self: center;
  padding: var(--gl-space-2) var(--gl-space-3);
  border-radius: var(--gl-radius-sm);
  background: var(--gl-surface-soft);
  font-size: var(--gl-text-sm);
}

.turn--error {
  padding: var(--gl-space-3) var(--gl-space-4);
  border: 1px solid #f3d4d1;
  border-radius: var(--gl-radius);
  background: #fffbfb;
}

.turn--stopped {
  padding: var(--gl-space-3) var(--gl-space-4);
  border-radius: var(--gl-radius);
  background: var(--gl-surface-soft);
}

.turn__meta {
  display: flex;
  align-items: center;
  gap: var(--gl-space-2);
  font-size: var(--gl-text-sm);
  font-weight: 600;
  color: var(--gl-ink);
}

.turn__meta--user {
  justify-content: flex-end;
  font-size: var(--gl-text-xs);
  font-weight: 500;
  color: var(--gl-ink-faint);
}

.turn__glyph {
  display: flex;
  width: 22px;
  height: 22px;
  flex: none;
  align-items: center;
  justify-content: center;
  border-radius: 50%;
  background: var(--gl-accent-soft);
  color: var(--gl-accent-ink);
}

.turn__who {
  letter-spacing: 0.01em;
}

.turn__flag {
  padding: 0.1em 0.5em;
  border-radius: var(--gl-radius-pill);
  background: var(--gl-surface-soft);
  border: 1px solid var(--gl-surface-line);
  color: var(--gl-ink-soft);
  font-size: var(--gl-text-xs);
  font-weight: 500;
}

.turn__body {
  font-size: var(--gl-text-md);
  line-height: var(--gl-leading-relaxed);
}

.turn--user .turn__body {
  font-size: var(--gl-text-base);
  line-height: var(--gl-leading-normal);
  color: var(--gl-ink);
}

.turn__notes {
  margin-bottom: var(--gl-space-3);
}

.turn__notes-toggle {
  padding: 3px var(--gl-space-2);
  border: 1px solid var(--gl-surface-line);
  border-radius: var(--gl-radius-pill);
  background: var(--gl-surface);
  color: var(--gl-ink-soft);
  font-size: var(--gl-text-sm);
  font-weight: 500;
}

.turn__notes-toggle:hover {
  border-color: var(--gl-line-strong);
  color: var(--gl-ink);
}

.turn__note-list {
  display: flex;
  flex-direction: column;
  gap: var(--gl-space-2);
  margin: var(--gl-space-3) 0 0;
  padding-left: var(--gl-space-4);
  border-left: 2px solid var(--gl-surface-line);
  color: var(--gl-ink-soft);
  font-size: var(--gl-text-base);
}

.turn__notice {
  margin-top: var(--gl-space-3);
}
</style>
