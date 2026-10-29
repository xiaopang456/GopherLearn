<script setup>
import { computed, nextTick, onBeforeUnmount, onMounted, ref } from 'vue'

import AppIcon from '../components/AppIcon.vue'
import BaselineSurfaceNotice from '../components/BaselineSurfaceNotice.vue'
import SectionFrame from '../components/SectionFrame.vue'
import ServerNotice from '../components/ServerNotice.vue'
import { listSessions, normalizeSession, readList } from '../api/sessions.js'
import { safeErrorMessage } from '../api/http.js'
import { isOpenStatus, questionTypeLabel } from '../api/learning.js'
import { LearningPhase, learningPhaseTitle } from '../lib/learningRun.js'
import { SessionMode } from '../lib/modes.js'
import { useLearning } from '../composables/useLearning.js'

/**
 * The practice page: one conversation, one topic, three questions at a time.
 *
 * The copy rules here are not stylistic. The contract publishes no feedback, no
 * per-question score and no verdict word — what a client may report after an accepted
 * answer is *that it was counted* — so nothing on this screen prints a number, a grade
 * or a comment about an answer, and the tests keep it that way even when a response
 * carries those fields.
 *
 * The countdown only ever stops the buttons. Whether a quiz expired is the store's
 * decision on read, so the page says "refresh and see" instead of declaring it.
 */

const learning = useLearning()

const sessions = ref([])
const sessionsError = ref('')
const sessionsLoading = ref(false)
let clock = null

const phase = computed(() => learning.phase.value)
const view = computed(() => learning.view.value)
const question = computed(() => learning.question.value)
const busy = computed(() => learning.work.value !== null)
const resuming = computed(() => learning.resumeState.value === 'loading')
const typeLabel = computed(() => questionTypeLabel(question.value?.type))
const canSend = computed(() => learning.canAct.value && learning.draft.value.trim() !== '')
const progressIsFresh = computed(
  () => learning.progress.value !== null && learning.progress.value !== 'absent',
)
const headline = computed(() => learningPhaseTitle(phase.value))
const positionText = computed(() => {
  const current = view.value
  if (!current || current.totalQuestions < 1) return ''
  return `第 ${current.currentQuestion} / ${current.totalQuestions} 题`
})

/** A status this build does not know gets its own words instead of an invented one. */
const statusLabel = computed(() => {
  const status = view.value?.status ?? ''
  const known = {
    created: '已排好，正在准备',
    asking: '正在出题',
    waiting_answer: '等你作答',
    grading: '正在评分',
    completed: '已完成',
    cancelled: '已结束',
    expired: '已超时',
  }
  return Object.prototype.hasOwnProperty.call(known, status) ? known[status] : '状态未识别'
})

/**
 * Only a learning-agent conversation can hold a quiz: the server refuses a start on a
 * plain chat one with 400 invalid_request, because the mastery record a quiz produces is
 * read back by the agent turn and by nothing else. The picker therefore lists what can
 * actually be started on, rather than offering conversations that will be refused.
 *
 * This is a usability filter, not the rule. The backend check stays, and a client that
 * names a chat conversation is still refused.
 */
const agentSessions = computed(() => sessions.value.filter((item) => item.mode === SessionMode.LearningAgent))
const noAgentSession = computed(() => !sessionsLoading.value && agentSessions.value.length === 0)

/**
 * The topic-memory consent belongs to exactly one conversation, so the switch is only
 * offered for a conversation that can hold one.
 *
 * `agentSessions` is what makes that check rather than an assumption: the picker is
 * already filtered, so a plain chat conversation cannot be selected here — but a
 * selection carried over from an earlier visit, or from another tab, can name anything.
 * Reading the stored answer and offering to change it are therefore both gated on the
 * same condition, and nothing renders as usable until the server has said which
 * conversation this is.
 */
const topicMemorySession = computed(() => {
  const id = learning.sessionId.value
  if (id === '') return false
  return agentSessions.value.some((item) => item.id === id)
})
/** Enabled only when the value on screen came from the server and no write is running. */
const canToggleTopicMemory = computed(
  () =>
    topicMemorySession.value &&
    learning.topicMemory.value !== null &&
    !learning.topicMemoryLoading.value &&
    !learning.topicMemorySaving.value,
)
const topicMemoryStateText = computed(() => {
  if (learning.topicMemorySaving.value) return '正在保存…'
  if (learning.topicMemoryLoading.value) return '正在读取这段对话当前的设置…'
  if (learning.topicMemory.value === null) return '还读不到这段对话当前的设置。'
  return learning.topicMemory.value === true ? '已允许' : '未允许'
})

