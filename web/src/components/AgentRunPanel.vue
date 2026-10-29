<script setup>
import { computed } from 'vue'

import ServerNotice from './ServerNotice.vue'
import {
  agentStageDetail,
  agentStageTitle,
  AgentStage,
  isStageActive,
} from '../lib/agentRun.js'
import { formatStepDuration, StepStatus } from '../lib/agentSteps.js'

/**
 * The Learning Agent's turn status region, plus the step list the server reports while
 * a turn runs.
 *
 * Deliberately inert about everything it is not told: the stage line is projected from
 * the four `message.*` events, the steps come from the four `step_started` /
 * `tool_started` / `tool_result` / `waiting_confirmation` frames, and the step section
 * renders **only when steps are actually supplied** — with none, nothing here pretends
 * to be agent work. No step is timed, ordered or invented on the client: the server
 * numbers them, and a turn that reports nothing shows a single status line, as it did
 * before this protocol existed.
 *
 * The one frame it does *not* render is `waiting_confirmation`. A draft is a decision
 * about the answer that proposed it, so it is put under that answer, by
 * `MemoryDraftCard`, rather than described up here where a reader would have to go and
 * find the row themselves.
 */
const props = defineProps({
  stage: { type: String, required: true },
  run: { type: Object, default: null },
  steps: { type: Array, default: () => [] },
  expanded: { type: Boolean, default: false },
})

const emit = defineEmits(['update:expanded'])

const visible = computed(() => props.stage !== AgentStage.None)
const active = computed(() => isStageActive(props.stage))
const hasSteps = computed(() => Array.isArray(props.steps) && props.steps.length > 0)
const error = computed(
  () =>
    props.stage === AgentStage.Failed || props.stage === AgentStage.Cancelled
      ? props.run?.error ?? null
      : null,
)

const DOT = {
  [AgentStage.Waiting]: 'pending',
  [AgentStage.Generating]: 'active',
  [AgentStage.Completed]: 'done',
  [AgentStage.Failed]: 'failed',
  [AgentStage.Cancelled]: 'cancelled',
}
const dot = computed(() => DOT[props.stage] ?? 'pending')
const detail = computed(() => agentStageDetail(props.stage, props.run))

/**
 * Step wording stays inside this table on purpose: a step's status is a fact the frames
 * establish (running until the next step opens, or until its own result arrives), and the
 * sentence below is the only thing this component adds to it.
 */
const STEP_STATUS_TEXT = {
  [StepStatus.Running]: '进行中',
  [StepStatus.Completed]: '已完成',
  [StepStatus.Failed]: '未成功',
  [StepStatus.Cancelled]: '已取消',
}

function stepStatusText(status) {
  return Object.prototype.hasOwnProperty.call(STEP_STATUS_TEXT, status)
    ? STEP_STATUS_TEXT[status]
    : '状态未识别'
}

function stepDuration(step) {
  return formatStepDuration(step?.durationMillis)
}

/**
 * What is happening right now, on the collapsed line, so a learner does not have to open
 * the trail to see it. Only the server's own sentence is used; when nothing is running
 * (finished, failed, or a turn that reported no steps) the line stays exactly as plain as
 * it was before this protocol existed.
 */
const currentStepLabel = computed(() => {
  const running = (props.steps ?? []).find((step) => step?.status === StepStatus.Running)
  return running ? running.label ?? running.name ?? '' : ''
})
</script>

