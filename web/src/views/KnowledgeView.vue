<script setup>
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'

import AppIcon from '../components/AppIcon.vue'
import BaselineSurfaceNotice from '../components/BaselineSurfaceNotice.vue'
import SectionFrame from '../components/SectionFrame.vue'
import ServerNotice from '../components/ServerNotice.vue'
import { ACCEPT_ATTRIBUTE, MAX_CONTENT_BYTES, formatForFile } from '../api/knowledge.js'
import { useKnowledge } from '../composables/useKnowledge.js'

const knowledge = useKnowledge()
const router = useRouter()

const newTitle = ref('')
const uploadError = ref('')
const busyDocumentId = ref('')

const collections = computed(() => knowledge.collections.value)
const documents = computed(() => knowledge.documents.value)
const hasCollections = computed(() => collections.value.length > 0)
const activeId = computed(() => knowledge.activeCollectionId.value)

const acceptedExtensionList = ACCEPT_ATTRIBUTE
const MAX_LABEL = `${Math.floor(MAX_CONTENT_BYTES / (1024 * 1024))} MiB`

onMounted(async () => {
  if (collections.value.length === 0) await knowledge.loadCollections()
  if (!activeId.value && collections.value[0]) {
    await knowledge.selectCollection(collections.value[0].id)
  }
})

async function submitCollection() {
  const created = await knowledge.createCollection(newTitle.value)
  if (!created) return
  newTitle.value = ''
  await knowledge.selectCollection(created.id)
}

async function onFileChosen(event) {
  uploadError.value = ''
  const input = event.target
  const file = input.files?.[0]
  // Cleared first so choosing the same file twice still fires a change event.
  input.value = ''
  if (!file) return

  const mapped = formatForFile(file.name)
  if (mapped.error) {
    uploadError.value = mapped.error
    return
  }
  if (file.size > MAX_CONTENT_BYTES) {
    uploadError.value = `文件 ${file.size} 字节，超过 ${MAX_LABEL} 上限。`
    return
  }
  await knowledge.uploadDocument(activeId.value, file)
}

async function removeDocument(document) {
  const confirmed =
    typeof window.confirm !== 'function' ||
    window.confirm(`删除文档「${document.title}」？它将从检索中彻底移除。`)
  if (!confirmed) return
  busyDocumentId.value = document.id
  await knowledge.removeDocument(document.id)
  busyDocumentId.value = ''
}

function when(value) {
  if (!value) return ''
  const stamp = Date.parse(value)
  if (Number.isNaN(stamp)) return value
  return new Intl.DateTimeFormat('zh-CN', {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
  }).format(new Date(stamp))
}
</script>

