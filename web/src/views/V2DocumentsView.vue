<script setup>
import { onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { NButton } from 'naive-ui'

import DocumentProcessingPanel from '../components/DocumentProcessingPanel.vue'
import GlEmpty from '../components/GlEmpty.vue'
import GlPageHeader from '../components/GlPageHeader.vue'
import GlStatusTag from '../components/GlStatusTag.vue'
import ServerNotice from '../components/ServerNotice.vue'
import { recordDevIssue } from '../lib/devlog.js'
import {
  listV2Documents,
  uploadV2Document,
  updateV2Document,
  retryV2Document,
  reindexV2Document,
  deleteV2Document,
  V2_MAX_FILE_BYTES,
} from '../api/v2Documents.js'

const router = useRouter()
const documents = ref([])
const error = ref('')
const notice = ref('')
const busy = ref(false)
const processingOpen = ref({}) // document id -> panel expanded
const fileInput = ref(null)
const updateId = ref('')

function toggleProcessing(id) {
  processingOpen.value = { ...processingOpen.value, [id]: !processingOpen.value[id] }
}

async function refresh() {
  try {
    documents.value = (await listV2Documents()).documents
  } catch (cause) {
    error.value = cause.message
    recordDevIssue('documents.list_failed', { code: cause.code })
  }
}

async function perform(action, deleted = false) {
  if (busy.value) return
  busy.value = true
  error.value = ''
  notice.value = ''
  try {
    const result = await action()
    notice.value = deleted
      ? '删除请求已提交，文档会从列表中消失。'
      : result?.unchanged
        ? '没有产生新版本。'
        : '请求已接受，实际状态以下方列表为准。'
    await refresh()
  } catch (cause) {
    error.value = cause.message
    recordDevIssue('documents.action_failed', { code: cause.code })
  } finally {
    busy.value = false
  }
}

function size(bytes) {
  const value = Number(bytes ?? 0)
  if (!value) return '0 字节'
  if (value < 1024) return `${value} 字节`
  return `${(value / 1024).toFixed(1)} KB`
}

function when(dateString) {
  if (!dateString) return ''
  const stamp = Date.parse(dateString)
  if (Number.isNaN(stamp)) return ''
  return new Intl.DateTimeFormat('zh-CN', {
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
  }).format(new Date(stamp))
}

/**
 * One hidden file input serves both "upload" and "update": the visible
 * control is a button, so the page never shows the browser's own
 * "选择文件 未选择文件" control next to the app's buttons.
 */
function pick(updateTarget = '') {
  updateId.value = updateTarget
  fileInput.value?.click()
}

function chosen(event) {
  const file = event.target.files?.[0]
  event.target.value = ''
  const target = updateId.value
  updateId.value = ''
  if (!file) return
  if (file.size > V2_MAX_FILE_BYTES) {
    error.value = '单个文件不能超过 5 MiB。'
    return
  }
  return perform(() => (target ? updateV2Document(target, file) : uploadV2Document(file)))
}

function openSource(document) {
  router.push({
    name: 'v2-source',
    params: { documentId: document.id },
    query: { version: String(document.active_version) },
  })
}

onMounted(refresh)
</script>

<template>
  <div class="gl-page docs">
    <GlPageHeader
      title="文档"
      description="上传 .md 或 .txt 之后，回答就能引用它们的内容。单文件不超过 5 MiB。"
    >
      <template #actions>
        <n-button type="primary" :disabled="busy" @click="pick()">上传文档</n-button>
      </template>
    </GlPageHeader>

    <ServerNotice v-if="error" :message="error" />
    <p v-if="notice" class="docs__notice" role="status">{{ notice }}</p>

    <input
      ref="fileInput"
      class="sr-only"
      type="file"
      accept=".md,.txt"
      :disabled="busy"
      data-test="v2-upload"
      @change="chosen"
    />

    <GlEmpty
      v-if="documents.length === 0"
      description="还没有文档。上传一个 .md 或 .txt 文件后，回答就能引用它的内容。"
    >
      <template #action>
        <n-button type="primary" :disabled="busy" @click="pick()">上传文档</n-button>
      </template>
    </GlEmpty>

    <!-- One document per turn, in the same flat shape as a conversation:
         a name, a state, a line of facts, and the actions under a hairline. -->
    <ul v-else class="docs__list">
      <li v-for="document in documents" :key="document.id" class="docs__item" data-test="v2-document">
        <div class="docs__head">
          <h2 class="docs__name">{{ document.original_name }}</h2>
          <GlStatusTag :status="document.index_status" data-test="v2-status" />
        </div>

        <p class="docs__meta">
          版本 {{ document.active_version }} · {{ size(document.file_size) }}
          <span v-if="when(document.updated_at)"> · {{ when(document.updated_at) }}</span>
        </p>

        <p v-if="document.error_message" class="docs__error">{{ document.error_message }}</p>

        <div class="docs__actions">
          <n-button
            v-if="document.active_version > 0"
            size="small"
            secondary
            :disabled="busy"
            @click="openSource(document)"
          >
            查看原文
          </n-button>
          <n-button
            v-if="document.index_status === 'failed'"
            size="small"
            quaternary
            :disabled="busy"
            @click="perform(() => retryV2Document(document.id))"
          >
            重试
          </n-button>
          <n-button
            v-if="document.index_status === 'ready'"
            size="small"
            quaternary
            :disabled="busy"
            @click="perform(() => reindexV2Document(document.id))"
          >
            重新索引
          </n-button>
          <n-button
            v-if="document.index_status === 'ready'"
            size="small"
            quaternary
            :disabled="busy"
            @click="pick(document.id)"
          >
            更新
          </n-button>
          <n-button
            size="small"
            quaternary
            type="error"
            :disabled="busy"
            @click="perform(() => deleteV2Document(document.id), true)"
          >
            删除
          </n-button>
          <n-button
            size="small"
            text
            :disabled="busy"
            data-test="v2-processing-toggle"
            :aria-expanded="String(!!processingOpen[document.id])"
            @click="toggleProcessing(document.id)"
          >
            处理详情
          </n-button>
        </div>

        <DocumentProcessingPanel
          v-if="processingOpen[document.id]"
          class="docs__panel"
          :document-id="document.id"
          :document-status="document.index_status"
        />
      </li>
    </ul>
  </div>
</template>

<style scoped>
/* The same reading column the transcript uses: one document per turn, the
   actions under a hairline, nothing boxed in. */
.docs {
  max-width: 760px;
}

.docs__notice {
  margin: 0 0 var(--gl-space-4);
  padding: var(--gl-space-2) var(--gl-space-3);
  border-radius: var(--gl-radius-sm);
  background: var(--gl-accent-soft);
  color: var(--gl-accent-ink);
  font-size: var(--gl-text-sm);
}

.docs__list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: var(--gl-space-5);
}

.docs__item {
  display: flex;
  flex-direction: column;
  gap: var(--gl-space-2);
  min-width: 0;
}

.docs__head {
  display: flex;
  align-items: center;
  gap: var(--gl-space-3);
  min-width: 0;
}

.docs__name {
  flex: 1;
  min-width: 0;
  font-size: var(--gl-text-lg);
  font-weight: 600;
  letter-spacing: -0.01em;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.docs__meta {
  margin: 0;
  font-size: var(--gl-text-sm);
  color: var(--gl-ink-faint);
}

.docs__error {
  margin: 0;
  font-size: var(--gl-text-sm);
  color: var(--gl-danger);
}

.docs__actions {
  display: flex;
  flex-wrap: wrap;
  gap: var(--gl-space-1);
  padding-top: var(--gl-space-2);
  border-top: 1px solid var(--gl-surface-line);
}

.docs__panel {
  margin-top: var(--gl-space-2);
}
</style>