<template>
  <section v-if="visible" class="agent" data-test="agent-panel" :aria-live="active ? 'polite' : undefined">
    <p class="agent__line">
      <span class="agent__dot" :class="`agent__dot--${dot}`" aria-hidden="true"></span>
      <span class="agent__title">{{ agentStageTitle(props.stage) }}</span>
      <span v-if="currentStepLabel" class="agent__current" data-test="agent-current-step">{{ currentStepLabel }}</span>
      <span v-if="hasSteps" class="agent__count">{{ props.steps.length }} 步</span>
      <button
        v-if="hasSteps"
        type="button"
        class="agent__toggle"
        data-test="agent-steps-toggle"
        :aria-expanded="String(props.expanded)"
        @click="emit('update:expanded', !props.expanded)"
      >
        {{ props.expanded ? '收起过程' : '展开过程' }}
      </button>
    </p>

    <p v-if="detail" class="agent__note" data-test="agent-stage-detail">{{ detail }}</p>

    <ol v-if="hasSteps && props.expanded" class="agent__steps" data-test="agent-steps">
      <li v-for="(step, index) in props.steps" :key="step.id ?? index" class="agent__step">
        <span class="agent__step-index">{{ index + 1 }}</span>
        <span class="agent__step-label">{{ step.label ?? step.name ?? '步骤' }}</span>
        <span class="agent__step-state" data-test="agent-step-state">{{ stepStatusText(step.status) }}</span>
        <span v-if="step.detail" class="agent__step-detail" data-test="agent-step-detail">{{ step.detail }}</span>
        <span v-if="stepDuration(step)" class="agent__step-time" data-test="agent-step-time">
          {{ stepDuration(step) }}
        </span>
      </li>
    </ol>

    <!-- A draft this turn left behind is decided under the reply that proposed it, not
         here: see `MemoryDraftCard`. -->

    <ServerNotice
      v-if="error"
      class="agent__error"
      :variant="props.stage === AgentStage.Cancelled ? 'stopped' : 'error'"
      :message="error.message"
      :code="error.code"
    />
  </section>
</template>

<style scoped>
/* One quiet strip above the transcript: it reports state, it does not
   compete with the answer, and it adds no card of its own. */
.agent {
  display: flex;
  flex-direction: column;
  gap: 0.3rem;
  padding: 0.4rem 0.6rem;
  border-radius: var(--gl-radius-sm);
  background: var(--gl-surface-soft);
  border: 1px solid var(--gl-surface-line);
  font-size: 0.82rem;
  color: var(--gl-ink-soft);
}

.agent__line {
  display: flex;
  align-items: center;
  gap: 0.45rem;
  margin: 0;
}

.agent__dot {
  width: 8px;
  height: 8px;
  flex: none;
  border-radius: 50%;
  background: var(--gl-ink-faint);
}

.agent__dot--active {
  background: var(--gl-accent);
}

.agent__dot--done {
  background: var(--gl-accent);
  box-shadow: 0 0 0 2px var(--gl-accent-soft);
}

.agent__dot--failed {
  background: var(--gl-danger);
}

.agent__dot--cancelled {
  background: var(--gl-warn);
}

.agent__title {
  font-weight: 600;
  color: var(--gl-ink);
}

/* The step in progress, read from the server's own sentence for it. */
.agent__current {
  color: var(--gl-ink-soft);
  overflow-wrap: anywhere;
}

.agent__count {
  padding: 0 0.4em;
  border-radius: 999px;
  background: var(--gl-surface);
  border: 1px solid var(--gl-surface-line);
  font-size: 0.72rem;
}

.agent__toggle {
  margin-left: auto;
  padding: 0.1em 0.5em;
  border: 1px solid transparent;
  border-radius: var(--gl-radius-sm);
  background: none;
  font-size: 0.76rem;
  color: var(--gl-accent-ink);
}

.agent__toggle:hover {
  border-color: var(--gl-surface-line);
}

.agent__note {
  margin: 0;
  color: var(--gl-ink-faint);
}

.agent__steps {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 0.15rem;
}

.agent__step {
  display: flex;
  align-items: baseline;
  gap: 0.45rem;
  padding: 0.15rem 0;
  border-top: 1px dashed var(--gl-surface-line);
}

.agent__step-index {
  font-family: var(--gl-mono);
  font-size: 0.72rem;
  color: var(--gl-ink-faint);
}

.agent__step-label {
  overflow-wrap: anywhere;
}

.agent__step-state {
  flex: none;
  font-size: 0.72rem;
  color: var(--gl-ink-faint);
  white-space: nowrap;
}

/* The tool's own sentence about what it did — the only summary this client shows, and
   never what the tool found. */
.agent__step-detail {
  flex: 1 1 auto;
  color: var(--gl-ink-soft);
  overflow-wrap: anywhere;
}

.agent__step-time {
  flex: none;
  font-family: var(--gl-mono);
  font-size: 0.72rem;
  color: var(--gl-ink-faint);
  white-space: nowrap;
}

.agent__error {
  margin-top: 0.1rem;
}
</style>
