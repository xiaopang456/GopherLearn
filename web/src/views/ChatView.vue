<script setup>
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import AgentRunPanel from '../components/AgentRunPanel.vue'
import AppIcon from '../components/AppIcon.vue'
import ComposerBox from '../components/ComposerBox.vue'
import MessageBubble from '../components/MessageBubble.vue'
import ServerNotice from '../components/ServerNotice.vue'
import SessionPanel from '../components/SessionPanel.vue'
import { useChat } from '../composables/useChat.js'
import { useKnowledge } from '../composables/useKnowledge.js'
import { useMediaQuery } from '../composables/useMediaQuery.js'
import { SessionMode } from '../lib/modes.js'

const route = useRoute()
const router = useRouter()
const chat = useChat()
const knowledge = useKnowledge()

const panelOpen = ref(false)
const transcript = ref(null)

const sessionId = computed(() => String(route.params.sessionId ?? ''))
// 1021px is the same breakpoint the scoped CSS below uses for the drawer.
const isDesktop = useMediaQuery('(min-width: 1021px)')
const showPanel = computed(() => isDesktop.value || panelOpen.value)

// The drawer is a small-screen affordance; leaving it open while the window
// grows back would show it twice, and shrinking should return to closed.
watch(isDesktop, (desktop) => {
  if (desktop) panelOpen.value = false
})

function togglePanel() {
  panelOpen.value = !panelOpen.value
}

onMounted(() => {
  chat.loadSessions()
  if (sessionId.value) chat.openSession(sessionId.value)
})

// The URL is the source of truth for which session is open.
watch(sessionId, (id) => {
  panelOpen.value = false
  if (id) {
    if (id !== chat.activeId.value) chat.openSession(id)
  } else {
    chat.closeSession()
  }
})

// A session created implicitly by the first message is written back to the URL
// so a reload lands on the same conversation.
watch(
  () => chat.activeId.value,
  (id) => {
    if (id && id !== sessionId.value) {
      router.replace({ name: 'session', params: { sessionId: id } })
    }
  },
)

watch(
  () => [chat.messages.value.length, chat.run.value?.text?.length ?? 0],
  async () => {
    await nextTick()
    const node = transcript.value
    if (node) node.scrollTop = node.scrollHeight
  },
)

async function startNew(mode) {
  const id = await chat.createSession(mode)
  if (id) router.replace({ name: 'session', params: { sessionId: id } })
}

function chooseKnowledge(next) {
  chat.setKnowledge(chat.activeId.value, next)
}

function select(id) {
  router.push({ name: 'session', params: { sessionId: id } })
}
</script>

