<script setup>
import { computed, onMounted, reactive, ref } from 'vue'

import AppIcon from '../components/AppIcon.vue'
import BaselineSurfaceNotice from '../components/BaselineSurfaceNotice.vue'
import SectionFrame from '../components/SectionFrame.vue'
import ServerNotice from '../components/ServerNotice.vue'
import {
  MANUAL_MEMORY_SCOPES,
  MEMORY_CREATE_LIMITS,
  MEMORY_FORM_COPY,
  MemoryScope,
  scopeChoiceLabel,
  scopeLabel,
  validateMemoryForm,
} from '../api/memory.js'
import { useMemory } from '../composables/useMemory.js'

/**
 * The long-term memory screen: what is waiting for a decision, what counts now,
 * a form for stating one yourself, and a read-only note about what has been retired.
 *
 * Nothing here is a capability claim. The copy avoids auto-extraction, mastery
 * updates, topic authorisation and cross-session recall, because none of those
 * has an endpoint in this build — the page shows records the server handed over
 * and the transitions the confirmation gate is made of.
 *
 * The two writes on this screen differ in who decided, and the copy says so:
 * something the learning agent proposed arrives as a draft you have to confirm,
 * while something you type yourself arrives already in force — you are the author,
 * and pressing "确认并添加" *is* the confirmation. There is deliberately no source
 * picker: a hand-written fact has no conversation behind it, and the endpoint
 * refuses one.
 */

const memory = useMemory()

const drafts = computed(() => memory.pendingDrafts.value)
const actives = computed(() => memory.activeMemories.value)
const isBusy = computed(() => memory.mutating.value)

const SOURCE_BADGES = {
  mock: '演示数据',
  unbound: '接口未开通',
  http: '已接入',
}

const sourceBadge = computed(() => SOURCE_BADGES[memory.dataSource.value] ?? '来源未识别')

const form = reactive({
  scope: MemoryScope.Global,
  topicKey: '',
  content: '',
})
const fieldErrors = ref({})

/** Only what a person may assert by hand: `episodic` describes a conversation. */
const scopeChoices = MANUAL_MEMORY_SCOPES
const needsTopicKey = computed(() => form.scope === MemoryScope.Topic)
const contentRunes = computed(() => Array.from(form.content.trim()).length)

onMounted(() => {
  memory.loadAll()
})

function confidenceText(value) {
  return value === null ? '未提供' : value.toFixed(2)
}

function stampText(value) {
  if (!value) return '未记录'
  const parsed = Date.parse(value)
  if (Number.isNaN(parsed)) return value
  return new Intl.DateTimeFormat('zh-CN', {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
  }).format(new Date(parsed))
}

function topicText(record) {
  return record.topicKey === '' ? '未标注' : record.topicKey
}

function messageText(record) {
  const ids = record.sources.flatMap((source) => source.messageIds)
  return ids.length === 0 ? '未记录' : ids.join('、')
}

function sessionText(record) {
  const ids = record.sources
    .map((source) => source.sessionId)
    .filter((id) => id !== '')
  return ids.length === 0 ? '未记录' : ids.join('、')
}

function fieldError(name) {
  return fieldErrors.value[name] ?? ''
}

async function submitMemory() {
  // The refusal happens here rather than behind a dead button, so an incomplete
  // form can name the field it is missing. Either way no request leaves the page.
  const { valid, errors } = validateMemoryForm(form)
  fieldErrors.value = errors
  if (!valid) return
  const result = await memory.addMemory({ ...form })
  if (!result.added) return
  // Only a memory the server stored clears the form: a failed write leaves every
  // keystroke in place, because the learner's text is the one thing a 503 cannot
  // recover.
  form.scope = MemoryScope.Global
  form.topicKey = ''
  form.content = ''
}

function isRowBusy(id) {
  return isBusy.value && memory.busyId.value === id
}

async function confirmDraft(draft) {
  await memory.confirmDraft(draft.id)
}

async function rejectDraft(draft) {
  await memory.rejectDraft(draft.id)
}

async function archive(memoryRow) {
  await memory.archiveMemory(memoryRow.id)
}
</script>