/**
 * A quiz that the server still considers open. A *terminal* quiz is not an open one,
 * so after a cancel or a finish the topic field unlocks and a new round can start —
 * the contract keeps terminal quizzes out of the "already active" refusal.
 */
const openQuiz = computed(() => !!view.value && isOpenStatus(view.value.status))
/** The topic field is only editable when no open card is on screen. */
const topicLocked = computed(() => openQuiz.value || busy.value)
const canStart = computed(
  () => !topicLocked.value && !!learning.sessionId.value && learning.topic.value.trim() !== '',
)

onMounted(async () => {
  await loadSessions()
  // A selection carried over from an earlier visit may name a conversation this page can
  // no longer start a quiz on. Dropping it is what keeps the screen honest: the picker and
  // the request can then not disagree about which conversation is being practised.
  if (
    learning.sessionId.value &&
    !agentSessions.value.some((item) => item.id === learning.sessionId.value)
  ) {
    learning.closeSession()
  }
  // The store may already hold this conversation's quiz, so a mount only picks a
  // default when there is nothing addressed yet.
  if (!learning.sessionId.value && agentSessions.value.length > 0) {
    await learning.openSession(agentSessions.value[0].id)
  } else if (learning.sessionId.value) {
    // Coming back to this tab re-reads the truth quietly: a quiz can have expired or
    // been answered elsewhere while the page was gone, and a cached card would
    // offer actions the server already refused. The consent is re-read for the same
    // reason — a switch carried over from localStorage would be a decision nobody
    // confirmed since this tab was last open.
    await learning.resume({ silent: true })
    await learning.loadTopicMemory()
  }
  if (view.value) startClock()
})

onBeforeUnmount(stopClock)

async function loadSessions() {
  sessionsLoading.value = true
  sessionsError.value = ''
  try {
    const payload = await listSessions()
    sessions.value = readList(payload, 'sessions')
      .map(normalizeSession)
      .filter((item) => item.id !== '')
  } catch (caught) {
    sessionsError.value = safeErrorMessage(caught, '对话列表加载失败，可以刷新这一页再试。')
  } finally {
    sessionsLoading.value = false
  }
}

function startClock() {
  if (clock !== null) return
  learning.tick(Date.now())
  clock = setInterval(() => learning.tick(Date.now()), 1000)
}

function stopClock() {
  if (clock === null) return
  clearInterval(clock)
  clock = null
}

/** The picker is uncontrolled on purpose: the store is what re-renders it. */
async function chooseSession(event) {
  stopClock()
  await learning.openSession(String(event.target.value ?? ''))
  if (view.value) startClock()
}

async function startQuiz() {
  await learning.start()
  if (view.value) startClock()
}

async function refreshQuiz() {
  await learning.resume()
  if (view.value) startClock()
}

async function endQuiz() {
  stopClock()
  await learning.cancel()
}

/**
 * The switch reports the server's answer, never the click.
 *
 * `saveTopicMemory` takes the value out of the PUT response and puts the old one back
 * if the write failed, so the *store* is always right. The element is a separate
 * matter: clicking a checkbox changes its `checked` property, and Vue only patches that
 * property when the bound value moves. So a learner who asked for "on" and was answered
 * "off" — the same as before — would leave their own click on screen, and a failed write
 * would leave the opposite. Both are positions nobody confirmed. Re-asserting from the
 * model once the write settles is what makes "the server's value is what you see" true
 * of the DOM and not just of the store, and it keeps the element (and its focus) rather
 * than remounting it.
 */
async function toggleTopicMemory(event) {
  const input = event?.target ?? null
  const asked = input?.checked === true
  const hadFocus = input !== null && document.activeElement === input
  await learning.saveTopicMemory(asked)
  if (!input) return
  input.checked = learning.topicMemory.value === true
  // The control is `disabled` while the write is in flight, and a browser drops focus
  // from a disabled element without giving it back when the flag clears. So a keyboard
  // learner who toggled this switch would land on `<body>` after every attempt — on a
  // failure especially, where the answer they are reading is the rollback notice.
  // Focus is returned only when nothing else holds it: if the learner tabbed away while
  // the request was running, that is their position, and stealing it back is the worse
  // bug. `nextTick` first, because the element is still disabled until the patch lands.
  if (hadFocus && document.activeElement === document.body) {
    await nextTick()
    if (!input.disabled && document.activeElement === document.body) input.focus()
  }
}

async function retryTopicMemory() {
  await learning.loadTopicMemory()
}
</script>

