<script setup>
import { computed } from 'vue'

import MarkdownView from './MarkdownView.vue'
import { markdownTree } from '../lib/markdown.js'

/**
 * One answer, rendered the same way wherever it appears.
 *
 * Model text is untrusted, so it is never a markup string: `markdown.js` turns it
 * into a whitelist node tree and `MarkdownView` turns that into elements, which is
 * how "render the formatting, refuse the HTML" is possible without a raw markup
 * sink or a sanitiser pass. Raw tags arrive as the literal characters the model
 * wrote, and a `javascript:` link arrives as its label.
 *
 * The tree is recomputed from the whole accumulated text on every delta. That is
 * deliberate: markdown has no incremental parse that stays correct (a `**` opened
 * three chunks ago changes what the earlier characters *mean*), and re-lexing a
 * few kilobytes is cheaper than a second, divergent rendering path. Partial
 * markup is therefore always rendered from the current prefix — `### 标` is a
 * heading while it is still `### 标`, and the finished line replaces it without
 * the page clearing in between.
 *
 * `rich` is off for the learner's own words: what they typed is literal, and
 * echoing it back through a formatter would be a second chance to misread it.
 */
const props = defineProps({
  text: { type: String, default: '' },
  caret: { type: Boolean, default: false },
  rich: { type: Boolean, default: true },
})

const blocks = computed(() => (props.rich ? markdownTree(props.text) : []))
/** Whitespace is not an answer: a reply of only newlines reads as an empty one. */
const hasText = computed(() => props.text.trim() !== '')
const showRich = computed(() => props.rich && blocks.value.length > 0)
</script>

<template>
  <div class="content" :data-caret="props.caret ? 'on' : 'off'">
    <div v-if="showRich" class="content__rich md" data-test="markdown-body">
      <MarkdownView :nodes="blocks" :caret="props.caret" />
    </div>
    <span v-else-if="hasText" class="content__text">{{ props.text }}</span>
    <span v-if="props.caret && !showRich" class="content__caret" aria-hidden="true"></span>
    <span v-if="!hasText && !props.caret" class="content__empty">
      （空回复）
    </span>
  </div>
</template>

<style scoped>
.content {
  display: block;
}

.content__text {
  display: block;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
  word-break: normal;
  tab-size: 2;
}

.content__empty {
  color: var(--gl-ink-faint);
  font-style: italic;
}

/* `.content__caret` is styled in styles/markdown.css: the same cursor is drawn by
   MarkdownView inside the last block when an answer is formatted, and scoped rules
   would not reach that one. */
</style>