<template>
  <div class="gl-page">
    <SectionFrame
      icon="memory"
      eyebrow="记忆"
      title="长期记忆"
      summary="这里列出当前生效的记忆，以及等待你决定的草稿。自己写下的记忆立即生效，学习 Agent 提出的要先确认。"
      :status="sourceBadge"
      :status-tone="memory.dataSource.value === 'http' ? 'done' : 'warn'"
    >
      <p v-if="memory.showsDemoData.value" class="mem__demo" data-test="memory-demo-note">
        下面这些记录来自前端内置的演示数据，用于本阶段验收界面与状态流转，不是这个账号的真实记忆。
      </p>

      <BaselineSurfaceNotice :replacement="{ href: '/memory/v2', label: '记忆治理' }" />

      <p
        v-if="memory.flash.value"
        class="mem__flash"
        role="status"
        data-test="memory-flash"
      >
        {{ memory.flash.value }}
      </p>
      <div v-if="memory.actionError.value" data-test="memory-action-error">
        <ServerNotice :message="memory.actionError.value" />
      </div>

      <!-- 手动添加记忆 -->
      <section
        v-if="memory.canAddMemory.value"
        class="mem__block"
        aria-labelledby="memory-add-heading"
      >
        <header class="mem__head">
          <h2 id="memory-add-heading">手动添加记忆</h2>
        </header>
        <p class="gl-muted mem__lead">
          写下你希望一直被告知的事情，添加后立即生效，并替代同一条主题下的旧说法。
        </p>

        <form class="mem__form" data-test="memory-add-form" @submit.prevent="submitMemory">
          <fieldset class="mem__choices">
            <legend>记忆类型</legend>
            <label v-for="choice in scopeChoices" :key="choice" class="mem__choice">
              <input
                v-model="form.scope"
                type="radio"
                name="memory-scope"
                data-test="add-scope"
                :value="choice"
                :disabled="isBusy"
              />
              <span>{{ scopeChoiceLabel(choice) }}</span>
            </label>
          </fieldset>

          <label v-if="needsTopicKey" class="mem__field" for="memory-topic-key">
            <span>主题标识</span>
            <input
              id="memory-topic-key"
              v-model="form.topicKey"
              type="text"
              autocomplete="off"
              data-test="add-topic-key"
              :aria-invalid="fieldError('topicKey') ? 'true' : undefined"
            />
            <small v-if="fieldError('topicKey')" class="mem__field-error" data-test="add-error-topic">
              {{ fieldError('topicKey') }}
            </small>
            <small v-else class="mem__hint">同主题的记忆会互相替代，标识最多 {{ MEMORY_CREATE_LIMITS.topicKeyRunes }} 个字符。</small>
          </label>

          <label class="mem__field" for="memory-content">
            <span>记忆内容</span>
            <textarea
              id="memory-content"
              v-model="form.content"
              rows="3"
              data-test="add-content"
              :aria-invalid="fieldError('content') ? 'true' : undefined"
            ></textarea>
            <small v-if="fieldError('content')" class="mem__field-error" data-test="add-error-content">
              {{ fieldError('content') }}
            </small>
            <small v-else class="mem__hint" data-test="add-content-count">
              {{ contentRunes }} / {{ MEMORY_CREATE_LIMITS.contentRunes }} 字符
            </small>
          </label>

          <div class="mem__actions">
            <button
              type="submit"
              class="gl-btn gl-btn--primary"
              data-test="add-submit"
              :disabled="isBusy"
            >
              {{ memory.adding.value ? MEMORY_FORM_COPY.submitting : '确认并添加' }}
            </button>
            <span class="mem__hint">添加后立即生效，不需要再确认一次。</span>
          </div>
        </form>
      </section>

      <!-- 待确认草稿 -->
      <section class="mem__block" aria-labelledby="memory-pending-heading">
        <header class="mem__head">
          <h2 id="memory-pending-heading">待确认草稿</h2>
          <span v-if="memory.draftsLoading.value" class="mem__busy">载入中…</span>
        </header>
        <p class="gl-muted mem__lead">
          这些内容还没有生效：确认才会写入长期记忆，拒绝不会。
        </p>

        <div v-if="memory.draftsError.value">
          <ServerNotice :message="memory.draftsError.value" />
        </div>

        <p
          v-if="memory.draftsLoading.value || !memory.draftsLoaded.value"
          class="mem__busy"
          data-test="drafts-loading"
        >
          正在读取待确认草稿…
        </p>

        <p
          v-else-if="!memory.draftsError.value && drafts.length === 0"
          class="mem__empty"
          data-test="drafts-empty"
        >
          现在没有等待确认的草稿。
        </p>

        <ul
          v-else-if="drafts.length > 0"
          class="mem__list"
          data-test="draft-list"
          :aria-busy="isBusy ? 'true' : undefined"
        >
          <li
            v-for="draft in drafts"
            :key="draft.id"
            class="mem__card gl-card"
            data-test="draft-card"
          >
            <div class="mem__card-head">
              <h3 class="mem__scope">
                <AppIcon name="memory" :size="15" />
                <span>{{ scopeLabel(draft.scope) }}</span>
              </h3>
              <span class="mem__confidence">置信度 {{ confidenceText(draft.confidence) }}</span>
            </div>

            <p class="mem__content" data-test="draft-content">{{ draft.content }}</p>

            <dl class="mem__facts">
              <div>
                <dt>主题标识</dt>
                <dd class="mem__mono">{{ topicText(draft) }}</dd>
              </div>
              <div>
                <dt>来源会话</dt>
                <dd class="mem__mono">{{ sessionText(draft) }}</dd>
              </div>
              <div>
                <dt>来源消息</dt>
                <dd class="mem__mono">{{ messageText(draft) }}</dd>
              </div>
              <div>
                <dt>提出时间</dt>
                <dd>{{ stampText(draft.createdAt) }}</dd>
              </div>
            </dl>

            <div class="mem__actions">
              <button
                type="button"
                class="gl-btn gl-btn--primary"
                data-test="confirm-draft"
                :disabled="isBusy"
                :aria-label="`确认这条${scopeLabel(draft.scope)}草稿`"
                @click="confirmDraft(draft)"
              >
                {{ isRowBusy(draft.id) ? '确认中…' : '确认' }}
              </button>
              <button
                type="button"
                class="gl-btn"
                data-test="reject-draft"
                :disabled="isBusy"
                :aria-label="`拒绝这条${scopeLabel(draft.scope)}草稿`"
                @click="rejectDraft(draft)"
              >
                {{ isRowBusy(draft.id) ? '处理中…' : '拒绝' }}
              </button>
            </div>
          </li>
        </ul>
      </section>

      <!-- 当前有效记忆 -->
      <section class="mem__block" aria-labelledby="memory-active-heading">
        <header class="mem__head">
          <h2 id="memory-active-heading">当前有效记忆</h2>
          <span v-if="memory.memoriesLoading.value" class="mem__busy">载入中…</span>
        </header>
        <p class="gl-muted mem__lead">
          分为全局画像、主题记忆和情景记忆三种。这里可以归档，但不提供编辑：归档只是停止把它算作有效记忆。
        </p>

        <div v-if="memory.memoriesError.value">
          <ServerNotice :message="memory.memoriesError.value" />
        </div>

        <p
          v-if="memory.memoriesLoading.value || !memory.memoriesLoaded.value"
          class="mem__busy"
          data-test="memories-loading"
        >
          正在读取有效记忆…
        </p>

        <p
          v-else-if="!memory.memoriesError.value && actives.length === 0"
          class="mem__empty"
          data-test="memories-empty"
        >
          还没有生效中的记忆。
        </p>

        <ul
          v-else-if="actives.length > 0"
          class="mem__list"
          data-test="memory-list"
          :aria-busy="isBusy ? 'true' : undefined"
        >
          <li
            v-for="item in actives"
            :key="item.id"
            class="mem__card gl-card"
            data-test="memory-card"
          >
            <div class="mem__card-head">
              <h3 class="mem__scope">
                <AppIcon name="memory" :size="15" />
                <span>{{ scopeLabel(item.scope) }}</span>
              </h3>
              <span class="mem__confidence">置信度 {{ confidenceText(item.confidence) }}</span>
            </div>

            <p class="mem__content" data-test="memory-content">{{ item.content }}</p>

            <dl class="mem__facts">
              <div>
                <dt>主题标识</dt>
                <dd class="mem__mono">{{ topicText(item) }}</dd>
              </div>
              <div>
                <dt>来源会话</dt>
                <dd class="mem__mono">{{ sessionText(item) }}</dd>
              </div>
              <div>
                <dt>来源消息</dt>
                <dd class="mem__mono">{{ messageText(item) }}</dd>
              </div>
              <div>
                <dt>创建时间</dt>
                <dd>{{ stampText(item.createdAt) }}</dd>
              </div>
            </dl>

            <div class="mem__actions">
              <button
                type="button"
                class="gl-btn"
                data-test="archive-memory"
                :disabled="isBusy"
                :aria-label="`归档${scopeLabel(item.scope)}`"
                @click="archive(item)"
              >
                {{ isRowBusy(item.id) ? '归档中…' : '归档' }}
              </button>
            </div>
          </li>
        </ul>
      </section>

      <!-- 已归档：只读提示 -->
      <section class="mem__block mem__block--quiet" aria-labelledby="memory-history-heading">
        <h2 id="memory-history-heading">已归档与历史状态</h2>
        <p class="gl-muted mem__lead" data-test="history-note">
          <template v-if="memory.memoriesError.value">
            记忆列表没有读到，所以本页给不出任何归档说明。
          </template>
          <template v-else>
            上面只列待确认草稿和生效中的记忆。归档与被替代的记录不在这个列表里，本页也不提供查看、恢复或编辑。<span
              v-if="memory.archivedHere.value > 0"
              data-test="history-count"
            >你在本页归档了 {{ memory.archivedHere.value }} 条。</span>
          </template>
        </p>
      </section>
    </SectionFrame>
  </div>
