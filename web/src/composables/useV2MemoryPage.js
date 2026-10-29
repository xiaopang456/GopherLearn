import { ref } from 'vue'
import { listV2Memory, revisionsV2Memory } from '../api/v2Memory.js'
import { useV2MemoryOperation } from './useV2MemoryOperation.js'

export function useV2MemoryPage() {
  const operation = useV2MemoryOperation()
  const memories = ref([]), drafts = ref([]), revisions = ref(null), readError = ref(null)
  let generation = 0
  async function refresh(filter = {}) {
    const current = ++generation; readError.value = null
    try {
      const response = await listV2Memory(filter)
      if (current === generation) { memories.value = response.memories; drafts.value = response.pendingDrafts }
    } catch (cause) { if (current === generation) readError.value = { message: cause.message, code: cause.code } }
  }
  async function showRevisions(id) {
    revisions.value = null; readError.value = null
    const current = ++generation
    try {
      const response = await revisionsV2Memory(id)
      if (current === generation) revisions.value = { id: String(id), items: response.revisions }
    } catch (cause) { if (current === generation) readError.value = { message: cause.message, code: cause.code } }
  }
  function dispose() { ++generation; operation.reset() }
  return { operation, memories, drafts, revisions, readError, refresh, showRevisions, dispose }
}
