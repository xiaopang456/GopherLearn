import { computed, ref } from 'vue'

import {
  addMemory as apiAddMemory,
  archiveMemory as apiArchiveMemory,
  confirmDraft as apiConfirmDraft,
  listDrafts as apiListDrafts,
  listMemories as apiListMemories,
  memoryAdapterName,
  memoryError,
  MEMORY_FORM_COPY,
  MemoryStatus,
  normalizeDraftList,
  normalizeMemoryList,
  normalizeMemoryResponse,
  onMemoryAdapterChange,
  rejectDraft as apiRejectDraft,
  validateMemoryForm,
} from '../api/memory.js'
import { isAbort, reportIfRejected } from '../api/http.js'

import { authStore } from '../auth/store.js'

/**
 * The memory page's state: pending drafts, effective memories, the three
 * transitions the confirmation gate is built from (confirm, reject, archive), and
 * the one write a learner starts themselves — which needs no gate, because stating
 * a fact about yourself *is* the decision.
 *
 * Module level for the same reason `useKnowledge` is: one store, so a draft
 * confirmed in one place cannot still be on offer in another. That sharing is
 * also the hazard — it outlives the component *and* the account — so every read
 * and write below is keyed on an epoch, exactly like the knowledge store.
 *
 * The operations themselves are deliberately conservative: one mutation in
 * flight at a time, no optimistic write, and the row only moves once the adapter
 * has accepted the change. A confirmation that "felt" successful but was rejected
 * by the server must not have already removed the user's only chance to look at
 * the draft again.
 */

let epoch = 0

const drafts = ref([])
const draftsLoading = ref(false)
const draftsError = ref('')
/**
 * Whether this account has actually been asked. Without it a first paint before
 * the response arrives would satisfy "no pending drafts" and tell the user
 * nothing is waiting when the truth is that the page does not know yet.
 */
const draftsLoaded = ref(false)
let draftsGeneration = 0
let draftsController = null

const memories = ref([])
const memoriesLoading = ref(false)
const memoriesError = ref('')
const memoriesLoaded = ref(false)
let memoriesGeneration = 0
let memoriesController = null

const mutating = ref(false)
const busyId = ref('')
const actionError = ref('')
const flash = ref('')

/**
 * The sentinel id an *addition* carries through the same single-flight lock the
 * transitions use. It is not a memory id — no row exists until the server writes
 * one — and it is what lets the form say "adding" while a row cannot mistake that
 * for its own confirm or archive button.
 */
const ADD_ROW = '__add__'

function isStale(startedIn) {
  return startedIn !== epoch
}

const pendingDrafts = computed(() => drafts.value)
const activeMemories = computed(() =>
  memories.value.filter((item) => item.status === MemoryStatus.Active),
)
/**
 * How many rows *this page* archived since the current account loaded it.
 *
 * Not "how many archived memories exist": the listing only ever returns pending
 * and active rows, so retired ones are invisible here and any total drawn from
 * them would be a number the client cannot know. A count of the actions this
 * screen actually took is the honest version of the same sentence.
 */
const archivedHere = ref(0)
/**
 * `mock` / `unbound` / `http`, straight off the installed adapter. The page uses
 * it for its header badge so a reader can never mistake demo rows for stored
 * memories, or an unwired build for a working one. A plain function call would
 * be cached forever by the computed that wraps it, so the adapter announces
 * itself instead.
 */
const dataSource = ref(memoryAdapterName())
onMemoryAdapterChange((name) => {
  dataSource.value = name
})
const showsDemoData = computed(() => dataSource.value === 'mock')
/** True only while *this* page's add request is in flight, not another row's. */
const adding = computed(() => mutating.value && busyId.value === ADD_ROW)
/**
 * Whether a hand-written memory can be offered at all. An unbound adapter has no
 * memory write, so the form is not shown: a screen that can only fail is worse
 * than no screen, and this page must never look like it stores something it cannot.
 */
const canAddMemory = computed(() => dataSource.value !== 'unbound')

