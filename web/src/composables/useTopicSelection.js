import { computed, ref, watch } from 'vue'

import { browseTopicCatalogue, issueTopicConsent, suggestTopics, topicConsentCapability } from '../api/agentTopics.js'
import { localId } from '../lib/id.js'

const PAGE_SIZE = 20

const emptyPrompt = () => ({
  visible: false,
  candidates: [],
  selected: [],
  maxSelectable: 0,
  question: '',
  requestId: '',
  sessionId: '',
  catalogueOpened: false,
  catalogueQuery: '',
  catalogueOffset: 0,
  catalogueHasMore: false,
  catalogueBusy: false,
  confirmBusy: false,
})

function mergeCandidates(existing, incoming) {
  const byKey = new Map((existing ?? []).map((candidate) => [candidate.topicKey, candidate]))
  for (const candidate of incoming ?? []) {
    if (candidate && candidate.topicKey && !byKey.has(candidate.topicKey)) {
      byKey.set(candidate.topicKey, candidate)
    }
  }
  return [...byKey.values()]
}

/**
 * The read-side confirmation flow for one chat turn: suggestions, catalogue
 * browse, checkbox selection, ticket issuance, then the send.
 *
 * The one identity that must not drift is the requestId. A ticket is signed
 * for (user, session, requestId, question); the run that consumes it is
 * verified against the same four, so the suggestion, the ticket and the
 * message all carry the id minted here - never one generated inside send().
 * Skipping the selection sends the same id with no ticket at all, which is
 * how "do not use my records" reaches the server.
 *
 * State that belongs to a retired question is never reused: a session switch
 * clears the prompt, and every async continuation compares the prompt's own
 * requestId before it writes anything.
 */
