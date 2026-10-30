<script setup>
import { onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { NButton } from 'naive-ui'

import GlEmpty from '../components/GlEmpty.vue'
import GlPageHeader from '../components/GlPageHeader.vue'
import ServerNotice from '../components/ServerNotice.vue'
import { useV2Learning } from '../composables/useV2Learning.js'

const page = useV2Learning()
const topic = ref('')
const answer = ref('')
const score = ref('')
const reason = ref('')

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
  return {
    name: 'v2-source',
    params: { documentId: source.documentId },
    query: { version: String(source.documentVersion), start_line: String(source.startLine), end_line: String(source.endLine) },
  }
}

const STATUS_TEXT = {
  created: '刚创建',
  asking: '出题中',
  waiting_answer: '等你作答',
  completed: '已完成',
  cancelled: '已取消',
  expired: '已过期',
}

const LEVEL_TEXT = {
  insufficient: '尚不足',
  basic: '入门',
  intermediate: '进阶',
  advanced: '熟练',
}

onMounted(page.loadSessions)
onBeforeUnmount(page.dispose)
</script>

<template>
  <div class="gl-page learning">
    <GlPageHeader
      title="学习／测验"
      description="在你的资料上出题、作答，成绩会累计到每个主题的掌握度上。已经结束、取消或过期的测验不能恢复，只能继续还没结束的那一次。"
    />

    <ServerNotice v-if="page.error.value" :message="page.error.value.message" :code="page.error.value.code" />

    <p v-if="page.uncertain.value" class="learning__uncertain" role="status" data-test="uncertain">
      服务端状态未确认；以下仅为上次确认的结果，请查询当前任务。
    </p>

    <form class="learning__bar" data-test="start-form" @submit.prevent="page.perform('start', { topic })">
      <label class="gl-label learning__session">
        <span class="gl-label__name">会话</span>
        <select data-test="session" :value="page.sessionId.value" class="gl-field" @change="select">
          <option value="">请选择学习 Agent 会话</option>
          <option v-for="session in page.sessions.value" :key="session.id" :value="session.id">
            {{ session.title }}
          </option>
        </select>
      </label>

      <label class="gl-label learning__topic">
        <span class="gl-label__name">主题</span>
        <input v-model="topic" data-test="topic" required class="gl-field" placeholder="例如：发布说明">
      </label>

      <div class="learning__bar-actions">
        <n-button
          type="primary"
          attr-type="submit"
          :disabled="page.busy.value || !page.sessionId.value || !topic.trim()"
        >
          生成测验
        </n-button>
        <n-button
          type="button"
          :disabled="page.busy.value || !page.sessionId.value"
          data-test="current"
          @click="page.perform('current')"
        >
          继续上次测验
        </n-button>
        <n-button
          type="button"
          :disabled="page.busy.value || !topic.trim()"
          data-test="progress"
          @click="page.perform('progress', { topic })"
        >
          查看掌握度
        </n-button>
      </div>

      <div class="learning__bar-actions">
        <n-button v-if="page.busy.value" attr-type="button" data-test="abort" @click="page.abort">
          取消本次请求
        </n-button>
        <n-button v-if="page.pending.value" attr-type="button" data-test="retry" :disabled="page.busy.value" @click="page.retry">
          重试上一次操作
        </n-button>
      </div>
    </form>

    <GlEmpty
      v-if="!page.task.value && !page.attempt.value && !page.progress.value"
      class="learning__empty"
      description="还没有测验。选一个学习 Agent 会话，再写一个主题，就能生成一份只考你资料里内容的题。"
    />

    <section v-if="page.task.value" class="learning__card gl-card" data-test="task">
      <header class="learning__card-head">
        <h2 class="learning__card-title">
          第 {{ page.task.value.currentQuestion }} / {{ page.task.value.totalQuestions }} 题
        </h2>
        <span class="learning__chip">{{ STATUS_TEXT[page.task.value.status] ?? page.task.value.status }}</span>
        <span class="learning__chip learning__chip--quiet">{{ page.task.value.topic }}</span>
      </header>

      <template v-if="page.task.value.question">
        <p class="learning__question">{{ page.task.value.question.question }}</p>

        <ul v-if="page.task.value.question.sources?.length" class="learning__sources">
          <li v-for="source in page.task.value.question.sources" :key="`${source.documentId}:${source.documentVersion}:${source.startLine}:${source.endLine}`">
            <RouterLink data-test="source" :to="sourceRoute(source)">
              {{ source.documentName }} · 第 {{ source.startLine }}–{{ source.endLine }} 行
              <span v-if="source.titlePath?.length">· {{ source.titlePath.join(' / ') }}</span>
            </RouterLink>
          </li>
        </ul>

        <form data-test="answer-form" @submit.prevent="page.perform('answer', { answer })">
          <label class="gl-label">
            <span class="gl-label__name">你的回答</span>
            <textarea v-model="answer" data-test="answer" required class="gl-field" placeholder="写下你的答案" />
          </label>
          <div class="learning__actions">
            <n-button
              type="primary"
              attr-type="submit"
              :disabled="page.busy.value || page.task.value.status !== 'waiting_answer' || !answer.trim()"
            >
              提交答案
            </n-button>
            <n-button
              type="button"
              :disabled="page.busy.value || page.task.value.status !== 'waiting_answer'"
              data-test="skip"
              @click="page.perform('action', { action: 'skip' })"
            >
              跳过这道题
            </n-button>
          </div>
        </form>
      </template>

      <div class="learning__actions">
        <n-button attr-type="button" :disabled="page.busy.value" data-test="cancel" @click="page.perform('cancel')">
          结束这次测验
        </n-button>
      </div>
    </section>

    <section v-if="page.attempt.value" class="learning__card gl-card" data-test="attempt">
      <header class="learning__card-head">
        <h2 class="learning__card-title">这次的表现</h2>
        <span class="learning__chip">得分 {{ page.attempt.value.effectiveTotal }}</span>
      </header>

      <div class="learning__scores">
        <div class="learning__score">
          <span class="learning__score-label">正确性</span>
          <span class="learning__score-value">{{ page.attempt.value.correctness }}<small>/60</small></span>
        </div>
        <div class="learning__score">
          <span class="learning__score-label">完整度</span>
          <span class="learning__score-value">{{ page.attempt.value.completeness }}<small>/25</small></span>
        </div>
        <div class="learning__score">
          <span class="learning__score-label">清晰度</span>
          <span class="learning__score-value">{{ page.attempt.value.clarity }}<small>/15</small></span>
        </div>
      </div>

      <p class="learning__feedback">{{ page.attempt.value.feedback }}</p>

      <div v-if="page.attempt.value.missingPoints?.length" class="learning__missing">
        <p class="learning__missing-title">还差一点的地方</p>
        <ul>
          <li v-for="point in page.attempt.value.missingPoints" :key="point">{{ point }}</li>
        </ul>
      </div>

      <form class="learning__correct" data-test="correct-form" @submit.prevent="correct">
        <label class="gl-label">
          <span class="gl-label__name">修正分数（0–100）</span>
          <input v-model="score" data-test="score" type="number" min="0" max="100" step="1" required class="gl-field">
        </label>
        <label class="gl-label">
          <span class="gl-label__name">修改理由</span>
          <input v-model="reason" data-test="reason" required class="gl-field" placeholder="为什么改这个分数">
        </label>
        <n-button type="primary" attr-type="submit" :disabled="page.busy.value">提交修正</n-button>
      </form>
    </section>

    <section v-if="page.progress.value" class="learning__card gl-card" data-test="mastery">
      <header class="learning__card-head">
        <h2 class="learning__card-title">主题掌握度</h2>
        <span class="learning__chip">{{ page.progress.value.topic }}</span>
      </header>

      <div class="learning__mastery">
        <span class="learning__mastery-value" data-test="mastery-score">{{ page.progress.value.mastery_score }}</span>
        <span class="learning__mastery-label">
          掌握度 · {{ LEVEL_TEXT[page.progress.value.level] ?? page.progress.value.level }}
          · {{ page.progress.value.valid_attempts }} 次有效作答
        </span>
      </div>

      <div v-if="page.progress.value.weak_points?.length" class="learning__missing">
        <p class="learning__missing-title">薄弱的地方</p>
        <ul>
          <li v-for="point in page.progress.value.weak_points" :key="point">{{ point }}</li>
        </ul>
      </div>
    </section>
  </div>