</template>

<style scoped>
.mem__demo {
  margin: 0 0 0.9rem;
  padding: 0.6rem 0.85rem;
  border: 1px dashed #e4d7bd;
  border-radius: var(--gl-radius-sm);
  background: var(--gl-warn-soft);
  color: var(--gl-warn);
  font-size: 0.9rem;
}

.mem__flash {
  margin: 0 0 0.9rem;
  padding: 0.6rem 0.85rem;
  border-radius: var(--gl-radius-sm);
  background: var(--gl-accent-soft);
  color: var(--gl-accent-ink);
  font-size: 0.92rem;
  font-weight: 600;
}

.mem__block {
  margin-top: 1.1rem;
  padding-top: 1.1rem;
  border-top: 1px solid var(--gl-surface-line);
}

.mem__block--quiet {
  background: var(--gl-surface-soft);
  border: 1px solid var(--gl-surface-line);
  border-radius: var(--gl-radius-sm);
  padding: 0.9rem 1rem;
  margin-top: 1.2rem;
}

.mem__block h2 {
  font-size: 1.05rem;
}

.mem__head {
  display: flex;
  align-items: baseline;
  gap: 0.6rem;
}

.mem__lead {
  margin: 0.35rem 0 0.8rem;
  font-size: 0.92rem;
}

