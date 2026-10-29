<script setup>
import { computed, ref } from 'vue'

import AppIcon from './AppIcon.vue'
import ServerNotice from './ServerNotice.vue'
import { confirmDraft as apiConfirmDraft, memoryError, rejectDraft as apiRejectDraft, scopeLabel } from '../api/memory.js'
import { reportIfRejected } from '../api/http.js'

/**
 * The decision a `waiting_confirmation` frame asked for, placed where it was asked: under
 * the reply whose tool call wrote the row.
 *
 * Three rules hold it together.
 *
 * It renders the projection the server sent on this turn's own run — never a draft looked
 * up afterwards. "The newest pending draft" is a different conversation's question the
 * moment two turns are in flight, and a button on it would decide something this reply
 * never proposed.
 *
 * One click costs one request. The guard is set *before* the call is made and, once the
 * row has been decided, the buttons do not come back: a draft has one answer, and a second
 * `confirm` on the wire would only be a request for a 409.
 *
 * The id is used and never shown. It is the address of a stored row, which is nothing a
 * learner can check and nothing they need in order to decide.
 *
 * The memory page keeps its full list, its provenance and its bulk management: this card is
 * a shortcut to the one decision this turn produced, not a second view over all of them.
 */
const props = defineProps({
  draft: { type: Object, required: true }, // { draftId, scope, topicKey, content }
})

const Decided = { None: '', Confirmed: 'confirmed', Rejected: 'rejected' }

const busy = ref(false)
const decided = ref(Decided.None)
const failure = ref(null)

const label = computed(() => scopeLabel(props.draft?.scope))
const open = computed(() => decided.value === Decided.None)
const actionable = computed(() => open.value && !busy.value)
const busyAction = ref('')
const resultText = computed(() => {
  if (decided.value === Decided.Confirmed) return '已确认并生效'
  if (decided.value === Decided.Rejected) return '已拒绝'
  return ''
})

/**
 * Send one transition for one draft, once.
 *
 * `action` is compared against the two names this component's own buttons use, so a click
 * bound to anything else cannot reach the wire — there is no third transition here, and in
 * particular no archive, which is the memory page's decision about a row it can see the
 * history of.
 */
async function decide(action) {
  const operation = action === 'confirm' ? apiConfirmDraft : action === 'reject' ? apiRejectDraft : null
  const id = String(props.draft?.draftId ?? '')
  if (!operation || id === '' || busy.value || !open.value) return
  busy.value = true
  busyAction.value = action
  failure.value = null
  try {
    await operation(id)
    // The card only changes its own story once the server has agreed to it. Nothing is
    // optimistically marked decided: a `confirm` that came back 409 has to leave the
    // buttons in place, because the row is still waiting somewhere.
    decided.value = action === 'confirm' ? Decided.Confirmed : Decided.Rejected
  } catch (error) {
    // A rejected bearer is a session fact, and the funnel that signs the user out is the
    // same one every other memory write reports it to.
    reportIfRejected(error)
    failure.value = memoryError(error)
  } finally {
    busy.value = false
    busyAction.value = ''
  }
}

function busyText(action) {
  if (!busy.value || busyAction.value !== action) return ''
  return action === 'confirm' ? '确认中…' : '处理中…'
}
</script>

<template>
  <section class="draft" data-test="memory-draft-card" aria-label="待确认的记忆草稿">
    <header class="draft__head">
      <AppIcon name="memory" :size="15" />
      <span class="draft__scope" data-test="memory-draft-scope">{{ label }}</span>
      <span class="draft__state">{{ open ? '待确认' : '已处理' }}</span>
    </header>

    <p v-if="props.draft?.topicKey" class="draft__topic" data-test="memory-draft-topic">
      主题：{{ props.draft.topicKey }}
    </p>

    <p class="draft__content" data-test="memory-draft-content">{{ props.draft?.content }}</p>

    <div v-if="open" class="draft__actions">
      <button
        type="button"
        class="gl-btn gl-btn--primary"
        data-test="memory-draft-confirm"
        :disabled="!actionable"
        :aria-label="`确认并生效这条${label}草稿`"
        @click="decide('confirm')"
      >
        {{ busyText('confirm') || '确认并生效' }}
      </button>
      <button
        type="button"
        class="gl-btn"
        data-test="memory-draft-reject"
        :disabled="!actionable"
        :aria-label="`拒绝这条${label}草稿`"
        @click="decide('reject')"
      >
        {{ busyText('reject') || '拒绝' }}
      </button>
    </div>

    <p v-else class="draft__result" data-test="memory-draft-result" role="status">
      {{ resultText }}
    </p>

    <ServerNotice
      v-if="failure"
      class="draft__notice"
      :message="failure.copy"
      :code="failure.code"
    />
  </section>
</template>

<style scoped>
/* It sits inside the assistant turn, so it borrows the turn's own surface and only marks
   itself off with a rule: a second card here would read as a second message. */
.draft {
  display: flex;
  flex-direction: column;
  gap: 0.35rem;
  margin-top: 0.6rem;
  padding: 0.55rem 0.7rem;
  border: 1px dashed #bfdcd7;
  border-radius: var(--gl-radius-sm);
  background: var(--gl-surface-soft);
  font-size: 0.9rem;
  color: var(--gl-ink-soft);
}

.draft__head {
  display: flex;
  align-items: center;
  gap: 0.35rem;
  font-size: 0.78rem;
}

.draft__scope {
  font-weight: 700;
  color: var(--gl-ink);
}

.draft__state {
  margin-left: auto;
  padding: 0.05em 0.5em;
  border: 1px solid var(--gl-surface-line);
  border-radius: 999px;
  background: var(--gl-surface);
  color: var(--gl-ink-faint);
  white-space: nowrap;
}

.draft__topic {
  margin: 0;
  font-size: 0.78rem;
  color: var(--gl-ink-faint);
  overflow-wrap: anywhere;
}

.draft__content {
  margin: 0;
  color: var(--gl-ink);
  overflow-wrap: anywhere;
  white-space: pre-wrap;
}

.draft__actions {
  display: flex;
  flex-wrap: wrap;
  gap: 0.4rem;
}

.draft__result {
  margin: 0;
  font-weight: 600;
  color: var(--gl-accent-ink);
}

.draft__notice {
  margin-top: 0.15rem;
}
</style>