<template>
  <div class="gl-page">
    <SectionFrame
      icon="learning"
      eyebrow="练习"
      title="练习"
      summary="选一段对话和一个主题，服务端从你自己的笔记里出三题，答完一题再出下一题。这一页不显示分数，也不显示评分理由。"
      status="已接入"
      status-tone="done"
    >
      <p
        v-if="headline && phase !== LearningPhase.Idle"
        class="learn__head"
        data-test="learn-phase"
      >
        {{ headline }}
      </p>

      <p
        v-if="learning.expiry.value.text"
        class="learn__clock"
        :class="{ 'learn__clock--off': learning.expiry.value.level === 'passed' }"
        role="status"
        data-test="learn-clock"
      >
        {{ learning.expiry.value.text }}
      </p>

      <div v-if="learning.error.value" data-test="learn-error">
        <ServerNotice :message="learning.error.value" />
      </div>

      <BaselineSurfaceNotice :replacement="{ href: '/learning/v2', label: '学习／测验' }" />

      <p v-if="learning.flash.value" class="learn__flash" role="status" data-test="learn-flash">
        {{ learning.flash.value }}
      </p>

      <!-- 这一轮练什么 -->
      <section class="learn__block" aria-labelledby="learn-setup-heading">
        <h2 id="learn-setup-heading">这一轮练什么</h2>

        <div class="learn__field">
          <label class="learn__label" for="learn-session">练习挂在哪段对话</label>
          <select
            id="learn-session"
            class="learn__select"
            data-test="learn-session-picker"
            :value="learning.sessionId.value"
            :disabled="busy || sessionsLoading"
            @change="chooseSession"
          >
            <option v-if="agentSessions.length === 0" value="">
              {{ sessionsLoading ? '正在载入对话…' : '没有可练习的学习 Agent 会话' }}
            </option>
            <option v-for="item in agentSessions" :key="item.id" :value="item.id">
              {{ item.title }}
            </option>
          </select>
        </div>
        <p v-if="noAgentSession" class="learn__hint" data-test="learn-no-agent-session">
          练习只能挂在“学习 Agent”会话上：先到「对话」页点“新建学习 Agent 会话”，再回到这一页开始。普通对话不能开始练习。
        </p>
        <p v-if="sessionsError" class="learn__hint" data-test="learn-sessions-error">
          {{ sessionsError }}
        </p>

        <!--
          主题记忆的同意开关：一段会话一个决定，值只从 GET 来。
          整个区块只在选中的是可练习会话时出现，读不到状态时控件是禁用的，
          所以页面永远不会把一个没读回来的位置显示成"已允许"或"未允许"。
        -->
        <div v-if="topicMemorySession" class="learn__field" data-test="learn-topic-memory">
          <label class="learn__consent" for="learn-topic-memory-switch">
            <input
              id="learn-topic-memory-switch"
              class="learn__checkbox"
              type="checkbox"
              role="switch"
              data-test="learn-topic-memory-switch"
              aria-describedby="learn-topic-memory-note"
              :checked="learning.topicMemory.value === true"
              :disabled="!canToggleTopicMemory"
              @change="toggleTopicMemory"
            />
            <span>允许学习 Agent 在当前会话中读取当前测验主题下已确认的记忆</span>
          </label>

          <p id="learn-topic-memory-note" class="learn__hint" data-test="learn-topic-memory-state">
            {{ topicMemoryStateText }}<span v-if="!openQuiz">有当前测验主题时才会生效；现在这一轮没有在进行中，偏好仍然可以先保存。</span>
          </p>

          <p v-if="learning.topicMemoryError.value" class="learn__hint" data-test="learn-topic-memory-error">
            {{ learning.topicMemoryError.value }}
          </p>

          <div v-if="learning.topicMemory.value === null && !learning.topicMemoryLoading.value" class="learn__actions">
            <button
              type="button"
              class="gl-btn gl-btn--quiet"
              data-test="learn-topic-memory-retry"
              @click="retryTopicMemory"
            >
              再读一次
            </button>
          </div>
        </div>

        <div class="learn__field">
          <label class="learn__label" for="learn-topic">主题</label>
          <input
            id="learn-topic"
            v-model="learning.topic.value"
            class="learn__input"
            type="text"
            maxlength="160"
            autocomplete="off"
            placeholder="例如：Redis 的 RDB 与 AOF"
            data-test="learn-topic"
            :disabled="topicLocked"
          />
        </div>

        <div class="learn__actions">
          <button
            type="button"
            class="gl-btn gl-btn--primary"
            data-test="learn-start"
            :disabled="!canStart"
            @click="startQuiz"
          >
            {{ learning.work.value === 'start' ? '正在准备…' : '开始这一轮' }}
          </button>
          <button
            v-if="view"
            type="button"
            class="gl-btn gl-btn--quiet"
            data-test="learn-refresh"
            :disabled="busy"
            @click="refreshQuiz"
          >
            刷新这一轮
          </button>
        </div>

        <p class="learn__hint" data-test="learn-setup-hint">
          一轮三题，出题成功后 24 小时内有效。资料里没有这个主题时服务端会直接拒绝出题。
        </p>
      </section>

      <!-- 没有进行中的一轮：这是一个状态，不是错误 -->
      <section
        v-if="!view && !resuming && !learning.error.value"
        class="learn__block"
        aria-labelledby="learn-idle-heading"
      >
        <h2 id="learn-idle-heading">当前没有进行中的练习</h2>
        <p class="learn__hint" data-test="learn-idle">
          {{
            learning.sessionId.value
              ? '这段对话现在没有开放中的一轮。填上面两项就能开始。'
              : '先选一段对话，再写主题。'
          }}
        </p>
      </section>

      <p v-else-if="!view && resuming" class="learn__hint" data-test="learn-resuming">
        正在看这一轮还在不在。
      </p>

      <!-- 题卡：只在服务端给了视图之后出现 -->
      <section
        v-if="view"
        class="learn__block"
        aria-labelledby="learn-card-heading"
        data-test="learn-card"
      >
        <header class="learn__card-head">
          <h2 id="learn-card-heading">{{ positionText || statusLabel }}</h2>
          <span class="learn__status" data-test="learn-status">{{ statusLabel }}</span>
        </header>

        <p v-if="!question" class="learn__hint" data-test="learn-no-question">
          {{
            view.status === 'created' || view.status === 'asking'
              ? '题目还在准备，稍等一下再看。'
              : '这一轮已经没有待答的题目了。'
          }}
        </p>

        <template v-else>
          <p v-if="typeLabel" class="learn__kind" data-test="learn-kind">{{ typeLabel }}</p>
          <p class="learn__question" data-test="learn-question">{{ question.text }}</p>

          <label class="learn__label" for="learn-answer">你的回答</label>
          <textarea
            id="learn-answer"
            v-model="learning.draft.value"
            class="learn__answer"
            rows="5"
            placeholder="用自己的话说清楚就行，不用凑长度。"
            data-test="learn-answer"
            :disabled="!learning.canAct.value"
          ></textarea>

          <div class="learn__actions">
            <button
              type="button"
              class="gl-btn gl-btn--primary"
              data-test="learn-submit"
              :disabled="!canSend"
              @click="learning.submitAnswer()"
            >
              {{ learning.work.value === 'answer' ? '正在发送…' : '提交答案' }}
            </button>
            <button
              type="button"
              class="gl-btn"
              data-test="learn-skip"
              :disabled="!learning.canAct.value"
              @click="learning.skip()"
            >
              {{ learning.work.value === 'skip' ? '正在跳过…' : '跳过这题' }}
            </button>
          </div>
        </template>

        <p
          v-if="view.status === 'completed'"
          class="learn__hint"
          data-test="learn-counted"
        >
          这一轮的作答已经计入你的掌握度记录。单题的评分理由不在这套接口里，所以这里也没有。
        </p>
        <p
          v-else-if="view.status === 'cancelled'"
          class="learn__hint"
          data-test="learn-cancelled-note"
        >
          这一轮由你结束，不再接受作答。要再来一轮就重新填主题开始。
        </p>
        <p
          v-else-if="view.status === 'expired'"
          class="learn__hint"
          data-test="learn-expired-note"
        >
          这一轮超时结束了。要再来一轮就重新填主题开始。
        </p>

        <div class="learn__actions learn__actions--end">
          <button
            type="button"
            class="gl-btn gl-btn--danger"
            data-test="learn-cancel"
            :disabled="!learning.canCancel.value"
            @click="endQuiz"
          >
            结束这一轮
          </button>
        </div>
      </section>

      <!-- 掌握度记录：数字只从这一个查询来 -->
      <section class="learn__block learn__block--quiet" aria-labelledby="learn-progress-heading">
        <h2 id="learn-progress-heading">这个主题的掌握度记录</h2>
        <p class="learn__hint">
          数字只来自这一个查询；开始、恢复、作答、跳过、结束的返回里都不带它。
        </p>
        <button
          type="button"
          class="gl-btn gl-btn--quiet"
          data-test="learn-progress-load"
          :disabled="learning.progressLoading.value || (!view && learning.topic.value.trim() === '')"
          @click="learning.loadProgress()"
        >
          {{ learning.progressLoading.value ? '正在查…' : '查一下这个主题' }}
        </button>
        <p v-if="learning.progressError.value" class="learn__hint" data-test="learn-progress-error">
          {{ learning.progressError.value }}
        </p>
        <p
          v-if="learning.progress.value === 'absent'"
          class="learn__hint"
          data-test="learn-progress-absent"
        >
          这个主题还没有可展示的掌握度记录。
        </p>
        <p v-else-if="progressIsFresh" class="learn__hint" data-test="learn-progress-text">
          {{ learning.progressText.value }}
        </p>
      </section>

      <p class="learn__foot">
        <AppIcon name="learning" :size="14" />
        <span>题目由服务端从你的笔记里生成。参考答案、评分依据和证据原文都不会下发给这一页。</span>
      </p>
    </SectionFrame>
  </div>