export function useTopicSelection(chat, api = { suggestTopics, browseTopicCatalogue, issueTopicConsent, topicConsentCapability }) {
  const enabled = ref(false)
  const prompt = ref(emptyPrompt())
  const error = ref('')
  const suggesting = ref(false)
  // Whether this deployment runs the confirmed flow at all. Until the probe
  // answers, the entry stays hidden: the default read mode still matches
  // topics from the question, and the page must not promise "nothing is read
  // before you choose" on a deployment that does not ask.
  const capability = ref({ supported: false, checked: false })
  let capabilityPromise = null
  // One identity per submit attempt. A response may only land if its attempt
  // is still the current one - a session switch retires it even when the
  // learner comes back to the same session id (A -> B -> A).
  let attempt = 0

  // A session switch retires the pending question: its requestId, its question
  // and any ticket belong to the session it was asked in.
  watch(
    () => chat.activeId.value,
    () => {
      attempt += 1
      prompt.value = emptyPrompt()
      error.value = ''
      suggesting.value = false
    },
  )

  async function refreshCapability() {
    if (capability.value.checked) return capability.value
    if (capabilityPromise) return capabilityPromise
    capabilityPromise = (async () => {
      try {
        const data = await api.topicConsentCapability()
        capability.value = { supported: Boolean(data && data.supported === true), checked: true }
      } catch {
        // An unreachable probe means "do not offer the flow"; asking still works.
        capability.value = { supported: false, checked: true }
      }
      capabilityPromise = null
      return capability.value
    })()
    return capabilityPromise
  }

  const canOffer = computed(() => chat.isAgentSession.value === true)
  const offered = computed(() => canOffer.value && capability.value.supported)

  async function requestSuggestions(body) {
    try {
      const data = await api.suggestTopics(body)
      if (!data || data.status_code !== 1000) return null
      if (!Array.isArray(data.candidates) || data.candidates.length === 0 || !data.maxSelectable) return null
      return data
    } catch {
      // A deployment without the confirmed mode, or a transient failure, means
      // this turn simply has no topic personalisation. The question still goes.
      return null
    }
  }

  async function submit() {
    if (prompt.value.visible || suggesting.value) return
    const content = String(chat.draft.value ?? '').trim()
    if (!content || chat.streaming.value) return
    error.value = ''
    // The switch only applies to an agent session; anything else keeps the
    // ordinary send path and never probes.
    if (!enabled.value || !chat.isAgentSession.value || !chat.activeId.value) return chat.send()
    // A deployment that does not run the confirmed flow hides the entry; if
    // the probe has not answered yet, ask once before promising anything.
    if (!capability.value.checked) await refreshCapability()
    if (!capability.value.supported) return chat.send()
    const sessionId = chat.activeId.value
    const requestId = localId('send')
    const attemptToken = ++attempt
    chat.draft.value = ''
    suggesting.value = true
    const suggestion = await requestSuggestions({ sessionId, question: content, requestId })
    suggesting.value = false
    // The response belongs to one attempt only: a switch away retires it, and
    // so does coming back to the same session (A -> B -> A). A stale
    // suggestion must neither restore the old panel nor send the old question.
    if (attemptToken !== attempt || chat.activeId.value !== sessionId) return
    if (!suggestion) return chat.send(content, null, { requestId })
    prompt.value = {
      ...emptyPrompt(),
      visible: true,
      candidates: mergeCandidates([], suggestion.candidates),
      maxSelectable: suggestion.maxSelectable,
      question: content,
      requestId,
      sessionId,
    }
  }

  function toggleSelection(topicKey) {
    const pending = prompt.value
    if (!pending.visible || typeof topicKey !== 'string' || topicKey === '') return
    const index = pending.selected.indexOf(topicKey)
    if (index >= 0) {
      pending.selected.splice(index, 1)
      return
    }
    // The ceiling comes from the server's own response, so the two cannot
    // disagree about what will actually be accepted.
    if (pending.selected.length >= pending.maxSelectable) return
    pending.selected.push(topicKey)
  }

  function setCatalogueQuery(value) {
    prompt.value.catalogueQuery = String(value ?? '')
  }

  async function requestCatalogue(query, offset) {
    const pending = prompt.value
    if (!pending.visible || pending.catalogueBusy) return
    const requestId = pending.requestId
    pending.catalogueBusy = true
    try {
      const data = await api.browseTopicCatalogue({ query, offset, limit: PAGE_SIZE })
      if (!data || data.status_code !== 1000 || !Array.isArray(data.candidates)) {
        throw new Error(data?.status_msg || '主题目录暂时不可用')
      }
      // A switch, a skip or a new question closes this prompt; a stale page
      // must not repopulate the next question's choices.
      if (!prompt.value.visible || prompt.value.requestId !== requestId) return
      prompt.value.candidates = mergeCandidates(prompt.value.candidates, data.candidates)
      prompt.value.catalogueOffset = Number(data.nextOffset) || 0
      prompt.value.catalogueHasMore = Boolean(data.hasMore)
    } catch (cause) {
      if (prompt.value.visible && prompt.value.requestId === requestId) {
        error.value = cause?.message || '主题目录暂时不可用'
      }
    } finally {
      if (prompt.value.requestId === requestId) prompt.value.catalogueBusy = false
    }
  }

  async function openCatalogue() {
    prompt.value.catalogueOpened = true
    prompt.value.catalogueQuery = ''
    prompt.value.catalogueOffset = 0
    await requestCatalogue('', 0)
  }

  async function searchCatalogue() {
    prompt.value.catalogueOffset = 0
    await requestCatalogue(prompt.value.catalogueQuery, 0)
  }

  async function showAllTopics() {
    prompt.value.catalogueQuery = ''
    prompt.value.catalogueOffset = 0
    await requestCatalogue('', 0)
  }

  async function loadMoreTopics() {
    await requestCatalogue(prompt.value.catalogueQuery, prompt.value.catalogueOffset)
  }

  async function confirmSelection() {
    const pending = prompt.value
    if (!pending.visible || pending.selected.length === 0 || pending.confirmBusy) return
    const { sessionId, question, requestId } = pending
    const selectedTopicKeys = [...pending.selected]
    pending.confirmBusy = true
    try {
      // The final ticket is issued from the current server-side catalogue, not
      // trusted from an earlier suggestion or browse response.
      const data = await api.issueTopicConsent({ sessionId, question, requestId, selectedTopicKeys })
      if (!data || data.status_code !== 1000 || !data.ticket) {
        throw new Error(data?.status_msg || '所选主题已失效，请重新选择')
      }
      if (!prompt.value.visible || prompt.value.requestId !== requestId) return
      prompt.value = emptyPrompt()
      error.value = ''
      // Same requestId, same session, same question the ticket was signed for.
      chat.send(question, null, { requestId, topicConsent: { ticket: data.ticket, selectedTopicKeys } })
    } catch (cause) {
      pending.confirmBusy = false
      if (prompt.value.requestId === requestId) {
        error.value = cause?.message || '所选主题已失效，请重新选择'
      }
    }
  }

  function skipSelection() {
    const pending = prompt.value
    if (!pending.visible) return
    const { question, requestId } = pending
    prompt.value = emptyPrompt()
    error.value = ''
    // Deliberately no ticket rather than a ticket with an empty selection:
    // "do not use my records" and "use none of the offered ones" reach the
    // server the same way, and the simpler form cannot expire.
    chat.send(question, null, { requestId })
  }

  return {
    enabled,
    prompt,
    error,
    suggesting,
    capability,
    offered,
    canOffer,
    refreshCapability,
    submit,
    toggleSelection,
    setCatalogueQuery,
    openCatalogue,
    searchCatalogue,
    showAllTopics,
    loadMoreTopics,
    confirmSelection,
    skipSelection,
  }
}