async function loadDrafts({ silent = false } = {}) {
  const generation = ++draftsGeneration
  const startedIn = epoch
  if (draftsController) draftsController.abort()
  const controller = new AbortController()
  draftsController = controller

  if (!silent) {
    draftsLoading.value = true
    draftsError.value = ''
  }
  try {
    const payload = await apiListDrafts({ signal: controller.signal })
    if (isStale(startedIn) || generation !== draftsGeneration) return []
    drafts.value = normalizeDraftList(payload)
    if (silent) draftsError.value = ''
    return drafts.value
  } catch (error) {
    if (isAbort(error)) return drafts.value
    // A rejected bearer is a session fact, not a page fact: hand it to the one
    // funnel that signs the user out and moves them, then stop describing it
    // here as a failed load.
    reportIfRejected(error)
    if (isStale(startedIn) || generation !== draftsGeneration) return []
    // A failed read is reported, but rows already on screen stay: the same
    // account may well still be looking at valid drafts, and blanking them on a
    // transient failure would read as if they had been processed.

    draftsError.value = memoryError(error).copy
    return drafts.value
  } finally {
    if (draftsController === controller) draftsController = null
    if (generation === draftsGeneration) {
      draftsLoading.value = false
      if (!silent) draftsLoaded.value = true
    }
  }
}

async function loadMemories({ silent = false } = {}) {
  const generation = ++memoriesGeneration
  const startedIn = epoch
  if (memoriesController) memoriesController.abort()
  const controller = new AbortController()
  memoriesController = controller

  if (!silent) {
    memoriesLoading.value = true
    memoriesError.value = ''
  }
  try {
    const payload = await apiListMemories({ signal: controller.signal })
    if (isStale(startedIn) || generation !== memoriesGeneration) return []
    memories.value = normalizeMemoryList(payload)
    if (silent) memoriesError.value = ''
    return memories.value
  } catch (error) {
    if (isAbort(error)) return memories.value
    reportIfRejected(error)
    if (isStale(startedIn) || generation !== memoriesGeneration) return []

    memoriesError.value = memoryError(error).copy
    return memories.value
  } finally {
    if (memoriesController === controller) memoriesController = null
    if (generation === memoriesGeneration) {
      memoriesLoading.value = false
      if (!silent) memoriesLoaded.value = true
    }
  }
}

async function loadAll() {
  await Promise.all([loadDrafts(), loadMemories()])
}

function mergeMemory(list, record) {
  const rest = list.filter((item) => item.id !== record.id)
  return [...rest, record]
}

/**
 * The single runner behind every write: the three transitions and the create.
 *
 * `mutating` is what makes a double click cost one request — and it gates *all*
 * action buttons, not just the clicked one, because two concurrent writes to one
 * confirmation gate have no defined order and can leave the page describing a
 * state the server never agreed to.
 *
 * `succeed` applies the change locally so the row moves the moment the server
 * agrees; every path, success or failure, then finishes with a silent reload of
 * both lists, so what stays on screen is the server's answer rather than what
 * the client hoped for.
 */
async function runTransition({ id, successMessage, operation, succeed }) {
  if (!id || mutating.value) return false
  const startedIn = epoch
  mutating.value = true
  busyId.value = id
  actionError.value = ''
  flash.value = ''
  let applied = false

  try {
    const payload = await operation()
    if (!isStale(startedIn)) {
      succeed(payload)
      flash.value = successMessage
      applied = true
    }
  } catch (error) {
    // A rejected bearer is a session fact: hand it to the funnel that signs the
    // user out and moves them, wherever it was detected.
    reportIfRejected(error)
    if (!isStale(startedIn)) actionError.value = memoryError(error).copy
  } finally {
    // Awaited here so no caller can observe a half-refreshed page between the
    // write landing and the lists catching up with it. A stale transition does
    // nothing at all: the sign-out already wiped this state, and clearing the
    // flags now would only unlock whatever the next account just started.
    if (!isStale(startedIn)) {
      await Promise.all([loadDrafts({ silent: true }), loadMemories({ silent: true })])
      mutating.value = false
      busyId.value = ''
    }
  }

  return applied
}

async function confirmDraft(draftId) {
  const id = String(draftId ?? '')
  return runTransition({
    id,
    successMessage: '已确认，这条记忆现在生效。',
    operation: () => apiConfirmDraft(id),
    succeed: (payload) => {
      drafts.value = drafts.value.filter((item) => item.id !== id)
      const created = normalizeMemoryResponse(payload)
      if (created) memories.value = mergeMemory(memories.value, created)
    },
  })
}

