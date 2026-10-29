<script setup>
import { onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import SectionFrame from '../components/SectionFrame.vue'
import ServerNotice from '../components/ServerNotice.vue'
import DocumentProcessingPanel from '../components/DocumentProcessingPanel.vue'
import { listV2Documents, getV2Document, uploadV2Document, updateV2Document, retryV2Document,
  reindexV2Document, deleteV2Document, V2_MAX_FILE_BYTES } from '../api/v2Documents.js'

const router = useRouter()
const documents = ref([])
const error = ref('')
const notice = ref('')
const busy = ref(false)
const detail = ref(null)
const processingOpen = ref({}) // document id -> panel expanded
function toggleProcessing(id) { processingOpen.value = { ...processingOpen.value, [id]: !processingOpen.value[id] } }
async function refresh() {
  try { documents.value = (await listV2Documents()).documents }
  catch (cause) { error.value = `${cause.message} (${cause.code})` }
}
async function perform(action, deleted = false) {
  if (busy.value) return
  busy.value = true; error.value = ''; notice.value = ''
  try {
    const result = await action()
    notice.value = deleted ? '删除请求已提交；文档将从原列表隐藏，后台清理结果尚未确认。'
      : result.unchanged ? '原实现返回 unchanged，未产生新版本。' : '请求已被原接口接受；以服务返回的实际索引状态为准。'
    await refresh()
  } catch (cause) { error.value = `${cause.message} (${cause.code})` }
  finally { busy.value = false }
}
function chooseFile(event, id) {
  const file = event.target.files?.[0]; event.target.value = ''
  if (!file) return
  if (file.size > V2_MAX_FILE_BYTES) { error.value = '原文档单文件上限为 5 MiB。'; return }
  return perform(() => id ? updateV2Document(id, file) : uploadV2Document(file))
}
async function inspect(id) {
  try { detail.value = (await getV2Document(id)).document }
  catch (cause) { error.value = `${cause.message} (${cause.code})` }
}
onMounted(refresh)
</script>

<template>
  <div class="gl-page">
    <SectionFrame title="V2 用户级文档" eyebrow="隔离迁移入口" icon="knowledge" status="原文档接口" status-tone="done"
      summary="这是原 V2 的用户级文档，不是 Collection。Collection 及普通 Chat 的知识范围适配尚未接通；Agent 保持原策略。">
      <p class="gl-muted">上传 .md/.txt，单文件上限 5 MiB（服务端 knowledgeConfig.maxFileSizeBytes 的默认值；改动后以服务端返回的提示为准）。提交成功不表示索引完成。更新使用原服务内部 CAS；没有客户端 expectedVersion 合同。</p>
      <ServerNotice v-if="error" :message="error" />
      <p v-if="notice" role="status">{{ notice }}</p>
      <button class="gl-btn" :disabled="busy" @click="refresh">刷新实际状态</button>
      <label>上传文档 <input type="file" accept=".md,.txt" :disabled="busy" data-test="v2-upload" @change="chooseFile($event)"></label>
      <ul>
        <li v-for="document in documents" :key="document.id" class="gl-card" data-test="v2-document">
          <h3>{{ document.original_name }}</h3>
          <p>文档 {{ document.id }} · 当前版本 {{ document.version }} · active_version {{ document.active_version }}</p>
          <p data-test="v2-status">{{ document.index_status }} · {{ document.file_size }} 字节</p>
          <p v-if="document.error_message">{{ document.error_message }}</p>
          <button class="gl-btn" :disabled="busy" @click="inspect(document.id)">详情</button>
          <button class="gl-btn" type="button" :aria-expanded="!!processingOpen[document.id]" data-test="v2-processing-toggle" @click="toggleProcessing(document.id)">处理详情</button>
          <button v-if="document.index_status === 'failed'" class="gl-btn" :disabled="busy" @click="perform(() => retryV2Document(document.id))">retry</button>
          <button v-if="document.index_status === 'ready'" class="gl-btn" :disabled="busy" @click="perform(() => reindexV2Document(document.id))">reindex</button>
          <label v-if="document.index_status === 'ready'">更新 <input type="file" accept=".md,.txt" :disabled="busy" @change="chooseFile($event, document.id)"></label>
          <button class="gl-btn" :disabled="busy" @click="perform(() => deleteV2Document(document.id), true)">提交删除</button>
          <button v-if="document.active_version > 0" type="button" class="gl-btn" @click="router.push({ name: 'v2-source', params: { documentId: document.id }, query: { version: String(document.active_version) } })">读取指定 active 原文</button>
          <DocumentProcessingPanel v-if="processingOpen[document.id]" :document-id="document.id" :document-status="document.index_status" />
        </li>
      </ul>
      <pre v-if="detail" data-test="v2-detail">{{ JSON.stringify(detail, null, 2) }}</pre>
    </SectionFrame>
  </div>
</template>