<template>
  <div class="chat">
    <div
      v-if="showPanel && !isDesktop"
      class="chat__scrim"
      data-test="scrim"
      @click="togglePanel"
    ></div>

    <SessionPanel
      v-if="showPanel"
      class="chat__panel"
      :sessions="chat.sessions.value"
      :active-id="chat.activeId.value"
      :loading="chat.sessionsLoading.value"
      :error="chat.sessionsError.value"
      @select="select"
      @create="startNew"
      @remove="chat.removeSession"
    />

    <section class="chat__main" aria-label="对话内容">
      <header class="chat__head">
        <button
          type="button"
          class="chat__drawer"
          data-test="drawer"
          :aria-expanded="String(showPanel)"
          title="会话列表"
          @click="togglePanel"
        >
          <AppIcon :name="panelOpen ? 'close' : 'panel'" :size="17" />
        </button>
        <h1 class="chat__title">
          {{ chat.activeSession.value?.title ?? '新对话' }}
        </h1>
      </header>

      <button
        v-if="chat.sessionsError.value && chat.pendingCreate.value?.mode === SessionMode.LearningAgent"
        type="button"
        class="chat__retry"
        data-test="retry-create"
        @click="chat.retryCreate"
      >
        重试同一次创建
      </button>
      <button
        v-if="chat.retryableSend.value"
        type="button"
        class="chat__retry"
        data-test="retry-agent-send"
        @click="chat.retrySend"
      >
        重试同一次 Agent 请求（沿用原操作键）
      </button>

      <div ref="transcript" class="chat__scroll" data-test="transcript">
        <p v-if="chat.historyError.value" class="chat__pad">
          <ServerNotice :message="chat.historyError.value" />
        </p>

        <div v-else-if="chat.historyLoading.value" class="chat__pad chat__busy">
          正在载入历史消息…
        </div>

        <div v-else-if="chat.messages.value.length === 0" class="chat__pad">
          <div class="chat__empty" data-test="empty-state">
            <div class="chat__empty-mark">
              <AppIcon name="logo" :size="26" />
            </div>
            <h2 class="chat__empty-title">
              {{ chat.isAgentSession.value ? '开始一段学习会话' : '从一个具体的问题开始' }}
            </h2>
            <p class="chat__empty-text">
              {{
                chat.isAgentSession.value
                  ? '直接提出学习目标即可。这个会话属于学习 Agent，输入框不会出现知识库开关。'
                  : '直接输入即可开始普通对话；需要使用学习 Agent 时，可单独创建一个会话。'
              }}
            </p>
            <div v-if="!chat.activeMode.value" class="chat__empty-actions">
              <button
                type="button"
                class="gl-btn gl-btn--primary"
                data-test="empty-new-chat"
                @click="startNew(SessionMode.Chat)"
              >
                <AppIcon name="chat" :size="15" />
                新建对话
              </button>
              <button
                type="button"
                class="gl-btn"
                data-test="empty-new-agent"
                @click="startNew(SessionMode.LearningAgent)"
              >
                <AppIcon name="learning" :size="15" />
                新建学习 Agent 会话
              </button>
            </div>
          </div>
        </div>

        <div v-else class="chat__list" role="log" aria-live="polite">
          <header
            v-if="chat.isAgentSession.value"
            class="chat__agenthead"
            data-test="agent-header"
          >
            <span class="chat__agenthead-glyph">
              <AppIcon name="learning" :size="15" />
            </span>
            <div class="chat__agenthead-text">
              <p class="chat__agenthead-title">学习 Agent</p>
              <p class="chat__agenthead-desc">
                学习 Agent 会根据服务端当前启用的能力处理问题。执行步骤仅在真实发生并由服务端返回后显示，重新打开这个会话仍能读到。
              </p>
            </div>
          </header>

          <AgentRunPanel
            v-if="chat.isAgentSession.value"
            :stage="chat.agentStage.value"
            :run="chat.run.value"
            :steps="chat.agentSteps.value"
            :expanded="chat.agentPanelOpen.value"
            @update:expanded="chat.setAgentPanelOpen"
          />

          <p
            v-else-if="!chat.activeMode.value && !chat.historyLoading.value"
            class="chat__notice"
          >
            这个会话的类型没能识别出来，先按普通对话处理。
          </p>
          <MessageBubble
            v-for="item in chat.messages.value"
            :key="item.id"
            :message="item"
            :steps="chat.stepsForMessage(item)"
          />
        </div>
      </div>

      <p v-if="chat.sendError.value" class="chat__send-error">
        {{ chat.sendError.value }}
      </p>

      <ComposerBox
        v-model="chat.draft.value"
        :streaming="chat.streaming.value"
        :disabled="chat.historyLoading.value"
        :mode="chat.activeMode.value"
        :knowledge="chat.activeKnowledge.value"
        :collections="knowledge.collections.value"
        :can-use-knowledge="chat.canUseKnowledge.value"
        @submit="chat.send()"
        @abort="chat.cancel"
        @update:knowledge="chooseKnowledge"
      />
    </section>
  </div>
</template>

<style scoped>
.chat {
  position: relative;
  display: grid;
  grid-template-columns: var(--gl-panel) minmax(0, 1fr);
  height: 100%;
  min-height: 0;
}

.chat__panel {
  min-height: 0;
}

/* On a narrow screen the session list is a drawer over the conversation. It
   needs something to sit on: without a scrim the page behind it stays
   readable and the drawer reads as a floating panel, not as an opened door. */
.chat__scrim {
  position: absolute;
  z-index: 20;
  inset: 0;
  background: rgba(23, 23, 26, 0.32);
}

.chat__main {
  display: flex;
  flex-direction: column;
  min-width: 0;
  min-height: 0;
  background: var(--gl-paper);
}

