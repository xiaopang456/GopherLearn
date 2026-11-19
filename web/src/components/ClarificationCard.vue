<script setup>
import { computed, ref } from 'vue'
import AppIcon from './AppIcon.vue'

/**
 * One clarification, waiting for the learner to say which conversation they meant.
 *
 * The card is the same kind of thing as the memory draft card under the same
 * reply: a question, the choices, and a way to answer with your own words. What
 * differs is where the answer goes — a draft is decided by a request of its
 * own, while a clarification is answered by the learner's NEXT message. So the
 * card does not talk to the server: it hands the answer up, and the chat's own
 * send channel carries it, exactly as if the learner had typed it.
 *
 * The learner therefore never has to ask again. A chosen candidate is sent as
 * its session id, because that is the one reading the server can match exactly;
 * the title and the date on the button are for the learner, who has to tell
 * two conversations of the same family apart.
 */
const props = defineProps({ clarification: { type: Object, required: true } })
const emit = defineEmits(['choose'])

const freeText = ref('')
const answered = ref(false)

const question = computed(() => String(props.clarification.question ?? ''))
const prompt = computed(() => String(props.clarification.prompt ?? ''))
const candidates = computed(() =>
  Array.isArray(props.clarification.candidates) ? props.clarification.candidates : [],
)
const allowFreeText = computed(() => props.clarification.allowFreeText === true)

/** What the button says the conversation is called. */
function candidateTitle(candidate) {
  const title = String(candidate?.title ?? '').trim()
  return title || String(candidate?.sessionId ?? '').trim()
}

/** The subjects the conversation was filed under, as one line. */
function candidateTopics(candidate) {
  const topics = Array.isArray(candidate?.topics) ? candidate.topics : []
  return topics
    .map((topic) => String(topic ?? '').trim())
    .filter((topic) => topic !== '')
    .join('、')
}

/** The day the conversation was last talked in, in the learner's own time. */
function candidateDate(candidate) {
  const raw = String(candidate?.lastTalkedAt ?? '').trim()
  if (raw === '') return ''
  const parsed = new Date(raw)
  if (Number.isNaN(parsed.getTime())) return ''
  const month = String(parsed.getMonth() + 1).padStart(2, '0')
  const day = String(parsed.getDate()).padStart(2, '0')
  return `${parsed.getFullYear()}-${month}-${day}`
}

/**
 * The answer as the server reads it. The session id is the exact reading; the
 * title is the fallback for a candidate that came without one.
 */
function pick(candidate) {
  if (answered.value) return
  const sessionId = String(candidate?.sessionId ?? '').trim()
  const text = sessionId || String(candidate?.title ?? '').trim()
  if (text === '') return
  answered.value = true
  freeText.value = ''
  emit('choose', text)
}

/** The learner's own clue — a title, a time, a topic. */
function submitFreeText() {
  if (answered.value) return
  const text = freeText.value.trim()
  if (text === '') return
  answered.value = true
  freeText.value = ''
  emit('choose', text)
}
</script>
<template>
  <section class="gl-card v2-clarification" data-test="clarification-card" aria-label="澄清卡">
    <h3><AppIcon name="chat" :size="15" /> 需要你确认是哪一次</h3>
    <p data-test="clarification-question">{{ question }}</p>
    <p v-if="prompt" class="gl-muted" data-test="clarification-prompt">{{ prompt }}</p>
    <ul v-if="!answered && candidates.length" class="v2-clarification__list" data-test="clarification-candidates">
      <li v-for="(candidate, index) in candidates" :key="candidate.sessionId || index">
        <button
          type="button"
          class="gl-btn v2-clarification__candidate"
          :data-test="`clarification-candidate-${index + 1}`"
          @click="pick(candidate)"
        >
          <span class="v2-clarification__name">{{ candidateTitle(candidate) }}</span>
          <span v-if="candidateDate(candidate)" class="v2-clarification__when">{{ candidateDate(candidate) }}</span>
          <span v-if="candidateTopics(candidate)" class="v2-clarification__topics">{{ candidateTopics(candidate) }}</span>
        </button>
      </li>
    </ul>
    <form
      v-if="allowFreeText && !answered"
      class="v2-clarification__free"
      data-test="clarification-free"
      @submit.prevent="submitFreeText"
    >
      <label>或者直接告诉我时间、标题或主题
        <input v-model="freeText" data-test="clarification-free-text" placeholder="例如：上周讲 HTTP 的那次" />
      </label>
      <button type="button" class="gl-btn" data-test="clarification-free-submit" :disabled="!freeText.trim()" @click="submitFreeText">发送</button>
    </form>
    <p v-if="answered" class="gl-muted" data-test="clarification-answered">已经把你的选择发出去了，等这一轮回答。</p>
  </section>
</template>
<style scoped>
.v2-clarification { margin-block: .75rem; padding: .8rem; border: 1px solid var(--gl-surface-line); border-radius: var(--gl-radius-sm); background: var(--gl-surface-soft); }
.v2-clarification__list { display: flex; flex-direction: column; gap: .4rem; margin: .5rem 0; padding: 0; list-style: none; }
.v2-clarification__candidate { display: flex; flex-wrap: wrap; align-items: baseline; gap: .4rem; width: 100%; text-align: left; }
.v2-clarification__name { font-weight: 600; }
.v2-clarification__when, .v2-clarification__topics { color: var(--gl-ink-faint); font-size: var(--gl-text-xs); }
.v2-clarification__free { display: flex; flex-wrap: wrap; align-items: end; gap: .4rem; margin-block: .5rem; }
.v2-clarification__free label { display: grid; gap: .25rem; flex: 1 1 12rem; }
</style>
