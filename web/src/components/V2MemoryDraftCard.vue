<script setup>
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import AppIcon from './AppIcon.vue'
import ServerNotice from './ServerNotice.vue'
import { useV2MemoryDraft, v2DraftId, v2ConflictIds } from '../composables/useV2MemoryDraft.js'
import { draftActionText, memoryDurabilityText, memoryScopeText } from '../lib/displayText.js'
import { debugUiEnabled } from '../lib/debugUi.js'

/**
 * One memory draft, waiting for the learner to decide about it.
 *
 * The card is a question, not a report: here is a thing the agent would like to
 * remember, here is what it says, and here are the three answers — keep it,
 * replace an older one, or refuse it. Everything on it is in the learner's
 * words. The identities behind it (the draft id, the memory id, the stored
 * status, the whole response object) are facts about the plumbing, and they
 * are shown only when the debug switch is on.
 */
const props = defineProps({ draft: { type: Object, required: true }, runId: { type: String, default: '' } })
const card = useV2MemoryDraft(() => props.draft, () => props.runId), op = card.operation
const reason = ref(''), target = ref('')
const conflicts = computed(() => v2ConflictIds(props.draft))
const debug = computed(() => debugUiEnabled())
const topic = computed(() => props.draft.topicKey ?? props.draft.topic_key ?? '')
const tags = computed(() => (Array.isArray(props.draft.tags) ? props.draft.tags : []))
const validUntil = computed(() => props.draft.validUntil ?? props.draft.valid_until ?? '')
watch(() => JSON.stringify([v2DraftId(props.draft), props.runId]), () => { reason.value = ''; target.value = '' }, { flush: 'sync' })
onBeforeUnmount(card.dispose)
</script>
<template>
  <section class="gl-card v2-draft" data-test="memory-draft-card" data-memory-contract="v2" aria-label="记忆草稿">
    <h3><AppIcon name="memory" :size="15" /> 记忆草稿</h3>
    <p>{{ memoryScopeText(draft.scope) }}<span v-if="topic"> · {{ topic }}</span></p>
    <p v-if="draft.subject">{{ draft.subject }}</p><p data-test="memory-draft-content">{{ draft.content }}</p>
    <p v-if="draft.durability">{{ memoryDurabilityText(draft.durability) }}<span v-if="validUntil">，有效期到 {{ validUntil }}</span></p>
    <p v-if="tags.length">标签：{{ tags.join('、') }}</p>
    <p class="gl-muted">这条草稿还没有生效。只有你点下面的按钮确认之后，它才会被记住。</p>
    <label>填写原因（新增或替换时必填，最多 500 字）<input v-model="reason" data-test="v2-draft-reason" :disabled="op.busy.value"></label>
    <label v-if="conflicts.length">选择要替换的旧记忆（替换时必须明确选择）
      <select v-model="target" data-test="v2-draft-target" :disabled="op.busy.value"><option value="" disabled>请选择目标</option><option v-for="(id, index) in conflicts" :key="id" :value="id">原记忆 {{ index + 1 }}</option></select>
    </label>
    <div class="v2-actions">
      <button class="gl-btn" data-test="v2-draft-add" :disabled="op.busy.value || !reason.trim()" @click="card.decide('add', reason)">{{ draftActionText('add') }}</button>
      <button v-if="conflicts.length" class="gl-btn" data-test="v2-draft-replace" :disabled="op.busy.value || !reason.trim() || !conflicts.includes(target)" @click="card.decide('replace', reason, target)">{{ draftActionText('replace') }}</button>
      <button class="gl-btn" data-test="v2-draft-cancel" :disabled="op.busy.value" @click="card.decide('cancel', reason)">{{ draftActionText('cancel') }}</button>
      <button v-if="op.busy.value" class="gl-btn" data-test="v2-draft-abort" @click="card.cancel">取消当前请求</button>
      <button v-if="op.pending.value && !op.busy.value" class="gl-btn" data-test="v2-draft-retry" @click="card.retry">重试刚才的请求</button>
    </div>
    <ServerNotice v-if="op.error.value" :message="op.error.value.message" :code="op.error.value.code" />
    <div v-if="card.outcome.value" data-test="v2-draft-outcome">
      <p v-if="card.outcome.value.draft.status === 'confirmed' && card.outcome.value.memory?.status === 'active'" data-test="v2-active-confirmation">这条记忆已保存，之后回答时会想起它。</p>
      <p v-else>这条记忆还没有生效；以服务端的返回为准。</p>
      <details v-if="debug" data-test="v2-draft-debug">
        <summary>调试信息</summary>
        <p>draft.status={{ card.outcome.value.draft.status }} · replay={{ card.outcome.value.replay === true }}</p>
        <p v-if="card.outcome.value.memory" data-test="v2-draft-memory-state">memory.status={{ card.outcome.value.memory.status }} · {{ card.outcome.value.memory.id }}</p>
        <p v-if="card.outcome.value.memory">memory.durability={{ card.outcome.value.memory.durability }} · valid_until={{ card.outcome.value.memory.valid_until || '无' }}</p>
        <pre>{{ JSON.stringify(card.outcome.value, null, 2) }}</pre>
      </details>
    </div>
  </section>
</template>
<style scoped>
.v2-draft { margin-block: .75rem; padding: .8rem; border: 1px solid var(--gl-surface-line); border-radius: var(--gl-radius-sm); background: var(--gl-surface-soft); }
.v2-actions { display: flex; flex-wrap: wrap; gap: .4rem; margin-block: .5rem; } label { display: grid; gap: .25rem; } pre { white-space: pre-wrap; overflow-wrap: anywhere; }
</style>
