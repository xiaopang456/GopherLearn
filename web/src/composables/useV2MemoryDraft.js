import { ref, watch } from 'vue'
import { useV2MemoryOperation } from './useV2MemoryOperation.js'

export const v2DraftId = (draft) => String(draft?.draftId ?? draft?.id ?? draft?.draft_id ?? '')
export const v2ConflictIds = (draft) => draft?.conflictIds ?? draft?.conflict_ids ?? []

export function useV2MemoryDraft(getDraft, getRunId = () => '') {
  const operation = useV2MemoryOperation(), outcome = ref(null)
  const stop = watch(() => JSON.stringify([v2DraftId(getDraft()), getRunId()]), () => {
    operation.reset(); outcome.value = null
  }, { flush: 'sync' })
  async function publish(request) {
    outcome.value = null
    const id = v2DraftId(getDraft())
    const response = await request()
    if (!response || id !== v2DraftId(getDraft())) return null
    if (v2DraftId(response.draft) !== id) {
      operation.error.value = { message: '原接口返回的草稿身份不一致；未显示成功。', code: 'invalid_response' }
      return null
    }
    // Use returned status/memory/revision/replay, never the clicked action.
    outcome.value = response
    return response
  }
  function decide(action, reason = '', target = '') {
    if (operation.busy.value) return Promise.resolve(null)
    if (!['add', 'replace', 'cancel'].includes(action) || !v2DraftId(getDraft())) return Promise.resolve(null)
    if ((action !== 'cancel' && !reason.trim()) || (action === 'replace' && !v2ConflictIds(getDraft()).includes(target))) return Promise.resolve(null)
    return publish(() => operation.start('resolve', { action, reason, ...(action === 'replace' ? { target_memory_id: target } : {}) }, v2DraftId(getDraft())))
  }
  function retry() {
    if (operation.busy.value || operation.pending.value?.id !== v2DraftId(getDraft())) return Promise.resolve(null)
    return publish(operation.retry)
  }
  function cancel() { operation.cancel(); outcome.value = null }
  function dispose() { stop(); operation.reset(); outcome.value = null }
  return { operation, outcome, decide, retry, cancel, dispose }
}
