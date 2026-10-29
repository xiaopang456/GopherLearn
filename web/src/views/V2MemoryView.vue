<script setup>
import { onMounted, onBeforeUnmount, reactive, ref, watch } from 'vue'
import SectionFrame from '../components/SectionFrame.vue'
import ServerNotice from '../components/ServerNotice.vue'
import V2MemoryDraftCard from '../components/V2MemoryDraftCard.vue'
import { useV2MemoryPage } from '../composables/useV2MemoryPage.js'

const page = useV2MemoryPage(), op = page.operation
const form = reactive({ scope: '', subject: '', content: '', topic_key: '', durability: '', valid_until: '', tags: '', reason: '' })
const filter = reactive({ scope: '', status: 'active' })
const edit = ref(null), reason = ref(''), decision = ref(''), target = ref('')
watch(() => op.result.value, () => { decision.value = ''; target.value = '' })
function tags(value) { return value.trim() ? value.split(',').map((item) => item.trim()) : [] }
function body() {
  return { scope: form.scope, subject: form.subject, content: form.content, durability: form.durability, reason: form.reason,
    tags: tags(form.tags), ...(form.topic_key ? { topic_key: form.topic_key } : {}), ...(form.valid_until ? { valid_until: form.valid_until } : {}) }
}
function refresh() { return page.refresh({ ...(filter.scope ? { scope: filter.scope } : {}), status: filter.status }) }
async function perform(kind, payload, id) {
  if (op.busy.value) return
  for (const [field, limit] of [['subject', 200], ['content', 400], ['topic_key', 160], ['reason', 500]]) {
    const value = String(payload[field] ?? '')
    if (payload[field] !== undefined && Array.from(field === 'reason' ? value : value.trim()).length > limit) {
      op.error.value = { message: `${field} 超过原 ${limit} 字上限。`, code: 'invalid_form' }; return
    }
  }
  if (await op.start(kind, payload, id)) await refresh()
}
async function retry() { if (await op.retry()) await refresh() }
function conflictDecision() {
  if (!decision.value || (decision.value === 'replace' && !target.value)) return
  const snapshot = op.pending.value?.body
  if (!snapshot) return
  return perform('create', { ...snapshot, decision: decision.value, ...(decision.value === 'replace' ? { target_memory_id: target.value } : {}) })
}
function editMemory(memory) { edit.value = { id: memory.id, subject: memory.subject, content: memory.content, tags: (memory.tags || []).join(','), reason: '' } }
onMounted(refresh)
onBeforeUnmount(page.dispose)
</script>
<template>
  <div class="gl-page">
    <SectionFrame title="V2 记忆治理" eyebrow="原 native 合同" icon="memory" status="独立入口" status-tone="done"
      summary="原 global/topic 治理入口；旧 clean 手写表单与卡片合同仍未无损迁移。治理操作不授予主题读取权限。">
      <p class="gl-muted">记忆列表、pendingDrafts、revision 各最多 100 条，无分页，不代表完整历史。global 写入保留原模型判断与 MySQL 命名锁。</p>
      <ServerNotice v-if="op.error.value || page.readError.value" :message="(op.error.value || page.readError.value).message" :code="(op.error.value || page.readError.value).code" />
      <button v-if="op.busy.value" class="gl-btn" data-test="v2-cancel" @click="op.cancel">取消当前请求</button>
      <button v-if="op.pending.value && !op.busy.value" class="gl-btn" data-test="v2-retry" @click="retry">明确重试同一请求（原键与内容）</button>
      <p v-if="op.pending.value?.body.idempotency_key" data-test="operation-key">本次操作键：{{ op.pending.value.body.idempotency_key }}</p>
      <details v-if="op.result.value" open data-test="v2-operation-result"><summary>原实际返回（含 conflicts/replay/state）</summary><pre>{{ JSON.stringify(op.result.value, null, 2) }}</pre></details>
      <form data-test="v2-create-form" @submit.prevent="perform('create', body())">
        <fieldset :disabled="op.busy.value" class="v2-fields"><legend>显式创建；提交新操作生成独立操作键</legend>
          <label>scope <select v-model="form.scope" required data-test="scope"><option value="" disabled>请选择范围</option><option value="global">global</option><option value="topic">topic</option></select></label>
          <label>subject（最多 200 字）<input v-model="form.subject" required data-test="subject"></label>
          <label>content（最多 400 字）<textarea v-model="form.content" required data-test="content" /></label>
          <label>topic_key（topic 必填，global 禁止）<input v-model="form.topic_key" :required="form.scope === 'topic'" data-test="topic-key"></label>
          <label>durability <select v-model="form.durability" required data-test="durability"><option value="" disabled>请选择有效期类型</option><option value="stable">stable</option><option value="time_bound">time_bound</option></select></label>
          <label>valid_until（time_bound 必填；RFC3339 含时区，stable 禁止）<input v-model="form.valid_until" :required="form.durability === 'time_bound'" placeholder="2030-01-01T00:00:00+08:00" data-test="valid-until"></label>
          <label>tags（原词表，最多 5 项，英文逗号分隔，可留空）<input v-model="form.tags" data-test="tags"></label>
          <label>reason（最多 500 字）<input v-model="form.reason" required data-test="reason"></label>
          <button class="gl-btn gl-btn--primary" data-test="v2-create">提交新创建操作</button>
        </fieldset>
      </form>
      <fieldset v-if="op.result.value?.conflicts?.length" :disabled="op.busy.value" data-test="v2-conflicts"><legend>原公开冲突；未自动写入，需明确选择</legend>
        <pre>{{ JSON.stringify(op.result.value.conflicts, null, 2) }}</pre>
        <select v-model="decision" data-test="create-decision"><option value="" disabled>请选择操作</option><option value="add">add</option><option value="replace">replace</option></select>
        <select v-if="decision === 'replace'" v-model="target" data-test="create-target"><option value="" disabled>请选择原冲突目标</option><option v-for="item in op.result.value.conflicts" :key="item.id" :value="item.id">{{ item.subject }} · {{ item.id }} · {{ item.scope }}/{{ item.topic_key }}</option></select>
        <button class="gl-btn" :disabled="!decision || (decision === 'replace' && !target)" @click="conflictDecision">提交明确冲突决策（新操作键）</button>
      </fieldset>
      <label>列表 scope <select v-model="filter.scope"><option value="">全部原范围</option><option value="global">global</option><option value="topic">topic</option></select></label>
      <label>列表状态 <select v-model="filter.status"><option v-for="state in ['active','archived','superseded','deleted']" :key="state">{{ state }}</option></select></label>
      <button class="gl-btn" data-test="v2-refresh" @click="refresh">读取实际列表</button>
      <label>归档/删除 reason <input v-model="reason" data-test="manage-reason"></label>
      <ul><li v-for="memory in page.memories.value" :key="memory.id" class="gl-card" data-test="v2-memory-row">
        <h3>{{ memory.subject }}</h3><p>{{ memory.content }}</p>
        <p>{{ memory.id }} · {{ memory.scope }}/{{ memory.topic_key }} · {{ memory.status }} · user_confirmed={{ memory.user_confirmed }}</p>
        <p>durability={{ memory.durability }} · valid_until={{ memory.valid_until || '无' }} · {{ memory.source }}</p><p>tags={{ memory.tags }}</p>
        <details><summary>原返回的公开字段</summary><pre>{{ JSON.stringify(memory, null, 2) }}</pre></details>
        <button class="gl-btn" data-test="v2-revisions" @click="page.showRevisions(memory.id)">查看原 revision</button>
        <button v-if="memory.status === 'active'" class="gl-btn" :disabled="op.busy.value" data-test="v2-edit" @click="editMemory(memory)">修改</button>
        <button v-if="memory.status === 'active'" class="gl-btn" :disabled="op.busy.value || !reason.trim()" data-test="v2-archive" @click="perform('archive', { reason }, memory.id)">归档</button>
        <button class="gl-btn" :disabled="op.busy.value || !reason.trim()" data-test="v2-delete" @click="perform('delete', { reason }, memory.id)">删除</button>
      </li></ul>
      <form v-if="edit" data-test="v2-edit-form" @submit.prevent="perform('correct', { subject: edit.subject, content: edit.content, tags: tags(edit.tags), reason: edit.reason }, edit.id)">
        <fieldset :disabled="op.busy.value" class="v2-fields"><legend>修改 {{ edit.id }}；保留原范围与有效期</legend>
          <input v-model="edit.subject" required aria-label="修改 subject"><textarea v-model="edit.content" required aria-label="修改 content" />
          <input v-model="edit.tags" required aria-label="修改 tags"><input v-model="edit.reason" required aria-label="修改 reason"><button class="gl-btn" data-test="v2-correct">提交修改</button>
        </fieldset>
      </form>
      <details v-if="page.revisions.value" open data-test="v2-revision-result"><summary>{{ page.revisions.value.id }} revision（最多 100）</summary><pre>{{ JSON.stringify(page.revisions.value.items, null, 2) }}</pre></details>
      <h3>原 pendingDrafts（最多 100）</h3><pre data-test="v2-pending-drafts">{{ JSON.stringify(page.drafts.value, null, 2) }}</pre>
      <V2MemoryDraftCard v-for="draft in page.drafts.value" :key="draft.id" :draft="draft" />
      <p class="gl-muted">没有原 detail/草稿状态查询接口；不会从列表缺失推断确认成功。历史未返回 RunResult 时无法恢复 Agent 卡片。</p>
    </SectionFrame>
  </div>
</template>
<style scoped>
.v2-fields { display: grid; gap: .65rem; margin-block: 1rem; border: 1px solid var(--gl-surface-line); border-radius: var(--gl-radius-sm); padding: 1rem; }
label { display: grid; gap: .25rem; } pre { white-space: pre-wrap; overflow-wrap: anywhere; } .gl-card { margin-block: .6rem; }
</style>
