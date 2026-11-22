<script setup>
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import AppIcon from './AppIcon.vue'
import ServerNotice from './ServerNotice.vue'
import { useV2MemoryDraft, v2DraftId, v2ConflictIds } from '../composables/useV2MemoryDraft.js'
import { listV2MemoryTopics } from '../api/v2Memory.js'
import { draftActionText, memoryDurabilityText, memoryScopeText } from '../lib/displayText.js'
import { debugUiEnabled } from '../lib/debugUi.js'

/**
 * One memory draft, waiting for the learner to decide about it.
 *
 * The card is a question, not a report: here is a thing the agent would like to
 * remember, where it came from, which existing entries it would touch, and the
 * three answers — keep it, replace an older one, or refuse it. When the server
 * answers that the replace target moved (409, target modified), the card shows
 * the latest content and the learner re-confirms against it; the server
 * re-verifies that hash inside the write transaction. A duplicate judgement on
 * the edit path ends the draft without asking the learner to abandon anything.
 * The identities behind it (the draft id, the memory id, the stored status, the
 * whole response object) are facts about the plumbing, and they are shown only
 * when the debug switch is on.
 */
const props = defineProps({ draft: { type: Object, required: true }, runId: { type: String, default: '' } })
const emit = defineEmits(['changed'])
const card = useV2MemoryDraft(() => props.draft, () => props.runId), op = card.operation
const reason = ref(''), target = ref('')
const conflicts = computed(() => v2ConflictIds(props.draft))
const debug = computed(() => debugUiEnabled())
const topic = computed(() => props.draft.topicKey ?? props.draft.topic_key ?? '')
const tags = computed(() => (Array.isArray(props.draft.tags) ? props.draft.tags : []))
const validUntil = computed(() => props.draft.validUntil ?? props.draft.valid_until ?? '')
const conflictTargets = computed(() => {
  const live = targetModified.value ? (op.error.value?.payload?.conflicts ?? []) : []
  const stored = props.draft.conflictTargets ?? props.draft.conflict_targets ?? []
  const byId = new Map(stored.map((item) => [item.id, item]))
  for (const item of live) byId.set(item.id, item)
  return conflicts.value.map((id) => byId.get(id)).filter(Boolean)
})
const targetLabel = (id) => {
  const item = conflictTargets.value.find((entry) => entry.id === id)
  return item?.subject ? item.subject : `原记忆 ${conflicts.value.indexOf(id) + 1}`
}
watch(() => JSON.stringify([v2DraftId(props.draft), props.runId]), () => { reason.value = ''; target.value = ''; resetEdit() }, { flush: 'sync' })
onBeforeUnmount(card.dispose)

// 409 target-modified: the server returned the latest row; the learner looks
// at it, then re-confirms with its hash so the write transaction can verify
// the version they saw.
const targetModified = computed(() => op.error.value?.nativeCode === 9007)
const latestConflicts = computed(() => (targetModified.value ? (op.error.value?.payload?.conflicts ?? []) : []))
const latestHash = computed(() => latestConflicts.value.find((item) => item.id === target.value)?.content_hash ?? '')

// Attribution: a topic draft may wait for its topic; the learner picks one
// from the memory catalogue (graded topics and existing note keys) or types a
// new key. Confirming an unassigned draft is refused by the server. The
// catalogue loads lazily - only for an unassigned card, or when the edit form
// needs the list - so a plain card makes no extra request.
const pendingTopic = computed(() => props.draft.scope === 'topic' && !topic.value)
const topics = ref([])
async function loadTopics() {
  try { topics.value = (await listV2MemoryTopics())?.topics ?? [] } catch { topics.value = [] }
}
onMounted(() => { if (pendingTopic.value) loadTopics() })
const assignedTopic = ref('')