.mem__busy,
.mem__empty {
  margin: 0;
  font-size: 0.92rem;
  color: var(--gl-ink-faint);
}

.mem__list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 0.7rem;
}

.mem__card {
  padding: 0.95rem 1rem 1rem;
}

.mem__card-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 0.8rem;
}

.mem__scope {
  display: flex;
  align-items: center;
  gap: 0.4rem;
  font-size: 0.95rem;
  color: var(--gl-accent-ink);
}

.mem__confidence {
  font-size: 0.8rem;
  color: var(--gl-ink-faint);
  white-space: nowrap;
}

.mem__content {
  margin: 0.55rem 0 0;
  font-size: 1rem;
  line-height: 1.55;
  overflow-wrap: anywhere;
}

.mem__facts {
  margin: 0.75rem 0 0;
  padding: 0;
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(150px, 1fr));
  gap: 0.4rem 1rem;
}

.mem__facts div {
  display: flex;
  flex-direction: column;
  gap: 0.1rem;
  min-width: 0;
}

.mem__facts dt {
  font-size: 0.74rem;
  color: var(--gl-ink-faint);
}

.mem__facts dd {
  margin: 0;
  font-size: 0.85rem;
  overflow-wrap: anywhere;
}

.mem__mono {
  font-family: var(--gl-mono);
}

.mem__actions {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 0.5rem;
  margin-top: 0.9rem;
}

.mem__form {
  display: flex;
  flex-direction: column;
  gap: 0.8rem;
  padding: 0.95rem 1rem 1.05rem;
  border: 1px solid var(--gl-surface-line);
  border-radius: var(--gl-radius-sm);
  background: var(--gl-surface-soft);
}

.mem__choices {
  display: flex;
  flex-wrap: wrap;
  gap: 1.1rem;
  margin: 0;
  padding: 0;
  border: 0;
}

.mem__choices legend {
  padding: 0 0 0.35rem;
  font-size: 0.88rem;
  color: var(--gl-ink-faint);
}

.mem__choice {
  display: flex;
  align-items: center;
  gap: 0.4rem;
  font-size: 0.95rem;
}

.mem__field {
  display: flex;
  flex-direction: column;
  gap: 0.3rem;
  font-size: 0.88rem;
  color: var(--gl-ink-faint);
  min-width: 0;
}

.mem__field input,
.mem__field select,
.mem__field textarea {
  padding: 0.5rem 0.6rem;
  border: 1px solid var(--gl-surface-line);
  border-radius: var(--gl-radius-sm);
  background: var(--gl-surface);
  color: var(--gl-ink);
  font: inherit;
  font-size: 0.95rem;
}

.mem__field textarea {
  resize: vertical;
  line-height: 1.5;
}

.mem__field :disabled {
  opacity: 0.6;
  cursor: not-allowed;
}

.mem__hint {
  margin: 0;
  font-size: 0.85rem;
  color: var(--gl-ink-faint);
}

.mem__field-error {
  margin: 0;
  font-size: 0.88rem;
  color: var(--gl-danger);
}
</style>
