import { ApiError, buildUrl, currentToken, isAbort, reportIfRejected } from './http.js'

/**
 * The read side of an Agent turn.
 *
 * The steps a turn took are persisted row by row while it runs, so a conversation that
 * is opened again can show them. This module is the only place that knows how to ask
 * for them, and it asks on the native v2 surface — the same surface the memory page
 * uses — because that is where the run read lives.
 *
 * Two facts about that surface are worth stating rather than discovering later:
 *
 *  - **A missing run is not a 404.** The native surface reports every outcome with
 *    HTTP 200 and a `status_code` in the body, so "this session is not yours" and
 *    "this session does not exist" both arrive as `status_code: 2009`. The two are
 *    deliberately the same answer: a caller must not be able to tell them apart.
 *  - **The rows carry only what the live stream already published** — the sequence,
 *    the step kind, the tool name, the duration — plus a status the server derived.
 *    The tool's arguments, the tool's observation and the raw error text stay in the
 *    database, because they can carry the user's own question and retrieved passages.
 *
 * So a step read back here is the same fact the live panel showed, and nothing more.
 */

export const AGENT_RUNS_PATH = '/api/v1/AI/agent/sessions'

/** The native surface's success code. Every other outcome is a failure code. */
export const NATIVE_SUCCESS = 1000

/** The native surface's "no such record, or not yours to read". */
export const NATIVE_RECORD_NOT_FOUND = 2009

export function sessionRunsPath(sessionId) {
  return `${AGENT_RUNS_PATH}/${encodeURIComponent(String(sessionId))}/runs`
}

export function runStepsPath(sessionId, runId) {
  return `${sessionRunsPath(sessionId)}/${encodeURIComponent(String(runId))}/steps`
}

/**
 * One request against the native surface, with its envelope.
 *
 * The bearer travels in a header, never in the URL. A non-1000 `status_code` becomes
 * an `ApiError` that keeps the native code on `error.nativeCode`, so a caller can
 * tell "not found" from "the store is busy" without the message telling anybody.
 */
export async function nativeRunRequest(path, { signal } = {}) {
  const token = currentToken()
  let response
  try {
    response = await fetch(buildUrl(path), {
      method: 'GET',
      signal,
      headers: {
        accept: 'application/json',
        ...(token ? { authorization: `Bearer ${token}` } : {}),
      },
    })
  } catch (error) {
    if (isAbort(error)) throw error
    throw new ApiError('无法读取这次会话的执行过程。', { code: 'network_error', path })
  }

  let result = null
  try {
    result = JSON.parse(await response.text())
  } catch {
    result = null
  }

  const nativeCode = result?.status_code
  if (nativeCode === NATIVE_SUCCESS) return result

  const error = new ApiError(result?.status_msg || '执行过程读取未完成。', {
    status: response.status,
    code: `v2_${nativeCode ?? response.status}`,
    path,
  })
  error.nativeCode = nativeCode
  throw reportIfRejected(error)
}

function readSteps(rows) {
  if (!Array.isArray(rows)) return []
  return rows.flatMap((row) => {
    if (!row || typeof row !== 'object') return []
    const sequence = Number(row.sequence)
    if (!Number.isInteger(sequence) || sequence <= 0) return []
    return [
      {
        sequence,
        stepType: String(row.step_type ?? ''),
        toolName: String(row.tool_name ?? ''),
        durationMillis: Number.isFinite(Number(row.duration_millis))
          ? Math.max(0, Math.floor(Number(row.duration_millis)))
          : 0,
        status: String(row.status ?? ''),
      },
    ]
  })
}

/**
 * One run, in the shape this client keeps it in.
 *
 * The fields that are not read here are not on the wire: the lease token, the request
 * key, the input hash and the stored result JSON stay in the server. A row that does
 * not carry an id is dropped rather than shown under an empty heading.
 */
function normalizeRun(raw) {
  if (!raw || typeof raw !== 'object') return null
  const runId = String(raw.run_id ?? '')
  if (runId === '') return null
  return {
    runId,
    status: String(raw.status ?? ''),
    executionKind: String(raw.execution_kind ?? ''),
    startedAt: String(raw.started_at ?? ''),
    finishedAt: String(raw.finished_at ?? ''),
    terminationReason: String(raw.termination_reason ?? ''),
    steps: readSteps(raw.steps),
  }
}

/**
 * The runs of one session, oldest first, with the steps each took.
 *
 * `truncated` says the list is not the whole story: the session holds more turns
 * than one read returns, and the older ones were left out. A caller that sees it can
 * say so instead of pretending the list is complete.
 */
export async function listSessionRuns(sessionId, { signal } = {}) {
  const result = await nativeRunRequest(sessionRunsPath(sessionId), { signal })
  const runs = Array.isArray(result?.runs) ? result.runs : []
  return {
    runs: runs.map(normalizeRun).filter(Boolean),
    truncated: result?.truncated === true,
  }
}

/** The steps of one run, for a reader that already knows which run it wants. */
export async function listRunSteps(sessionId, runId, { signal } = {}) {
  const result = await nativeRunRequest(runStepsPath(sessionId, runId), { signal })
  const run = normalizeRun(result?.run)
  return { run, steps: run ? run.steps : [] }
}