</template>

<style scoped>
.learning {
  max-width: 860px;
}

.learning__uncertain {
  margin: 0 0 var(--gl-space-3);
  padding: var(--gl-space-2) var(--gl-space-3);
  border-radius: var(--gl-radius-sm);
  background: var(--gl-warn-soft);
  color: var(--gl-warn);
  font-size: var(--gl-text-sm);
}

.learning__bar {
  display: grid;
  grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
  gap: var(--gl-space-3);
  margin-bottom: var(--gl-space-5);
  padding-bottom: var(--gl-space-4);
  border-bottom: 1px solid var(--gl-surface-line);
}

.learning__bar-actions {
  display: flex;
  flex-wrap: wrap;
  align-items: flex-end;
  gap: var(--gl-space-2);
}

.learning__card {
  margin-bottom: var(--gl-space-4);
  padding: var(--gl-space-5);
}

.learning__empty {
  margin-top: var(--gl-space-6);
}

.learning__card-head {
  display: flex;
  align-items: center;
  gap: var(--gl-space-2);
  margin-bottom: var(--gl-space-3);
}

.learning__card-title {
  flex: 1;
  font-size: var(--gl-text-lg);
  font-weight: 600;
  letter-spacing: -0.01em;
}

.learning__chip {
  padding: 0.1em 0.55em;
  border-radius: var(--gl-radius-pill);
  background: var(--gl-accent-soft);
  color: var(--gl-accent-ink);
  font-size: var(--gl-text-xs);
  font-weight: 600;
}