<template>
  <div class="gl-page">
    <SectionFrame
      icon="knowledge"
      eyebrow="知识库"
      title="知识库与文档"
      summary="这里管理可被检索的资料。对话会话在每一轮单独决定是否依据它们回答。"
      status="已接入"
      status-tone="done"
    >
      <p class="gl-muted">隔离迁移中的 Collection 尚未接通；<button type="button" class="gl-btn" @click="router.push({ name: 'v2-documents' })">打开独立 V2 用户级文档管理</button>。本入口不替换 Collection 的选择范围。</p>
      <div class="kb">
        <section class="kb__side" aria-label="知识库列表">
          <header class="kb__head">
            <h2>知识库</h2>
            <span v-if="knowledge.collectionsLoading.value" class="kb__busy">载入中…</span>
          </header>

          <form class="kb__create" data-test="collection-form" @submit.prevent="submitCollection">
            <input
              v-model="newTitle"
              class="kb__input"
              type="text"
              maxlength="64"
              placeholder="新建知识库名称"
              aria-label="新建知识库名称"
              :disabled="knowledge.mutating.value"
            />
            <button
              type="submit"
              class="gl-btn gl-btn--primary"
              data-test="create-collection"
              :disabled="!newTitle.trim() || knowledge.mutating.value"
            >
              创建
            </button>
          </form>

          <BaselineSurfaceNotice :replacement="{ href: '/knowledge/v2-documents', label: '文档' }" />

          <p
            v-if="knowledge.collectionsError.value"
            class="kb__error"
            data-test="collections-error"
          >
            {{ knowledge.collectionsError.value }}
          </p>

          <p v-if="!hasCollections && !knowledge.collectionsError.value" class="kb__hint" data-test="collections-empty">
            还没有知识库。先创建一个，再上传 Markdown 或纯文本笔记。
          </p>

          <ul v-else class="kb__list" data-test="collection-list">
            <li v-for="item in collections" :key="item.id">
              <button
                type="button"
                class="kb__row"
                :class="{ 'kb__row--active': item.id === activeId }"
                data-test="collection-row"
                :aria-current="item.id === activeId ? 'true' : undefined"
                @click="knowledge.selectCollection(item.id)"
              >
                <AppIcon name="knowledge" :size="15" />
                <span class="kb__row-title">{{ item.title }}</span>
              </button>
            </li>
          </ul>
        </section>

        <section class="kb__main" aria-label="文档">
          <header class="kb__head">
            <h2>{{ knowledge.activeCollection.value?.title ?? '文档' }}</h2>
            <!--
              A label bound to the input, rather than a button that calls
              .click() on a pointer-events:none element: the control stays in
              the accessibility tree and the tab order, and screen readers get
              to announce what file is wanted.
            -->
            <label
              class="gl-btn kb__upload"
              :class="{ 'kb__upload--off': !activeId || knowledge.mutating.value }"
              data-test="upload-button"
              for="knowledge-file-input"
            >
              <AppIcon name="plus" :size="15" />
              上传文档
            </label>
            <input
              id="knowledge-file-input"
              ref="fileInput"
              class="kb__file"
              type="file"
              :accept="acceptedExtensionList"
              data-test="file-input"
              aria-label="选择要上传的 Markdown 或纯文本笔记"
              :disabled="!activeId || knowledge.mutating.value"
              @change="onFileChosen"
            />
          </header>

          <p class="kb__hint">支持 {{ acceptedExtensionList }}，单个文件不超过 {{ MAX_LABEL }}。</p>

          <p v-if="uploadError" class="kb__error" data-test="upload-error">{{ uploadError }}</p>
          <p v-if="knowledge.documentsError.value" class="kb__error" data-test="documents-error">
            {{ knowledge.documentsError.value }}
          </p>

          <p
            v-if="knowledge.documentsLoading.value"
            class="kb__busy"
            data-test="documents-loading"
          >
            正在读取文档列表…
          </p>

          <p
            v-else-if="!activeId"
            class="kb__empty"
            data-test="collection-unselected"
          >
            从左侧选择一个知识库来查看它的文档。
          </p>

          <p
            v-else-if="documents.length === 0"
            class="kb__empty"
            data-test="documents-empty"
          >
            这个知识库还没有文档。上传第一份笔记后，对话里才能依据它回答。
          </p>

          <ul v-else class="kb__docs" data-test="document-list">
            <li v-for="doc in documents" :key="doc.id" class="kb__doc gl-card">
              <div class="kb__doc-head">
                <p class="kb__doc-title">{{ doc.title }}</p>
                <button
                  type="button"
                  class="gl-btn gl-btn--quiet kb__doc-remove"
                  data-test="delete-document"
                  :aria-label="`删除文档 ${doc.title}`"
                  :disabled="busyDocumentId === doc.id"
                  @click="removeDocument(doc)"
                >
                  <AppIcon name="trash" :size="15" />
                  <span>{{ busyDocumentId === doc.id ? '删除中…' : '删除' }}</span>
                </button>
              </div>
              <dl class="kb__facts">
                <div>
                  <dt>格式</dt>
                  <dd>{{ doc.format }}</dd>
                </div>
                <div>
                  <dt>版本</dt>
                  <dd>{{ doc.version }}</dd>
                </div>
                <div>
                  <dt>切片数</dt>
                  <dd>{{ doc.chunkCount }}</dd>
                </div>
                <div>
                  <dt>更新</dt>
                  <dd>{{ when(doc.updatedAt) }}</dd>
                </div>
              </dl>
            </li>
          </ul>

          <template v-if="hasCollections">
            <ServerNotice
              class="kb__cta"
              variant="info"
              message="资料准备好后，回到对话在输入框上方开启并使用它。"
            />
            <button
              type="button"
              class="gl-btn gl-btn--primary kb__go"
              data-test="goto-chat"
              @click="router.push({ name: 'chat' })"
            >
              <AppIcon name="chat" :size="15" />
              回到对话
            </button>
          </template>
        </section>
      </div>
    </SectionFrame>
  </div>
