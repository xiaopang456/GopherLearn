import { computed, ref } from 'vue'

import {
  addDocument,
  createCollection as apiCreateCollection,
  deleteDocument as apiDeleteDocument,
  formatForFile,
  listCollections,
  listDocuments,
  MAX_CONTENT_BYTES,
  normalizeCollectionList,
  normalizeCollectionResponse,
  normalizeDocumentList,
  normalizeDocumentResponse,
  replaceDocument as apiReplaceDocument,
  validateDocumentContent,
} from '../api/knowledge.js'
import { isAbort, safeErrorMessage } from '../api/http.js'

import { authStore } from '../auth/store.js'

/**
 * The single source of truth for knowledge collections and documents.
 *
 * The state is module level on purpose: the Chat composer and the knowledge
 * management page must see the same list, because a grounding toggle that reads
 * a private copy of `collections` is how a session ends up sending an id that
 * the user deleted a moment ago. There is exactly one store, and both surfaces
 * go through it.
 *
 * Being shared across the whole tab is also the hazard: it outlives a component
 * and, worse, outlives an *account*. Everything below therefore keys off a
 * session `epoch` as well as per-resource generations.
 */

let epoch = 0

const collections = ref([])
const collectionsLoading = ref(false)
const collectionsError = ref('')

let collectionsGeneration = 0
let collectionsController = null

const activeCollectionId = ref('')
const documents = ref([])
const documentsLoading = ref(false)
const documentsError = ref('')
const mutating = ref(false)
let documentsGeneration = 0
let documentsController = null

const collectionIds = computed(() => collections.value.map((item) => item.id))
const activeCollection = computed(
  () =>
    collections.value.find((item) => item.id === activeCollectionId.value) ?? null,
)

/**
 * Generations stop one account's requests from overtaking each other. The epoch
 * stops a *previous* account's request from writing at all: after a sign-out it
 * has no right to repopulate anything, however recently it was started.
 *
 * @param {number} startedIn the epoch captured when the operation began
 */
function isStale(startedIn) {
  return startedIn !== epoch
}

async function loadCollections() {
  const generation = ++collectionsGeneration
  const startedIn = epoch
  if (collectionsController) collectionsController.abort()
  const controller = new AbortController()
  collectionsController = controller

  collectionsLoading.value = true
  collectionsError.value = ''
  try {
    const payload = await listCollections({ signal: controller.signal })
    if (isStale(startedIn) || generation !== collectionsGeneration) return []
    const list = normalizeCollectionList(payload)
    collections.value = list
    reconcileActiveCollection(list)
    return list
  } catch (error) {
    if (isStale(startedIn) || isAbort(error) || generation !== collectionsGeneration) {
      return []
    }
    // A transient failure keeps the list the same user already had; an empty
    // list on a network blip would look like their documents vanished.

    collectionsError.value = safeErrorMessage(error, '知识库列表加载失败。')
    return []
  } finally {
    if (collectionsController === controller) collectionsController = null
    if (!isStale(startedIn) && generation === collectionsGeneration) {
      collectionsLoading.value = false
    }
  }
}

/**
 * A refresh that no longer contains the open collection leaves the screen
 * pointing at something the server will not answer for, so drop the selection
 * along with the documents it was showing.
 */
function reconcileActiveCollection(list) {
  if (!activeCollectionId.value) return
  if (list.some((item) => item.id === activeCollectionId.value)) return
  dropActiveCollection()
}

function dropActiveCollection() {
  activeCollectionId.value = ''
  documentsGeneration += 1
  if (documentsController) documentsController.abort()
  documents.value = []
  documentsError.value = ''
  documentsLoading.value = false
}

async function createCollection(title) {
  const clean = String(title ?? '').trim()
  if (!clean) {
    collectionsError.value = '请填写知识库名称。'
    return null
  }
  const startedIn = epoch
  mutating.value = true
  collectionsError.value = ''
  try {
    const payload = await apiCreateCollection(clean)
    // The account may have changed while this was in flight. Appending the
    // result then would hand the new user a collection that is not theirs.
    if (isStale(startedIn)) return null
    const created = normalizeCollectionResponse(payload)
    if (!created) {
      collectionsError.value = '服务没有返回新建知识库的标识。'
      return null
    }
    // Append rather than replace, so a list refresh cannot drop a collection
    // the server has already given us.
    if (!collections.value.some((item) => item.id === created.id)) {
      collections.value = [...collections.value, created]
    }
    return created
  } catch (error) {
    if (isStale(startedIn)) return null
    collectionsError.value = safeErrorMessage(error, '创建知识库失败。')
    return null
  } finally {
    if (!isStale(startedIn)) mutating.value = false
  }
}

/**
 * Reads one collection's documents. Every call takes a fresh generation and its
 * own abort signal, so clicking A then B cannot let A's slower response paint
 * over B — the same class of race the transcript guard already handles.
 */
async function loadDocuments(collectionId = activeCollectionId.value) {
  if (!collectionId) return []
  const generation = ++documentsGeneration
  const startedIn = epoch
  if (documentsController) documentsController.abort()
  const controller = new AbortController()
  documentsController = controller

  documents.value = []
  documentsError.value = ''
  documentsLoading.value = true
  try {
    const payload = await listDocuments(collectionId, { signal: controller.signal })
    if (isStale(startedIn) || generation !== documentsGeneration) return []
    documents.value = normalizeDocumentList(payload)
    return documents.value
  } catch (error) {
    // A superseded request was aborted on purpose; that is not a failure the
    // user should ever be told about.
    if (isStale(startedIn) || isAbort(error) || generation !== documentsGeneration) {
      return []
    }
    documentsError.value = safeErrorMessage(error, '文档列表加载失败。')
    return []
  } finally {
    if (documentsController === controller) documentsController = null
    if (!isStale(startedIn) && generation === documentsGeneration) {
      documentsLoading.value = false
    }
  }
}

