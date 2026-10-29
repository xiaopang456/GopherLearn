<script setup>
import { ref, onMounted, onBeforeUnmount, watch } from 'vue'
import SectionFrame from '../components/SectionFrame.vue'
import ServerNotice from '../components/ServerNotice.vue'
import { useV2Learning } from '../composables/useV2Learning.js'
const page = useV2Learning()
const topic = ref(''), answer = ref(''), score = ref(''), reason = ref('')
watch(() => page.task.value?.question?.id, () => { answer.value = '' })
watch(() => page.sessionId.value, () => { topic.value = ''; score.value = ''; reason.value = '' })
watch(() => page.attempt.value?.id, () => { score.value = ''; reason.value = '' })
function select(event) { page.selectSession(event.target.value) }
function correct() {
  if (!page.attempt.value || score.value === '' || !reason.value.trim()) return
  const effectiveTotal = Number(score.value) // Score only; identities stay strings.
  if (!Number.isInteger(effectiveTotal) || effectiveTotal < 0 || effectiveTotal > 100) return
  return page.perform('correct', { attemptId: page.attempt.value.id, effectiveTotal, reason: reason.value })
}
function sourceRoute(source) {
  return { name: 'v2-source', params: { documentId: source.documentId },
    query: { version: String(source.documentVersion), start_line: String(source.startLine), end_line: String(source.endLine) } }
}
onMounted(page.loadSessions)
onBeforeUnmount(page.dispose)
</script>
<template>
  <div class="gl-page">
    <SectionFrame title="V2 学习／测验" eyebrow="原 native 合同" icon="learning" status="独立入口" status-tone="done"
      summary="旧学习页及白名单仍保留；这里展示原 V2 题目、判分与掌握度。Collection 范围未接通，不使用学习 SSE。">
      <p class="gl-muted">current 仅恢复活动任务；完成、取消、过期后不能恢复完整历史、旧题或成绩。Start 没有幂等键，失败或取消后请先查询 current，不自动重新创建。</p>
      <ServerNotice v-if="page.error.value" :message="page.error.value.message" :code="page.error.value.code" />
      <p v-if="page.uncertain.value" role="status" data-test="uncertain">服务端状态未确认；以下仅为上次确认的结果，请查询 current。</p>
      <form class="learning-form" data-test="start-form" @submit.prevent="page.perform('start', { topic })">
        <label>真实 Agent 会话
          <select data-test="session" :value="page.sessionId.value" @change="select">
            <option value="">请选择 learning_agent 会话</option>
            <option v-for="session in page.sessions.value" :key="session.id" :value="session.id">{{ session.title }} · {{ session.id }}</option>
          </select>
        </label>
        <label>明确主题 <input v-model="topic" data-test="topic" required /></label>
        <button class="gl-btn" :disabled="page.busy.value || !page.sessionId.value || !topic.trim()">生成原测验</button>
        <button type="button" class="gl-btn" data-test="current" :disabled="page.busy.value || !page.sessionId.value" @click="page.perform('current')">查询／恢复活动任务</button>
        <button type="button" class="gl-btn" data-test="progress" :disabled="page.busy.value || !topic.trim()" @click="page.perform('progress', { topic })">查询原掌握度</button>
        <button v-if="page.busy.value" type="button" class="gl-btn" data-test="abort" @click="page.abort">取消本次请求（不等于取消任务）</button>
        <button v-if="page.pending.value" type="button" class="gl-btn" data-test="retry" :disabled="page.busy.value" @click="page.retry">明确重试原提交（原 ID 和完整请求）</button>
      </form>
      <section v-if="page.task.value" data-test="task">
        <p>{{ page.task.value.topic }} · {{ page.task.value.status }} · {{ page.task.value.id }}</p>
        <p>topicKey {{ page.task.value.topicKey }} · runId {{ page.task.value.runId }} · sessionId {{ page.task.value.sessionId }}</p>
        <p>原进度 {{ page.task.value.currentQuestion }} / {{ page.task.value.totalQuestions }} · 有效期 {{ page.task.value.expiresAt }}</p>
        <template v-if="page.task.value.question">
          <h3>{{ page.task.value.question.question }}</h3>
          <p>{{ page.task.value.question.type }} · {{ page.task.value.question.id }}</p>
          <ul>
            <li v-for="source in page.task.value.question.sources" :key="`${source.documentId}:${source.documentVersion}:${source.startLine}:${source.endLine}`">
              <RouterLink data-test="source" :to="sourceRoute(source)">{{ source.documentName }} · {{ source.documentId }} · v{{ source.documentVersion }} · L{{ source.startLine }}–{{ source.endLine }} · {{ source.titlePath?.join(' / ') }}</RouterLink>
            </li>
          </ul>
          <form data-test="answer-form" @submit.prevent="page.perform('answer', { answer })">
            <label>答案 <textarea v-model="answer" data-test="answer" required /></label>
            <button class="gl-btn" :disabled="page.busy.value || page.task.value.status !== 'waiting_answer' || !answer.trim()">提交原判分</button>
          </form>
          <button class="gl-btn" data-test="skip" :disabled="page.busy.value || page.task.value.status !== 'waiting_answer'" @click="page.perform('action', { action: 'skip' })">跳过当前题</button>
        </template>
        <button class="gl-btn" data-test="cancel" :disabled="page.busy.value" @click="page.perform('cancel')">请求原取消任务</button>
      </section>
      <section v-if="page.attempt.value" data-test="attempt">
        <h3>原答题结果</h3>
        <p>{{ page.attempt.value.id }} · {{ page.attempt.value.status }} · {{ page.attempt.value.submissionId }} · questionId {{ page.attempt.value.questionId }}</p>
        <p>correctness {{ page.attempt.value.correctness }} · completeness {{ page.attempt.value.completeness }} · clarity {{ page.attempt.value.clarity }}</p>
        <p>autoTotal {{ page.attempt.value.autoTotal }} · effectiveTotal {{ page.attempt.value.effectiveTotal }} · confidence {{ page.attempt.value.confidence }}</p>
        <p>{{ page.attempt.value.feedback }}</p>
        <ul><li v-for="point in page.attempt.value.missingPoints" :key="point">{{ point }}</li></ul>
        <p v-if="page.attempt.value.correctionReason">{{ page.attempt.value.correctionReason }}</p>
        <form data-test="correct-form" @submit.prevent="correct">
          <label>明确有效分数（0–100） <input v-model="score" data-test="score" type="number" min="0" max="100" step="1" required /></label>
          <label>纠正原因 <input v-model="reason" data-test="reason" required /></label>
          <button class="gl-btn" :disabled="page.busy.value">提交原纠正</button>
        </form>
      </section>
      <section v-if="page.progress.value" data-test="mastery">
        <h3>原 TopicProgress</h3>
        <p>{{ page.progress.value.topic }} · {{ page.progress.value.topic_key }} · {{ page.progress.value.id }}</p>
        <p>mastery_score {{ page.progress.value.mastery_score }} · confidence {{ page.progress.value.confidence }} · valid_attempts {{ page.progress.value.valid_attempts }} · level {{ page.progress.value.level }}</p>
        <ul><li v-for="point in page.progress.value.weak_points" :key="point">{{ point }}</li></ul>
      </section>
    </SectionFrame>
  </div>
</template>
<style scoped>
.learning-form { display: flex; flex-wrap: wrap; gap: 12px; align-items: end; }
label { display: grid; gap: 6px; margin: 10px 0; }
input, select, textarea { padding: 8px; color: inherit; background: var(--gl-surface); border: 1px solid var(--gl-surface-line); border-radius: var(--gl-radius-sm); }
textarea { width: 100%; min-height: 100px; }
section { margin-top: 22px; border-top: 1px solid var(--gl-surface-line); padding-top: 14px; overflow-wrap: anywhere; }
</style>