// Edit: change the wording or the attribution before saving; the server
// re-checks conflicts with the final content under the final topic, and a
// duplicate judgement ends the draft without an abandon click.
const editing = ref(false)
const editForm = ref({})
function startEdit() {
  editing.value = true
  editForm.value = {
    subject: props.draft.subject ?? '', content: props.draft.content ?? '',
    tags: tags.value.join(','), topic_key: topic.value,
  }
  if (props.draft.scope === 'topic' && !topics.value.length) loadTopics()
}
function resetEdit() { editing.value = false; editForm.value = {} }
function parseTags(value) { return String(value).trim() ? String(value).trim().split(',').map((item) => item.trim()).filter(Boolean) : [] }
async function saveEdit() {
  const body = { subject: editForm.value.subject, content: editForm.value.content, tags: parseTags(editForm.value.tags) }
  if (props.draft.scope === 'topic') body.topic_key = editForm.value.topic_key
  const response = await card.editDraft(body)
  if (!response) return
  emit('changed')
  if (response.closed) resetEdit()
  else editing.value = false
}
async function assignTopic() {
  if (!assignedTopic.value) return
  const response = await card.editDraft({ topic_key: assignedTopic.value })
  if (!response) return
  emit('changed')
}
</script>
<template>
  <section class="gl-card v2-draft" data-test="memory-draft-card" data-memory-contract="v2" aria-label="记忆草稿">
    <h3><AppIcon name="memory" :size="15" /> 记忆草稿</h3>
    <p class="v2-draft__meta">
      {{ memoryScopeText(draft.scope) }}<span v-if="topic"> · {{ topic }}</span><span v-else-if="pendingTopic"> · 归属待定</span>
      · 来源：{{ draft.source || '对话提议' }} · {{ new Date(draft.created_at).toLocaleString() }}
    </p>
    <p v-if="draft.subject">{{ draft.subject }}</p><p data-test="memory-draft-content">{{ draft.content }}</p>
    <p v-if="draft.durability">{{ memoryDurabilityText(draft.durability) }}<span v-if="validUntil">，有效期到 {{ validUntil }}</span></p>
    <p v-if="tags.length">标签：{{ tags.join('、') }}</p>

    <div v-if="pendingTopic" class="v2-draft__assign" data-test="v2-draft-assign-topic">
      <label>选择归属主题（必填后才能保存）
        <select v-model="assignedTopic" data-test="v2-draft-topic-select" :disabled="op.busy.value">
          <option value="" disabled>请选择主题</option>
          <option v-for="item in topics" :key="item.key" :value="item.key">{{ item.name || item.key }}</option>
        </select>
      </label>
      <label>或新建主题
        <input v-model="assignedTopic" data-test="v2-draft-topic-input" :disabled="op.busy.value" placeholder="输入新主题名">
      </label>
      <button class="gl-btn" data-test="v2-draft-topic-save" :disabled="op.busy.value || !assignedTopic.trim()" @click="assignTopic">保存归属</button>
    </div>

    <div v-if="conflicts.length" class="v2-draft__conflicts" data-test="v2-draft-conflicts">
      <p class="gl-muted">这条草稿会改到下面的旧记忆：</p>
      <ul>
        <li v-for="item in conflictTargets" :key="item.id">
          <strong>{{ item.subject }}</strong>
          <span class="gl-muted">{{ item.content }}</span>
          <span v-if="item.updated_at" class="gl-muted">（保存于 {{ new Date(item.updated_at).toLocaleDateString() }}）</span>
        </li>
      </ul>
    </div>

    <p class="gl-muted">这条草稿还没有生效。只有你点下面的按钮确认之后，它才会被记住。</p>
    <label>填写原因（新增或替换时必填，最多 500 字）<input v-model="reason" data-test="v2-draft-reason" :disabled="op.busy.value"></label>
    <label v-if="conflicts.length">选择要替换的旧记忆（替换时必须明确选择）
      <select v-model="target" data-test="v2-draft-target" :disabled="op.busy.value"><option value="" disabled>请选择目标</option><option v-for="(id, index) in conflicts" :key="id" :value="id">{{ targetLabel(id) || `原记忆 ${index + 1}` }}</option></select>
    </label>
    <div class="v2-actions">
      <button class="gl-btn" data-test="v2-draft-add" :disabled="op.busy.value || pendingTopic || !reason.trim()" @click="card.decide('add', reason)">{{ draftActionText('add') }}</button>
      <button v-if="conflicts.length" class="gl-btn" data-test="v2-draft-replace" :disabled="op.busy.value || pendingTopic || !reason.trim() || !conflicts.includes(target)" @click="card.decide('replace', reason, target)">{{ draftActionText('replace') }}</button>
      <button class="gl-btn" data-test="v2-draft-cancel" :disabled="op.busy.value" @click="card.decide('cancel', reason)">{{ draftActionText('cancel') }}</button>
      <button class="gl-btn" data-test="v2-draft-edit" :disabled="op.busy.value || pendingTopic" @click="editing ? resetEdit() : startEdit()">{{ editing ? '取消编辑' : '编辑' }}</button>
      <button v-if="op.busy.value" class="gl-btn" data-test="v2-draft-abort" @click="card.cancel">取消当前请求</button>
      <button v-if="op.pending.value && !op.busy.value" class="gl-btn" data-test="v2-draft-retry" @click="card.retry">重试刚才的请求</button>
    </div>

    <form v-if="editing" class="v2-draft__edit" data-test="v2-draft-edit-form" @submit.prevent="saveEdit">
      <label>标题<input v-model="editForm.subject" data-test="v2-draft-edit-subject" required :maxlength="200" :disabled="op.busy.value"></label>
      <label>内容<textarea v-model="editForm.content" data-test="v2-draft-edit-content" required :maxlength="400" :disabled="op.busy.value"></textarea></label>
      <label>标签（逗号分隔）<input v-model="editForm.tags" data-test="v2-draft-edit-tags" :disabled="op.busy.value"></label>
      <label v-if="draft.scope === 'topic'">归属主题<input v-model="editForm.topic_key" data-test="v2-draft-edit-topic" :disabled="op.busy.value"></label>
      <button class="gl-btn" type="submit" data-test="v2-draft-edit-save" :disabled="op.busy.value || !editForm.content?.trim()">保存修改（保存前会重新查重）</button>
    </form>

    <div v-if="targetModified" class="v2-draft__modified" data-test="v2-draft-target-modified">
      <p><strong>要替换的旧记忆刚被修改过。</strong>下面是最新内容：</p>
      <ul>
        <li v-for="item in latestConflicts" :key="item.id">
          <strong>{{ item.subject }}</strong>
          <span class="gl-muted">{{ item.content }}</span>
          <span v-if="item.updated_at" class="gl-muted">（保存于 {{ new Date(item.updated_at).toLocaleDateString() }}）</span>
        </li>
      </ul>
      <button class="gl-btn" data-test="v2-draft-reconfirm" :disabled="op.busy.value || !reason.trim() || !target" @click="card.decide('replace', reason, target, latestHash)">
        已看过最新内容，仍要替换
      </button>
    </div>

    <ServerNotice v-if="op.error.value && !targetModified" :message="op.error.value.message" :code="op.error.value.code" />
    <div v-if="card.outcome.value" data-test="v2-draft-outcome">
      <p v-if="card.outcome.value.closed" data-test="v2-draft-duplicate-closed">
        这条与已有记忆相同（{{ card.outcome.value.conflicts?.[0]?.subject || '已有条目' }}），已沿用旧条目，草稿结束。
      </p>
      <p v-else-if="card.outcome.value.draft.status === 'confirmed' && card.outcome.value.memory?.status === 'active'" data-test="v2-active-confirmation">这条记忆已保存，之后回答时会想起它。</p>
      <p v-else-if="card.outcome.value.draft.status === 'cancelled'">这条草稿还没有生效，已关闭；没有写入任何记忆。</p>
      <p v-else>这条记忆还没有生效；以服务端的返回为准。</p>
      <p v-if="card.outcome.value.replay" class="gl-muted" data-test="v2-draft-replay">该草稿已处理过；下面是服务端记录的当前结果，没有产生第二条。</p>
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
.v2-draft__meta { color: var(--gl-ink-soft); font-size: .85rem; }
.v2-actions { display: flex; flex-wrap: wrap; gap: .4rem; margin-block: .5rem; }
.v2-draft__conflicts ul, .v2-draft__modified ul, .v2-draft__assign, .v2-draft__edit { display: grid; gap: .35rem; margin-block: .4rem; }
.v2-draft__conflicts li, .v2-draft__modified li { display: flex; flex-direction: column; padding: .35rem .55rem; border: 1px solid var(--gl-surface-line); border-radius: var(--gl-radius-sm); background: var(--gl-surface); }
.v2-draft__modified { border: 1px solid #f0d8b4; background: #fffdf7; padding: .5rem .6rem; border-radius: var(--gl-radius-sm); }
.v2-draft__edit label, .v2-draft__assign label { display: grid; gap: .2rem; }
label { display: grid; gap: .25rem; } pre { white-space: pre-wrap; overflow-wrap: anywhere; }
</style>