/* No rule under this bar: the app bar above it already carries the only
   separation the page needs, and a second one makes the two read as one
   thick band. */
.chat__head {
  display: flex;
  align-items: center;
  gap: var(--gl-space-3);
  height: 46px;
  padding: 0 var(--gl-space-6);
  background: var(--gl-surface);
}

.chat__drawer {
  display: flex;
  width: 30px;
  height: 30px;
  flex: none;
  align-items: center;
  justify-content: center;
  padding: 0;
  border: 0;
  border-radius: var(--gl-radius-xs);
  background: none;
  color: var(--gl-ink-soft);
}

.chat__drawer:hover {
  background: var(--gl-surface-soft);
  color: var(--gl-ink);
}

.chat__title {
  font-size: var(--gl-text-md);
  font-weight: 600;
  letter-spacing: -0.01em;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.chat__retry {
  align-self: flex-start;
  margin: var(--gl-space-3) var(--gl-space-6) 0;
}

.chat__scroll {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  padding: var(--gl-space-6) var(--gl-space-6) var(--gl-space-4);
}

.chat__pad {
  margin: 0;
}

.chat__busy {
  color: var(--gl-ink-faint);
  font-size: var(--gl-text-sm);
}

.chat__list {
  display: flex;
  flex-direction: column;
  gap: var(--gl-space-6);
  max-width: 760px;
  margin: 0 auto;
}

/* --- empty state ---------------------------------------------------- */

.chat__empty {
  display: flex;
  max-width: 420px;
  margin: 10vh auto 0;
  flex-direction: column;
  align-items: flex-start;
  gap: var(--gl-space-3);
}

.chat__empty-mark {
  display: flex;
  width: 52px;
  height: 52px;
  align-items: center;
  justify-content: center;
  border-radius: var(--gl-radius);
  background: var(--gl-accent-soft);
  color: var(--gl-accent-ink);
}

.chat__empty-title {
  font-size: var(--gl-text-xl);
  font-weight: 650;
  letter-spacing: -0.02em;
}

.chat__empty-text {
  font-size: var(--gl-text-base);
  color: var(--gl-ink-soft);
  line-height: var(--gl-leading-normal);
}

.chat__empty-actions {
  display: flex;
  flex-wrap: wrap;
  gap: var(--gl-space-2);
  padding-top: var(--gl-space-2);
}

/* --- agent notice ---------------------------------------------------- */

.chat__notice {
  margin: 0 0 var(--gl-space-2);
  padding: var(--gl-space-2) var(--gl-space-3);
  border-radius: var(--gl-radius-sm);
  background: var(--gl-surface-soft);
  border: 1px solid var(--gl-surface-line);
  font-size: var(--gl-text-sm);
  color: var(--gl-ink-soft);
}

.chat__agenthead {
  display: flex;
  align-items: flex-start;
  gap: var(--gl-space-2);
  padding: var(--gl-space-3);
  border-radius: var(--gl-radius);
  background: var(--gl-surface);
  border: 1px solid var(--gl-surface-line);
}

.chat__agenthead-glyph {
  display: flex;
  width: 26px;
  height: 26px;
  flex: none;
  align-items: center;
  justify-content: center;
  border-radius: var(--gl-radius-xs);
  background: var(--gl-accent-soft);
  color: var(--gl-accent-ink);
}

.chat__agenthead-text {
  min-width: 0;
}

.chat__agenthead-title {
  font-size: var(--gl-text-sm);
  font-weight: 600;
}

.chat__agenthead-desc {
  margin-top: 2px;
  font-size: var(--gl-text-sm);
  color: var(--gl-ink-faint);
  line-height: var(--gl-leading-normal);
}

.chat__send-error {
  margin: 0;
  padding: var(--gl-space-2) var(--gl-space-6);
  background: var(--gl-danger-soft);
  color: var(--gl-danger);
  font-size: var(--gl-text-sm);
  font-weight: 600;
}

@media (max-width: 1020px) {
  .chat {
    grid-template-columns: minmax(0, 1fr);
  }

  .chat__head,
  .chat__scroll {
    padding-inline: var(--gl-space-4);
  }
}

@media (min-width: 1021px) {
  .chat__drawer {
    display: none;
  }
}
</style>
