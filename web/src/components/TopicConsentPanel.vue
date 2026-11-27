<script setup>
/**
 * The read-side confirmation card for one pending question.
 *
 * It is a question about this turn only: which of the learner's own topics may
 * the answer read. Nothing personal has left the server to build this list -
 * it carries topic names only - and nothing is loaded until a ticket is
 * issued for the checked keys.
 *
 * The card lives in the transcript, under the question it is about and where
 * the answer will land. That is why its title says what it is deciding: the
 * learner is answering a question about *this* question, and everything in the
 * card - the candidates, the count, the search entry and the two buttons -
 * lines up on one left edge so the card reads as one decision rather than
 * four scattered controls.
 */
const props = defineProps({
  prompt: { type: Object, required: true },
})

const emit = defineEmits([
  'toggle',
  'open-catalogue',
  'search',
  'show-all',
  'load-more',
  'confirm',
  'skip',
  'update:query',
])

const isSelected = (key) => props.prompt.selected.includes(key)
</script>

<template>
  <section class="topic-consent gl-card" data-test="topic-consent">
    <div class="topic-consent__title" data-test="topic-consent-title">
      选择本轮使用的主题
    </div>
    <div class="topic-consent__count" data-test="topic-consent-count">
      已选择 {{ prompt.selected.length }}/{{ prompt.maxSelectable }}，仅本次回答使用
    </div>
    <ul class="topic-consent__options" data-test="topic-consent-options">
      <li
        v-for="candidate in prompt.candidates"
        :key="candidate.topicKey"
        :class="['topic-consent__option', { 'is-disabled': !isSelected(candidate.topicKey) && prompt.selected.length >= prompt.maxSelectable }]"
      >
        <label>
          <input
            type="checkbox"
            :checked="isSelected(candidate.topicKey)"
            :disabled="!isSelected(candidate.topicKey) && prompt.selected.length >= prompt.maxSelectable"
            :data-test="`topic-option-${candidate.topicKey}`"
            @change="emit('toggle', candidate.topicKey)"
          >
          <span>{{ candidate.displayName || candidate.topicKey }}</span>
        </label>
      </li>
    </ul>
    <div class="topic-consent__catalogue">
      <button
        v-if="!prompt.catalogueOpened"
        type="button"
        class="topic-consent__link"
        data-test="topic-open-catalogue"
        @click="emit('open-catalogue')"
      >
        找不到？搜索或查看全部主题
      </button>
      <template v-else>
        <div class="topic-consent__search">
          <input
            :value="prompt.catalogueQuery"
            type="search"
            placeholder="按主题名称搜索，例如 Docker"
            :disabled="prompt.catalogueBusy"
            data-test="topic-catalogue-query"
            @input="emit('update:query', $event.target.value)"
            @keydown.enter.prevent="emit('search')"
          >
          <button type="button" :disabled="prompt.catalogueBusy" data-test="topic-catalogue-search" @click="emit('search')">搜索</button>
          <button type="button" :disabled="prompt.catalogueBusy" data-test="topic-catalogue-all" @click="emit('show-all')">查看全部</button>
        </div>
        <button
          v-if="prompt.catalogueHasMore"
          type="button"
          class="topic-consent__link"
          :disabled="prompt.catalogueBusy"
          data-test="topic-catalogue-more"
          @click="emit('load-more')"
        >
          加载更多主题
        </button>
      </template>
    </div>
    <div class="topic-consent__actions">
      <button
        type="button"
        class="gl-btn topic-consent__primary"
        :disabled="prompt.selected.length === 0 || prompt.catalogueBusy || prompt.confirmBusy"
        data-test="topic-confirm"
        @click="emit('confirm')"
      >
        使用所选记录
      </button>
      <button type="button" class="gl-btn" data-test="topic-skip" @click="emit('skip')">本次不使用</button>
    </div>
  </section>
</template>

<style scoped>
/* The card is a step in the conversation, not a strip above it: it takes the
   width of the transcript column and shares its left edge with the answer
   that will arrive under it. */
.topic-consent {
  margin: 0;
  padding: var(--gl-space-3) var(--gl-space-4);
  border-color: var(--gl-surface-line);
  background: var(--gl-surface-soft);
  text-align: left;
}
.topic-consent__title {
  font-weight: 600;
  color: var(--gl-ink);
}
.topic-consent__count {
  margin-top: 2px;
  color: var(--gl-ink-soft);
  font-size: var(--gl-text-sm);
}
/* One candidate per line, all starting at the same left edge: the learner is
   comparing subjects, and a column is the shape a comparison reads in. */
.topic-consent__options {
  margin: var(--gl-space-2) 0 0;
  padding: 0;
  list-style: none;
}
.topic-consent__option {
  padding: 3px 0;
}
.topic-consent__option label {
  display: flex;
  align-items: center;
  gap: 0.5em;
  cursor: pointer;
}
.topic-consent__option.is-disabled {
  opacity: 0.55;
  cursor: not-allowed;
}
.topic-consent__catalogue {
  margin-top: var(--gl-space-2);
}
.topic-consent__link {
  padding: 0;
  border: 0;
  background: none;
  color: var(--gl-accent-ink);
  cursor: pointer;
  font-size: var(--gl-text-sm);
  text-decoration: underline;
}
.topic-consent__search {
  display: flex;
  gap: var(--gl-space-2);
  margin-bottom: var(--gl-space-2);
}
.topic-consent__search input {
  flex: 1;
  min-width: 0;
}
.topic-consent__actions {
  display: flex;
  gap: var(--gl-space-2);
  margin-top: var(--gl-space-3);
}
</style>