</template>

<style scoped>
.learn__head {
  margin: 0 0 0.6rem;
  font-size: 1.02rem;
  font-weight: 700;
}

.learn__clock {
  margin: 0 0 0.9rem;
  font-size: 0.88rem;
  color: var(--gl-ink-soft);
  font-variant-numeric: tabular-nums;
}

.learn__clock--off {
  color: var(--gl-warn);
  font-weight: 600;
}

.learn__flash {
  margin: 0 0 0.9rem;
  padding: 0.55rem 0.8rem;
  border-radius: var(--gl-radius-sm);
  background: var(--gl-accent-soft);
  color: var(--gl-accent-ink);
  font-size: 0.92rem;
  font-weight: 600;
}

.learn__block {
  margin-top: 1.1rem;
  padding-top: 1.1rem;
  border-top: 1px solid var(--gl-surface-line);
}

.learn__block h2 {
  font-size: 1.05rem;
}

.learn__block--quiet {
  margin-top: 1.2rem;
  padding: 0.9rem 1rem 1rem;
  border: 1px dashed var(--gl-surface-line);
  border-radius: var(--gl-radius-sm);
  background: var(--gl-surface-soft);
}

.learn__card-head {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 0.8rem;
}

.learn__status {
  font-size: 0.8rem;
  color: var(--gl-ink-faint);
}

