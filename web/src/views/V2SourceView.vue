<script setup>
import { ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import SectionFrame from '../components/SectionFrame.vue'
import ServerNotice from '../components/ServerNotice.vue'
import { readV2Source } from '../api/v2Documents.js'
const route = useRoute()
const source = ref(null)
const error = ref('')
let generation = 0
watch(() => route.fullPath, async () => {
  const current = ++generation; source.value = null; error.value = ''
  try {
    const result = await readV2Source(route.params.documentId, route.query.version,
      route.query.start_line, route.query.end_line)
    if (current === generation) source.value = result
  } catch (cause) { if (current === generation) error.value = `${cause.message} (${cause.code})` }
}, { immediate: true })
</script>
<template>
  <div class="gl-page">
    <SectionFrame title="授权原文" eyebrow="指定文档版本" status="原 SourceReader" status-tone="done"
      summary="只读指定 active 版本，不以最新版本或 snippet 替代。旧版本不可读、缺失或越权会明确失败。">
      <ServerNotice v-if="error" :message="error" />
      <template v-if="source">
        <p>{{ source.document_name }} · {{ source.document_id }} · 版本 {{ source.document_version }}</p>
        <pre v-for="line in source.lines" :key="line.number" :id="`L${line.number}`">L{{ line.number }}: {{ line.text }}</pre>
      </template>
    </SectionFrame>
  </div>
</template>