async function selectCollection(collectionId) {
  const id = String(collectionId ?? '')
  activeCollectionId.value = id
  if (!id) {
    dropActiveCollection()
    return
  }
  await loadDocuments(id)
}

/**
 * Accepts anything shaped like a `File`: the name decides the format and the
 * text is read here so the size ceiling is measured on the decoded characters,
 * which is what the server counts.
 */
async function uploadDocument(collectionId, file) {
  const id = String(collectionId ?? '')
  if (!id) {
    documentsError.value = '请先选择一个知识库。'
    return null
  }
  const name = String(file?.name ?? '')
  const mapped = formatForFile(name)
  if (mapped.error) {
    documentsError.value = mapped.error
    return null
  }

  let content
  try {
    content = typeof file?.text === 'function' ? await file.text() : String(file ?? '')
  } catch {
    documentsError.value = '读取文件内容失败，请重试。'
    return null
  }
  const tooLarge = validateDocumentContent(content)
  if (tooLarge) {
    documentsError.value = tooLarge
    return null
  }

  const startedIn = epoch
  mutating.value = true
  documentsError.value = ''
  try {
    const payload = await addDocument(id, {
      title: name,
      format: mapped.format,
      content,
    })
    if (isStale(startedIn)) return null
    const created = normalizeDocumentResponse(payload)
    if (!created) {
      documentsError.value = '服务没有返回新建文档的标识。'
      return null
    }
    // Only adopt it into the list the user is looking at now, and only if the
    // server filed it under the collection we asked about.
    if (activeCollectionId.value === id && created.collectionId === id) {
      documents.value = mergeDocument(documents.value, created)
    }
    return created
  } catch (error) {
    if (isStale(startedIn)) return null
    if (activeCollectionId.value === id) {
      documentsError.value = safeErrorMessage(error, '上传文档失败。')
    }
    return null
  } finally {
    if (!isStale(startedIn)) mutating.value = false
  }
}

/** Wired for the next phase's editor; no UI offers it yet, by design. */
async function replaceDocument(documentId, { title, format, content }) {
  const id = String(documentId ?? '')
  if (!id) return null
  const tooLarge = validateDocumentContent(content)
  if (tooLarge) {
    documentsError.value = tooLarge
    return null
  }
  const startedIn = epoch
  mutating.value = true
  documentsError.value = ''
  try {
    const payload = await apiReplaceDocument(id, { title, format, content })
    if (isStale(startedIn)) return null
    const replaced = normalizeDocumentResponse(payload)
    if (!replaced) return null
    documents.value = mergeDocument(
      documents.value.map((item) => (item.id === replaced.id ? replaced : item)),
      replaced,
    )
    return replaced
  } catch (error) {
    if (isStale(startedIn)) return null
    documentsError.value = safeErrorMessage(error, '更新文档失败。')
    return null
  } finally {
    if (!isStale(startedIn)) mutating.value = false
  }
}

async function removeDocument(documentId) {
  const id = String(documentId ?? '')
  if (!id) return false
  const startedIn = epoch
  mutating.value = true
  documentsError.value = ''
  try {
    await apiDeleteDocument(id)
    if (isStale(startedIn)) return false
    documents.value = documents.value.filter((item) => item.id !== id)
    return true
  } catch (error) {
    if (isStale(startedIn)) return false
    documentsError.value = safeErrorMessage(error, '删除文档失败。')
    return false
  } finally {
    if (!isStale(startedIn)) mutating.value = false
  }
}

function mergeDocument(list, document) {
  const rest = list.filter((item) => item.id !== document.id)
  return [...rest, document]
}

/**
 * Wipe everything that belongs to the account that just left.
 *
 * Aborting is what stops a request the previous user started from finishing
 * into the next one's screen; the epoch bump is what makes an already-issued
 * response harmless even when its abort arrives too late to matter. Mutations
 * are deliberately *not* aborted mid-flight — a create that the server already
 * committed should not be left unknown to everybody — their results are simply
 * discarded once the epoch has moved on.
 *
 * Exported so a sign-out is not the only caller: tests and any future "leave
 * this account" flow need the same guarantees.
 */
export function resetKnowledgeState() {
  epoch += 1
  collectionsGeneration += 1
  documentsGeneration += 1
  if (collectionsController) {
    const pending = collectionsController
    collectionsController = null
    pending.abort()
  }
  if (documentsController) {
    const pending = documentsController
    documentsController = null
    pending.abort()
  }
  collections.value = []
  collectionsLoading.value = false
  collectionsError.value = ''

  activeCollectionId.value = ''
  documents.value = []
  documentsLoading.value = false
  documentsError.value = ''
  mutating.value = false
}

// One registration, at module load, covers every exit path because they all
// funnel through authStore.signOut(). The auth layer never learns what this
// store holds.
authStore.subscribeSignOut(resetKnowledgeState)

export function useKnowledge() {
  return {
    collections,
    collectionIds,
    collectionsLoading,
    collectionsError,

    activeCollectionId,
    activeCollection,
    documents,
    documentsLoading,
    documentsError,
    mutating,
    loadCollections,
    createCollection,
    selectCollection,
    loadDocuments,
    uploadDocument,
    replaceDocument,
    removeDocument,
    resetKnowledgeState,
    MAX_CONTENT_BYTES,
  }
}