.learn__kind {
  margin: 0.5rem 0 0;
  font-size: 0.84rem;
  color: var(--gl-accent-ink);
}

.learn__question {
  margin: 0.4rem 0 1rem;
  font-size: 1.05rem;
  line-height: 1.6;
  overflow-wrap: anywhere;
}

.learn__field {
  display: flex;
  flex-direction: column;
  gap: 0.3rem;
  margin-bottom: 0.8rem;
}

.learn__label {
  font-size: 0.84rem;
  font-weight: 600;
  color: var(--gl-ink-soft);
}

/* A consent line reads as one decision, not a form row: the box sits beside the
   sentence it governs and stays on the same baseline as the hints below it. */
.learn__consent {
  display: flex;
  align-items: flex-start;
  gap: 0.45rem;
  font-size: 0.92rem;
  line-height: 1.5;
}

.learn__checkbox {
  flex: none;
  width: 1.05rem;
  height: 1.05rem;
  margin-top: 0.12rem;
  cursor: pointer;
}

.learn__checkbox:disabled {
  cursor: not-allowed;
}

.learn__select,
.learn__input,
.learn__answer {
  width: 100%;
  padding: 0.55rem 0.7rem;
  border: 1px solid var(--gl-surface-line);
  border-radius: var(--gl-radius-sm);
  background: var(--gl-surface);
  color: var(--gl-ink);
  font: inherit;
  font-size: 0.94rem;
}

.learn__answer {
  margin: 0.2rem 0 0.8rem;
  resize: vertical;
  line-height: 1.6;
}

.learn__select:focus-visible,
.learn__input:focus-visible,
.learn__answer:focus-visible {
  outline: 2px solid var(--gl-accent);
  outline-offset: 1px;
}

.learn__actions {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 0.5rem;
}

.learn__actions--end {
  margin-top: 1.1rem;
  padding-top: 0.9rem;
  border-top: 1px dashed var(--gl-surface-line);
}

.learn__hint {
  margin: 0.6rem 0 0;
  font-size: 0.88rem;
  color: var(--gl-ink-soft);
  line-height: 1.6;
}

.learn__foot {
  display: flex;
  align-items: flex-start;
  gap: 0.45rem;
  margin: 1.4rem 0 0;
  font-size: 0.84rem;
  color: var(--gl-ink-faint);
  line-height: 1.6;
}
</style>
