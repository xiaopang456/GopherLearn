<script setup>
defineProps({
  result: { type: Object, required: true },
  runId: { type: String, default: '' },
  messageId: { type: String, default: '' },
})
</script>

<template>
  <details data-test="agent-result">
    <summary>Agent 结果{{ result.replayed ? '（原请求重放）' : '' }}</summary>
    <p>Run：{{ runId }} · 消息：{{ messageId }}</p>
    <p>回答依据：{{ result.answerBasis }} · 知识状态：{{ result.knowledgeStatus }}</p>
    <p v-if="result.knowledgeReason">知识说明：{{ result.knowledgeReason }}</p>
    <p v-if="result.memoryDraft">原记忆草稿已返回；通过独立 V2 卡片处理。旧 clean 手写与卡片合同尚未无损迁移。</p>
    <details>
      <summary>原 V2 公开结果</summary>
      <pre>{{ JSON.stringify(result, null, 2) }}</pre>
    </details>
  </details>
</template>
