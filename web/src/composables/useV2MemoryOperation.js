import { ref } from 'vue'
import { createV2Memory, correctV2Memory, archiveV2Memory, deleteV2Memory, resolveV2Draft } from '../api/v2Memory.js'
import { localId } from '../lib/id.js'

// Request presentation only. Original services decide states/conflicts/replay.
export function useV2MemoryOperation() {
  const clone = (value) => JSON.parse(JSON.stringify(value)) // Snapshot exactly the native JSON wire, including Vue reactive inputs.
  const busy = ref(false), error = ref(null), result = ref(null), pending = ref(null)
  let generation = 0, controller
  const operations = {
    create: (op, signal) => createV2Memory(op.body, signal),
    correct: (op, signal) => correctV2Memory(op.id, op.body, signal),
    archive: (op, signal) => archiveV2Memory(op.id, op.body.reason, signal),
    delete: (op, signal) => deleteV2Memory(op.id, op.body.reason, signal),
    resolve: (op, signal) => resolveV2Draft(op.id, op.body, signal),
  }
  async function send(op) {
    if (busy.value) return null
    const execute = operations[op.kind]
    if (!execute) throw new Error('未知治理操作')
    const current = ++generation
    controller = new AbortController()
    busy.value = true; error.value = null; result.value = null
    pending.value = clone(op)
    try {
      const response = await execute(op, controller.signal)
      if (current !== generation) return null
      result.value = response
      return response
    } catch (cause) {
      if (current === generation) error.value = { message: cause.name === 'AbortError' ? '请求已取消；服务端状态尚未确认。' : cause.message, code: cause.code || cause.name }
      return null
    } finally { if (current === generation) busy.value = false }
  }
  function start(kind, body, id) {
    if (busy.value) return Promise.resolve(null)
    const request = clone(body)
    // This is a client operation key, never a memory/draft business identity.
    if (kind === 'create') request.idempotency_key = localId('v2-memory')
    return send({ kind, body: request, ...(id === undefined ? {} : { id: String(id) }) })
  }
  function retry() { return pending.value ? send(clone(pending.value)) : Promise.resolve(null) }
  function cancel() {
    ++generation; controller?.abort(); busy.value = false; result.value = null
    error.value = { message: '请求已取消；未确认服务端状态，可明确重试原请求。', code: 'AbortError' }
  }
  function reset() { cancel(); pending.value = null; result.value = null; error.value = null }
  return { busy, error, result, pending, start, retry, cancel, reset }
}
