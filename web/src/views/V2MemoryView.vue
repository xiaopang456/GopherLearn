<script setup>
import { onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { NButton } from 'naive-ui'

import GlEmpty from '../components/GlEmpty.vue'
import GlPageHeader from '../components/GlPageHeader.vue'
import GlStatusTag from '../components/GlStatusTag.vue'
import ServerNotice from '../components/ServerNotice.vue'
import V2MemoryDraftCard from '../components/V2MemoryDraftCard.vue'
import { useV2MemoryPage } from '../composables/useV2MemoryPage.js'

const page = useV2MemoryPage()
const op = page.operation
const form = reactive({ scope: '', subject: '', content: '', topic_key: '', durability: '', valid_until: '', tags: '', reason: '' })
const filter = reactive({ scope: '', status: 'active' })
const edit = ref(null)
const reason = ref('')
const decision = ref('')
const target = ref('')
watch(() => op.result.value, () => { decision.value = ''; target.value = '' })

function tags(value) { return value.trim() ? value.split(',').map((item) => item.trim()) : [] }

function body() {
  return {
    scope: form.scope,
    subject: form.subject,
    content: form.content,
    durability: form.durability,
    reason: form.reason,
    tags: tags(form.tags),
    ...(form.topic_key ? { topic_key: form.topic_key } : {}),
    ...(form.valid_until ? { valid_until: form.valid_until } : {}),
  }
}

function refresh() { return page.refresh({ ...(filter.scope ? { scope: filter.scope } : {}), status: filter.status }) }

async function perform(kind, payload, id) {
  if (op.busy.value) return
  for (const [field, limit] of [['subject', 200], ['content', 400], ['topic_key', 160], ['reason', 500]]) {
    const value = String(payload[field] ?? '')
    if (payload[field] !== undefined && Array.from(field === 'reason' ? value : value.trim()).length > limit) {
      op.error.value = { message: `${field} 超过原 ${limit} 字上限。`, code: 'invalid_form' }
      return
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

function editMemory(memory) {
  edit.value = { id: memory.id, subject: memory.subject, content: memory.content, tags: (memory.tags || []).join(','), reason: '' }
}

function scopeLabel(scope) { return scope === 'topic' ? '主题' : '全局' }

function durabilityLabel(durability) { return durability === 'time_bound' ? '有期限' : '长期有效' }

onMounted(refresh)
onBeforeUnmount(page.dispose)
</script>

<template>
  <div class="gl-page memory">
    <GlPageHeader
      title="记忆治理"
      description="你想让 AI 一直记住的事。回答时会被检索，只有你确认过的才会生效。"
    />

    <ServerNotice
      v-if="op.error.value || page.readError.value"
      :message="(op.error.value || page.readError.value).message"
      :code="(op.error.value || page.readError.value).code"
    />

    <p v-if="op.result.value?.memory" class="memory__done" role="status" data-test="v2-operation-result">
      已保存：{{ op.result.value.memory.subject }}
    </p>

    <div class="memory__actions">
      <n-button v-if="op.busy.value" size="small" data-test="v2-cancel" @click="op.cancel">
        取消本次请求
      </n-button>
      <n-button v-if="op.pending.value && !op.busy.value" size="small" data-test="v2-retry" @click="retry">
        重试上一次操作
      </n-button>
    </div>

    <details class="gl-disclosure memory__create">
      <summary>新建一条记忆</summary>
      <div class="gl-disclosure__body">
        <form data-test="v2-create-form" @submit.prevent="perform('create', body())">
          <div class="memory__grid">
            <label class="gl-label">
              <span class="gl-label__name">范围</span>
              <select v-model="form.scope" required class="gl-field" data-test="scope">
                <option value="" disabled>请选择范围</option>
                <option value="global">全局</option>
                <option value="topic">主题</option>
              </select>
            </label>

            <label class="gl-label">
              <span class="gl-label__name">有效期</span>
              <select v-model="form.durability" required class="gl-field" data-test="durability">
                <option value="" disabled>请选择有效期类型</option>
                <option value="stable">长期有效</option>
                <option value="time_bound">到某个时间为止</option>
              </select>
            </label>

            <label class="gl-label memory__span">
              <span class="gl-label__name">标题</span>
              <input v-model="form.subject" required class="gl-field" data-test="subject" placeholder="一句话概括这条记忆">
            </label>

            <label class="gl-label memory__span">
              <span class="gl-label__name">内容</span>
              <textarea v-model="form.content" required class="gl-field" data-test="content" placeholder="要记住的具体内容" />
            </label>

            <label class="gl-label">
              <span class="gl-label__name">主题</span>
              <input v-model="form.topic_key" class="gl-field" data-test="topic-key" placeholder="主题范围时必填">
              <span class="gl-label__hint">选了「主题」才需要填</span>
            </label>

            <label class="gl-label">
              <span class="gl-label__name">到期时间</span>
              <input v-model="form.valid_until" class="gl-field" data-test="valid-until" placeholder="2030-01-01T00:00:00+08:00">
              <span class="gl-label__hint">选了「到某个时间为止」才需要填</span>
            </label>

            <label class="gl-label">
              <span class="gl-label__name">标签</span>
              <input v-model="form.tags" class="gl-field" data-test="tags" placeholder="用逗号分隔，可留空">
            </label>

            <label class="gl-label">
              <span class="gl-label__name">原因</span>
              <input v-model="form.reason" required class="gl-field" data-test="reason" placeholder="为什么值得记住">
            </label>
          </div>

          <div class="memory__submit">
            <n-button type="primary" attr-type="submit" :disabled="op.busy.value" data-test="v2-create">
              保存这条记忆
            </n-button>
          </div>
        </form>
      </div>
    </details>

    <section v-if="op.result.value?.conflicts?.length" class="memory__conflict gl-card" data-test="v2-conflicts">
      <h2 class="memory__conflict-title">这条和已有的记忆重复了</h2>
      <p class="gl-muted">系统没有替你选，两条都在。你决定是都留着，还是用新的替换其中一条。</p>
      <ul class="memory__conflict-list">
        <li v-for="item in op.result.value.conflicts" :key="item.id">
          <strong>{{ item.subject }}</strong>
          <span class="gl-muted">{{ item.content }}</span>
        </li>
      </ul>
      <div class="memory__conflict-actions">
        <label class="gl-label">
          <span class="gl-label__name">你的决定</span>
          <select v-model="decision" class="gl-field" data-test="create-decision">
            <option value="" disabled>请选择</option>
            <option value="add">两条都留着</option>
            <option value="replace">用新的替换其中一条</option>
          </select>
        </label>
        <label v-if="decision === 'replace'" class="gl-label">
          <span class="gl-label__name">替换哪一条</span>
          <select v-model="target" class="gl-field" data-test="create-target">
            <option value="" disabled>请选择要替换的记忆</option>
            <option v-for="item in op.result.value.conflicts" :key="item.id" :value="item.id">
              {{ item.subject }}
            </option>
          </select>
        </label>
        <n-button
          type="primary"
          :disabled="!decision || (decision === 'replace' && !target)"
          @click="conflictDecision"
        >
          确认这个决定
        </n-button>
      </div>
    </section>

    <div class="memory__filter">
      <label class="gl-label">
        <span class="gl-label__name">范围</span>
        <select v-model="filter.scope" class="gl-field">
          <option value="">全部</option>
          <option value="global">全局</option>
          <option value="topic">主题</option>
        </select>
      </label>
      <label class="gl-label">
        <span class="gl-label__name">状态</span>
        <select v-model="filter.status" class="gl-field">
          <option v-for="state in ['active', 'archived', 'superseded', 'deleted']" :key="state" :value="state">
            {{ { active: '生效中', archived: '已归档', superseded: '已被替代', deleted: '已删除' }[state] }}
          </option>
        </select>
      </label>
      <n-button size="small" data-test="v2-refresh" @click="refresh">刷新</n-button>
    </div>

    <label class="gl-label memory__reason">
      <span class="gl-label__name">操作原因</span>
      <input
        v-model="reason"
        class="gl-field"
        data-test="manage-reason"
        placeholder="归档或删除前需要写明原因"
      >
      <span class="gl-label__hint">归档与删除需要先填这里</span>
    </label>

    <GlEmpty
      v-if="page.memories.value.length === 0"
      description="还没有记忆。保存一条你希望 AI 一直记住的偏好试试。"
    />

    <ul v-else class="memory__list">
      <li v-for="memory in page.memories.value" :key="memory.id" class="memory__item gl-card" data-test="v2-memory-row">
        <div class="memory__item-head">
          <h2 class="memory__subject">{{ memory.subject }}</h2>
          <div class="memory__tags">
            <GlStatusTag :status="memory.status" />
            <span class="memory__chip">{{ scopeLabel(memory.scope) }}</span>
            <span class="memory__chip">{{ durabilityLabel(memory.durability) }}</span>
          </div>
        </div>

        <p class="memory__content">{{ memory.content }}</p>

        <p v-if="memory.tags?.length" class="memory__meta">标签：{{ memory.tags.join('、') }}</p>

        <div class="memory__item-actions">
          <n-button size="small" quaternary data-test="v2-revisions" @click="page.showRevisions(memory.id)">
            修订记录
          </n-button>
          <n-button
            v-if="memory.status === 'active'"
            size="small"
            quaternary
            data-test="v2-edit"
            @click="editMemory(memory)"
          >
            修改
          </n-button>
          <n-button
            v-if="memory.status === 'active'"
            size="small"
            quaternary
            :disabled="op.busy.value || !reason.trim()"
            data-test="v2-archive"
            @click="perform('archive', { reason }, memory.id)"
          >
            归档
          </n-button>
          <n-button
            size="small"
            quaternary
            type="error"
            :disabled="op.busy.value || !reason.trim()"
            data-test="v2-delete"
            @click="perform('delete', { reason }, memory.id)"
          >
            删除
          </n-button>
        </div>

        <form
          v-if="edit && edit.id === memory.id"
          class="memory__edit"
          data-test="v2-edit-form"
          @submit.prevent="perform('correct', { subject: edit.subject, content: edit.content, tags: tags(edit.tags), reason: edit.reason }, edit.id)"
        >
          <label class="gl-label">
            <span class="gl-label__name">标题</span>
            <input v-model="edit.subject" required class="gl-field" aria-label="修改 subject">
          </label>
          <label class="gl-label">
            <span class="gl-label__name">内容</span>
            <textarea v-model="edit.content" required class="gl-field" aria-label="修改 content" />
          </label>
          <label class="gl-label">
            <span class="gl-label__name">标签</span>
            <input v-model="edit.tags" required class="gl-field" aria-label="修改 tags">
          </label>
          <label class="gl-label">
            <span class="gl-label__name">修改原因</span>
            <input v-model="edit.reason" required class="gl-field" aria-label="修改 reason">
          </label>
          <n-button type="primary" attr-type="submit" size="small" :disabled="op.busy.value" data-test="v2-correct">
            保存修改
          </n-button>
        </form>
      </li>
    </ul>

    <details v-if="page.revisions.value" class="gl-disclosure memory__revisions" open data-test="v2-revision-result">
      <summary>修订记录（最多 100 条）</summary>
      <div class="gl-disclosure__body">
        <ul class="memory__revision-list">
          <li v-for="item in page.revisions.value.items" :key="item.id">
            <span class="memory__revision-id">{{ item.id }}</span>
            <span class="memory__revision-reason">{{ item.reason }}</span>
          </li>
        </ul>
      </div>
    </details>

    <section v-if="page.drafts.value.length" class="memory__drafts">
      <h2 class="memory__section-title">待确认的草稿</h2>
      <V2MemoryDraftCard v-for="draft in page.drafts.value" :key="draft.id" :draft="draft" />
    </section>
  </div>
</template>

<style scoped>
.memory {
  max-width: 900px;
}

.memory__actions {
  display: flex;
  gap: var(--gl-space-2);
  margin-bottom: var(--gl-space-3);
}

.memory__done {
  margin: 0 0 var(--gl-space-3);
  padding: var(--gl-space-2) var(--gl-space-3);
  border-radius: var(--gl-radius-sm);
  background: var(--gl-accent-soft);
  color: var(--gl-accent-ink);
  font-size: var(--gl-text-sm);
  font-weight: 600;
}

.memory__create {
  margin-bottom: var(--gl-space-4);
}

.memory__grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: var(--gl-space-3);
}

.memory__span {
  grid-column: 1 / -1;
}

.memory__submit {
  display: flex;
  justify-content: flex-end;
  padding-top: var(--gl-space-4);
}

.memory__conflict {
  margin-bottom: var(--gl-space-4);
  padding: var(--gl-space-4);
  border-color: #f0d8b4;
  background: #fffdf7;
}

.memory__conflict-title {
  font-size: var(--gl-text-base);
  font-weight: 600;
}

.memory__conflict-list {
  list-style: none;
  margin: var(--gl-space-3) 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: var(--gl-space-2);
}

.memory__conflict-list li {
  display: flex;
  flex-direction: column;
  gap: 2px;
  padding: var(--gl-space-2) var(--gl-space-3);
  border-radius: var(--gl-radius-sm);
  background: var(--gl-surface);
  border: 1px solid var(--gl-surface-line);
}

.memory__conflict-actions {
  display: flex;
  flex-wrap: wrap;
  align-items: flex-end;
  gap: var(--gl-space-3);
}

.memory__filter {
  display: flex;
  align-items: flex-end;
  gap: var(--gl-space-3);
  margin-bottom: var(--gl-space-3);
}

.memory__filter .gl-label {
  width: 160px;
}

.memory__reason {
  max-width: 420px;
  margin-bottom: var(--gl-space-4);
}

.memory__list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: var(--gl-space-3);
}

.memory__item {
  padding: var(--gl-space-4);
}

.memory__item-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: var(--gl-space-3);
}

.memory__subject {
  font-size: var(--gl-text-lg);
  font-weight: 600;
  letter-spacing: -0.01em;
}

.memory__tags {
  display: flex;
  flex: none;
  flex-wrap: wrap;
  gap: var(--gl-space-1);
}

.memory__chip {
  padding: 0.1em 0.55em;
  border-radius: var(--gl-radius-pill);
  background: var(--gl-surface-soft);
  border: 1px solid var(--gl-surface-line);
  color: var(--gl-ink-soft);
  font-size: var(--gl-text-xs);
  font-weight: 600;
}

.memory__content {
  margin-top: var(--gl-space-2);
  font-size: var(--gl-text-base);
  line-height: var(--gl-leading-normal);
  color: var(--gl-ink);
}

.memory__meta {
  margin-top: var(--gl-space-2);
  font-size: var(--gl-text-xs);
  color: var(--gl-ink-faint);
}

.memory__item-actions {
  display: flex;
  flex-wrap: wrap;
  gap: var(--gl-space-1);
  margin-top: var(--gl-space-3);
  padding-top: var(--gl-space-2);
  border-top: 1px solid var(--gl-surface-line);
}

.memory__edit {
  display: flex;
  flex-direction: column;
  gap: var(--gl-space-3);
  margin-top: var(--gl-space-3);
  padding-top: var(--gl-space-3);
  border-top: 1px solid var(--gl-surface-line);
}

.memory__edit .gl-btn,
.memory__edit .n-button {
  align-self: flex-start;
}

.memory__revisions {
  margin-top: var(--gl-space-4);
}

.memory__revision-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: var(--gl-space-1);
}

.memory__revision-list li {
  display: flex;
  gap: var(--gl-space-3);
  font-size: var(--gl-text-sm);
}

.memory__revision-id {
  font-family: var(--gl-mono);
  font-size: var(--gl-text-xs);
  color: var(--gl-ink-faint);
}

.memory__revision-reason {
  color: var(--gl-ink-soft);
}

.memory__drafts {
  margin-top: var(--gl-space-5);
}

.memory__section-title {
  font-size: var(--gl-text-lg);
  font-weight: 600;
  margin-bottom: var(--gl-space-3);
}

@media (max-width: 700px) {
  .memory__grid {
    grid-template-columns: minmax(0, 1fr);
  }

  .memory__filter {
    flex-wrap: wrap;
  }

  .memory__filter .gl-label {
    width: 100%;
  }
}
</style>