</template>

<style scoped>
.kb {
  display: grid;
  grid-template-columns: 250px minmax(0, 1fr);
  gap: 1rem;
  align-items: start;
}

.kb__side,
.kb__main {
  display: flex;
  flex-direction: column;
  gap: 0.6rem;
  min-width: 0;
}

.kb__side {
  padding: 0.75rem;
  background: var(--gl-surface);
  border: 1px solid var(--gl-surface-line);
  border-radius: var(--gl-radius);
}

.kb__head {
  display: flex;
  align-items: center;
  gap: 0.5rem;
}

.kb__head h2 {
  flex: 1;
  min-width: 0;
  font-size: 0.98rem;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.kb__create {
  display: flex;
  gap: 0.4rem;
}

.kb__input {
  flex: 1;
  min-width: 0;
  padding: 0.4rem 0.55rem;
  border: 1px solid var(--gl-surface-line);
  border-radius: var(--gl-radius-sm);
  background: var(--gl-surface-soft);
  font-size: 0.88rem;
}

.kb__list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 0.2rem;
}

.kb__row {
  display: flex;
  align-items: center;
  gap: 0.45rem;
  width: 100%;
  padding: 0.45rem 0.5rem;
  border: 1px solid transparent;
  border-radius: var(--gl-radius-sm);
  background: none;
  color: var(--gl-ink-soft);
  font-size: 0.9rem;
  text-align: left;
}

.kb__row:hover {
  background: var(--gl-surface-soft);
}

.kb__row--active {
  background: var(--gl-accent-soft);
  border-color: #bfdcd7;
  color: var(--gl-accent-ink);
  font-weight: 600;
}

.kb__row-title {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.kb__upload {
  cursor: pointer;
  user-select: none;
}

.kb__upload--off {
  opacity: 0.55;
  cursor: not-allowed;
  pointer-events: none;
}

/* Present but visually replaced: the label above is its visible face, and it
   stays focusable so the keyboard path still works. */
.kb__file {
  position: absolute;
  width: 1px;
  height: 1px;
  padding: 0;
  margin: -1px;
  overflow: hidden;
  clip-path: inset(50%);
  white-space: nowrap;
  border: 0;
}

.kb__hint {
  margin: 0;
  font-size: 0.8rem;
  color: var(--gl-ink-faint);
}

.kb__error {
  margin: 0;
  padding: 0.45rem 0.6rem;
  border-radius: var(--gl-radius-sm);
  background: var(--gl-danger-soft);
  color: var(--gl-danger);
  font-size: 0.85rem;
  font-weight: 600;
  overflow-wrap: anywhere;
}

.kb__busy,
.kb__empty {
  margin: 0;
  padding: 0.9rem 0.6rem;
  border: 1px dashed var(--gl-surface-line);
  border-radius: var(--gl-radius-sm);
  background: var(--gl-surface-soft);
  font-size: 0.88rem;
  color: var(--gl-ink-soft);
}

.kb__docs {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 0.5rem;
}

.kb__doc {
  padding: 0.7rem 0.85rem;
}

.kb__doc-head {
  display: flex;
  align-items: center;
  gap: 0.5rem;
}

.kb__doc-title {
  flex: 1;
  min-width: 0;
  margin: 0;
  font-size: 0.95rem;
  font-weight: 600;
  overflow-wrap: anywhere;
}

.kb__doc-remove {
  flex: none;
  font-size: 0.78rem;
  padding: 0.25em 0.55em;
}

.kb__doc-remove:hover:not(:disabled) {
  color: var(--gl-danger);
}

.kb__facts {
  display: flex;
  flex-wrap: wrap;
  gap: 0.3rem 1.4rem;
  margin: 0.45rem 0 0;
}

.kb__facts div {
  display: flex;
  align-items: baseline;
  gap: 0.35rem;
}

.kb__facts dt {
  font-size: 0.74rem;
  color: var(--gl-ink-faint);
}

.kb__facts dd {
  margin: 0;
  font-size: 0.82rem;
  font-family: var(--gl-mono);
  color: var(--gl-ink-soft);
}

.kb__cta {
  margin-top: 0.3rem;
}

.kb__go {
  align-self: flex-start;
}

@media (max-width: 820px) {
  .kb {
    grid-template-columns: minmax(0, 1fr);
  }
}
</style>
