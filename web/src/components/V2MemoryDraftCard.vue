<script setup>
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import AppIcon from './AppIcon.vue'
import ServerNotice from './ServerNotice.vue'
import { useV2MemoryDraft, v2DraftId, v2ConflictIds } from '../composables/useV2MemoryDraft.js'
const props = defineProps({ draft: { type: Object, required: true }, runId: { type: String, default: '' } })
const card = useV2MemoryDraft(() => props.draft, () => props.runId), op = card.operation
const reason = ref(''), target = ref('')
const conflicts = computed(() => v2ConflictIds(props.draft))
watch(() => JSON.stringify([v2DraftId(props.draft), props.runId]), () => { reason.value = ''; target.value = '' }, { flush: 'sync' })
onBeforeUnmount(card.dispose)
</script>
<template>
  <section class="gl-card v2-draft" data-test="memory-draft-card" data-memory-contract="v2" aria-label="原 V2 记忆草稿">
    <h3><AppIcon name="memory" :size="15" /> V2 记忆草稿</h3>
    <p>{{ draft.scope }} · {{ draft.topicKey ?? draft.topic_key }}</p>
    <p v-if="draft.subject">{{ draft.subject }}</p><p data-test="memory-draft-content">{{ draft.content }}</p>
    <p v-if="draft.durability">durability={{ draft.durability }} · valid_until={{ draft.validUntil ?? draft.valid_until ?? '无' }}</p>
    <p v-if="draft.tags">tags={{ draft.tags }}</p>
    <p class="gl-muted">这是原返回的草稿身份，尚未据此确认生效；旧卡片仍由原接口决定，历史缺少 RunResult 时不猜造卡片。</p>
    <label>决定 reason（add/replace 必填，最多 500 字）<input v-model="reason" data-test="v2-draft-reason" :disabled="op.busy.value"></label>
    <label v-if="conflicts.length">原冲突目标（replace 须明确选择）
      <select v-model="target" data-test="v2-draft-target" :disabled="op.busy.value"><option value="" disabled>请选择目标</option><option v-for="id in conflicts" :key="id" :value="id">{{ id }}</option></select>
    </label>
    <div class="v2-actions">
      <button class="gl-btn" data-test="v2-draft-add" :disabled="op.busy.value || !reason.trim()" @click="card.decide('add', reason)">add：明确确认新增</button>
      <button v-if="conflicts.length" class="gl-btn" data-test="v2-draft-replace" :disabled="op.busy.value || !reason.trim() || !conflicts.includes(target)" @click="card.decide('replace', reason, target)">replace：确认选定目标</button>
      <button class="gl-btn" data-test="v2-draft-cancel" :disabled="op.busy.value" @click="card.decide('cancel', reason)">cancel：明确拒绝草稿</button>
      <button v-if="op.busy.value" class="gl-btn" data-test="v2-draft-abort" @click="card.cancel">取消当前 HTTP 请求</button>
      <button v-if="op.pending.value && !op.busy.value" class="gl-btn" data-test="v2-draft-retry" @click="card.retry">明确重试原请求</button>
    </div>
    <ServerNotice v-if="op.error.value" :message="op.error.value.message" :code="op.error.value.code" />
    <div v-if="card.outcome.value" data-test="v2-draft-outcome">
      <p>实际 draft.status={{ card.outcome.value.draft.status }} · replay={{ card.outcome.value.replay === true }}</p>
      <p v-if="card.outcome.value.memory" data-test="v2-draft-memory-state">实际 memory.status={{ card.outcome.value.memory.status }} · {{ card.outcome.value.memory.id }}</p>
      <p v-if="card.outcome.value.memory">实际 memory.durability={{ card.outcome.value.memory.durability }} · valid_until={{ card.outcome.value.memory.valid_until || '无' }}</p>
      <p v-if="card.outcome.value.draft.status === 'confirmed' && card.outcome.value.memory?.status === 'active'" data-test="v2-active-confirmation">原返回已确认，记忆存储状态为 active；有效期和召回仍由原链决定。</p>
      <p v-else>未声明当前生效；以原返回状态为准。</p>
      <details><summary>原 draft/memory/revision/replay</summary><pre>{{ JSON.stringify(card.outcome.value, null, 2) }}</pre></details>
    </div>
  </section>
</template>
<style scoped>
.v2-draft { margin-block: .75rem; padding: .8rem; border: 1px solid var(--gl-surface-line); border-radius: var(--gl-radius-sm); background: var(--gl-surface-soft); }
.v2-actions { display: flex; flex-wrap: wrap; gap: .4rem; margin-block: .5rem; } label { display: grid; gap: .25rem; } pre { white-space: pre-wrap; overflow-wrap: anywhere; }
</style>