.learning__chip--quiet {
  background: var(--gl-surface-soft);
  border: 1px solid var(--gl-surface-line);
  color: var(--gl-ink-soft);
}

.learning__question {
  font-size: var(--gl-text-md);
  line-height: var(--gl-leading-relaxed);
  color: var(--gl-ink);
}

.learning__sources {
  list-style: none;
  margin: var(--gl-space-3) 0 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: var(--gl-space-1);
}

.learning__sources a {
  font-size: var(--gl-text-sm);
  color: var(--gl-accent-ink);
  text-decoration: underline;
  text-underline-offset: 2px;
}

.learning__actions {
  display: flex;
  flex-wrap: wrap;
  gap: var(--gl-space-2);
  margin-top: var(--gl-space-3);
}

.learning__scores {
  display: flex;
  gap: var(--gl-space-5);
  padding-bottom: var(--gl-space-4);
  border-bottom: 1px solid var(--gl-surface-line);
}

.learning__score {
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.learning__score-label {
  font-size: var(--gl-text-xs);
  color: var(--gl-ink-faint);
}

.learning__score-value {
  font-size: var(--gl-text-xl);
  font-weight: 600;
  letter-spacing: -0.02em;
}

.learning__score-value small {
  font-size: var(--gl-text-sm);
  font-weight: 400;
  color: var(--gl-ink-faint);
}

.learning__feedback {
  margin-top: var(--gl-space-4);
  font-size: var(--gl-text-base);
  line-height: var(--gl-leading-normal);
}

.learning__missing {
  margin-top: var(--gl-space-3);
}

.learning__missing-title {
  font-size: var(--gl-text-sm);
  font-weight: 600;
  color: var(--gl-ink-soft);
}

.learning__missing ul {
  margin: var(--gl-space-1) 0 0;
  padding-left: 1.1rem;
  font-size: var(--gl-text-sm);
  color: var(--gl-ink-soft);
}

.learning__correct {
  display: flex;
  flex-wrap: wrap;
  align-items: flex-end;
  gap: var(--gl-space-3);
  margin-top: var(--gl-space-4);
  padding-top: var(--gl-space-4);
  border-top: 1px solid var(--gl-surface-line);
}

.learning__correct .gl-label {
  width: 200px;
}

.learning__mastery {
  display: flex;
  align-items: baseline;
  gap: var(--gl-space-3);
}

.learning__mastery-value {
  font-size: var(--gl-text-xl);
  font-weight: 600;
  letter-spacing: -0.02em;
}

.learning__mastery-label {
  font-size: var(--gl-text-sm);
  color: var(--gl-ink-faint);
}

@media (max-width: 700px) {
  .learning__bar {
    grid-template-columns: minmax(0, 1fr);
  }

  .learning__correct .gl-label {
    width: 100%;
  }
}
</style>