async function rejectDraft(draftId) {
  const id = String(draftId ?? '')
  return runTransition({
    id,
    successMessage: '已拒绝，这条草稿不会生效。',
    operation: () => apiRejectDraft(id),
    succeed: () => {
      drafts.value = drafts.value.filter((item) => item.id !== id)
    },
  })
}

async function archiveMemory(memoryId) {
  const id = String(memoryId ?? '')
  return runTransition({
    id,
    successMessage: '已归档，这条记忆不再算作有效记忆。',
    operation: () => apiArchiveMemory(id),
    succeed: (payload) => {
      // The action answers with the retired row, so the list is rebuilt from what
      // the server said rather than from a guess about which status to write.
      const retired = normalizeMemoryResponse(payload)
      memories.value = memories.value.filter((item) => item.id !== id)
      if (retired && retired.status === MemoryStatus.Active) {
        memories.value = mergeMemory(memories.value, retired)
      }
      archivedHere.value += 1
    },
  })
}

/**
 * State a memory by hand.
 *
 * Runs through the same single-flight runner as the three transitions, so a double
 * click is one POST and no other write can interleave with it. The form is re-checked
 * here rather than trusted: an invalid submission never reaches the network.
 *
 * The row lands in the **active** list, not the pending one — this request carries no
 * provenance and the submit button *is* the confirmation, so putting it in the draft
 * queue would ask the learner to confirm a decision they already made. A previous
 * active statement about the same subject is superseded by the server in the same
 * write, which the silent reload below then reflects.
 *
 * @param {{scope: string, topicKey?: string, content: string}} form
 * @returns {Promise<{added: boolean, errors: Record<string,string>}>}
 */
async function addMemory(form) {
  const { valid, errors } = validateMemoryForm(form)
  if (!valid) return { added: false, errors }
  if (mutating.value) return { added: false, errors: {} }

  const applied = await runTransition({
    id: ADD_ROW,
    successMessage: MEMORY_FORM_COPY.added,
    operation: () => apiAddMemory(form),
    succeed: (payload) => {
      // The server's answer is the row: id, timestamps, pinned confidence and which
      // older statement it just superseded all belong to it. A body we cannot
      // address is not inserted, and the reload decides what the list holds.
      const created = normalizeMemoryResponse(payload)
      if (created) memories.value = mergeMemory(memories.value, created)
    },
  })
  return { added: applied, errors: {} }
}

function clearFlash() {
  flash.value = ''
}

/**
 * Wipe everything belonging to the account that just left.
 *
 * Reads are aborted and their generations advanced; an in-flight mutation is
 * *not* aborted, because the server may already have committed it — its result
 * is dropped by the epoch check instead, and the next user's screen never sees
 * the previous user's rows.
 */
export function resetMemoryState() {
  epoch += 1
  draftsGeneration += 1
  memoriesGeneration += 1
  if (draftsController) {
    const pending = draftsController
    draftsController = null
    pending.abort()
  }
  if (memoriesController) {
    const pending = memoriesController
    memoriesController = null
    pending.abort()
  }
  drafts.value = []
  draftsLoading.value = false
  draftsError.value = ''
  draftsLoaded.value = false
  memories.value = []
  memoriesLoading.value = false
  memoriesError.value = ''
  memoriesLoaded.value = false

  mutating.value = false
  busyId.value = ''
  actionError.value = ''
  flash.value = ''
  archivedHere.value = 0
}

// One registration at module load covers every exit path, because they all
// funnel through `authStore.signOut()`. The auth layer never learns what this
// store holds.
authStore.subscribeSignOut(resetMemoryState)

export function useMemory() {
  return {
    pendingDrafts,
    activeMemories,
    archivedHere,
    dataSource,
    showsDemoData,
    draftsLoading,
    draftsError,
    draftsLoaded,
    memoriesLoading,
    memoriesError,
    memoriesLoaded,

    mutating,
    busyId,
    adding,
    canAddMemory,
    actionError,
    flash,
    loadAll,
    loadDrafts,
    loadMemories,
    addMemory,
    confirmDraft,
    rejectDraft,
    archiveMemory,
    clearFlash,
    resetMemoryState,
  }
}
