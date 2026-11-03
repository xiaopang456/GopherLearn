// One trial against the system under test: log in, open a new session, ask once, read
// back the steps the run took, and keep everything as a raw record.
//
// Three promises, each pinned by a test:
//   * It never spends what it was not allowed to. The environment must be named, a
//     fake run may only talk to a loopback address, a real run needs a budget above
//     zero, and the budget is written to the ledger *before* the request goes out.
//   * It never retries. A failed call is a record, not a second attempt; a retry would
//     make a flaky system look better than it is and cost more than was authorised.
//   * It never loses a failure. Timeouts, bad bodies and an unreadable step list all
//     become records with the reason, so scoring can count them.

import crypto from 'node:crypto'
import fs from 'node:fs'
import path from 'node:path'

import { assertOutsideWorktree } from '../lib/pathguard.mjs'
import { RECORD_SCHEMA } from '../lib/record.mjs'
import { SUCCESS_CODE } from '../lib/enums.mjs'

const LOOPBACK = new Set(['127.0.0.1', 'localhost', '[::1]', '::1'])

/**
 * Read the provider's settings from its configuration and the environment. Nothing has
 * a default that could spend: the environment is required, and the run budget is 0.
 */
export function resolveConfig(config = {}, env = process.env) {
  const pick = (key, envName, fallback) => config[key] ?? env[envName] ?? fallback
  return {
    env: pick('env', 'GL_L23_ENV'),
    baseUrl: String(pick('baseUrl', 'GL_L23_BASE_URL', '')).replace(/\/+$/, ''),
    email: pick('email', 'GL_L23_EMAIL'),
    password: pick('password', 'GL_L23_PASSWORD'),
    knowledgePolicy: pick('knowledgePolicy', 'GL_L23_POLICY', 'knowledge_preferred'),
    maxRuns: Number.parseInt(String(pick('maxRuns', 'GL_L23_MAX_RUNS', '0')), 10),
    ledgerPath: pick('ledgerPath', 'GL_L23_LEDGER'),
    rawDir: pick('rawDir', 'GL_L23_RAW_DIR'),
    timeoutMs: Number.parseInt(String(pick('timeoutMs', 'GL_L23_TIMEOUT_MS', '120000')), 10),
    // Whether each request says the model should think. Left out unless a boolean is given;
    // then it is sent explicitly on every request, whatever the deployment default is.
    enableThinking: parseThinking(pick('enableThinking', 'GL_L23_ENABLE_THINKING')),
    // A file whose existence means "stop": whoever watches the run (the counting proxy, a person)
    // creates it, and no further request is made while it is there.
    stopFile: pick('stopFile', 'GL_L23_STOP_FILE'),
    // Whether the session a trial opened is deleted once the trial is over. The product keeps at most
    // 100 sessions per user, so a stage of more than 100 trials cannot run without it. On unless false.
    deleteSessions: parseFlag(pick('deleteSessions', 'GL_L23_DELETE_SESSIONS'), true),
  }
}

/** A boolean setting: true / false (or their strings); unset takes the default; anything else is kept so it can be refused. */
function parseFlag(value, fallback) {
  if (value === undefined || value === null || value === '') return fallback
  if (value === true || value === 'true') return true
  if (value === false || value === 'false') return false
  return value
}

/** true / false (or their strings) are choices; unset stays undefined; anything else is kept so it can be refused. */
function parseThinking(value) {
  if (value === undefined || value === null || value === '') return undefined
  if (value === true || value === 'true') return true
  if (value === false || value === 'false') return false
  return value
}

/** Why this configuration may not run, or '' when it may. */
export function configProblem(cfg) {
  if (cfg.env !== 'fake' && cfg.env !== 'real') return 'GL_L23_ENV must be set to "fake" or "real"; there is no default, so a real run is never started by accident'
  let url
  try {
    url = new URL(cfg.baseUrl)
  } catch {
    return `baseUrl ${JSON.stringify(cfg.baseUrl)} is not a URL`
  }
  if (cfg.env === 'fake' && !LOOPBACK.has(url.hostname)) return `a fake run may only talk to a loopback address, not ${url.hostname}`
  if (cfg.env === 'real' && !(cfg.maxRuns > 0)) return 'a real run needs GL_L23_MAX_RUNS above zero'
  if (!Number.isInteger(cfg.maxRuns) || cfg.maxRuns < 0) return 'maxRuns must be a non-negative integer'
  if (cfg.enableThinking !== undefined && typeof cfg.enableThinking !== 'boolean') return `enableThinking must be true or false when given, not ${JSON.stringify(cfg.enableThinking)}`
  if (typeof cfg.deleteSessions !== 'boolean') return `deleteSessions must be true or false when given, not ${JSON.stringify(cfg.deleteSessions)}`
  for (const [name, value] of [['ledgerPath', cfg.ledgerPath], ['rawDir', cfg.rawDir]]) {
    try {
      assertOutsideWorktree(name, value)
    } catch (error) {
      return error.message
    }
  }
  return ''
}

const safeName = (value) => String(value).replace(/[^A-Za-z0-9_.-]/g, '_')

async function jsonFetch(url, init, timeoutMs) {
  const response = await fetch(url, { ...init, signal: AbortSignal.timeout(timeoutMs) })
  const text = await response.text()
  let body
  try {
    body = JSON.parse(text)
  } catch {
    body = { raw: text.slice(0, 300) }
  }
  return { status: response.status, body }
}

export class TrialRunner {
  constructor(config, { fakeSessionProvider } = {}) {
    this.cfg = config
    this.token = null
    this.fakeSessionProvider = fakeSessionProvider
  }

  ledgerRead() {
    if (!fs.existsSync(this.cfg.ledgerPath)) return []
    return fs.readFileSync(this.cfg.ledgerPath, 'utf8').split('\n').filter(Boolean).map((line) => JSON.parse(line))
  }

  ledgerAppend(entry) {
    fs.mkdirSync(path.dirname(this.cfg.ledgerPath), { recursive: true })
    fs.appendFileSync(this.cfg.ledgerPath, `${JSON.stringify({ at: new Date().toISOString(), ...entry })}\n`)
  }

  async login() {
    if (!this.cfg.email || !this.cfg.password) throw new Error('email and password are not set')
    const r = await jsonFetch(`${this.cfg.baseUrl}/api/v1/auth/login`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ email: this.cfg.email, password: this.cfg.password }),
    }, 15000)
    if (r.status !== 200 || !r.body?.data?.token) throw new Error(`login failed: http ${r.status}`)
    this.token = r.body.data.token
  }

  /**
   * The id of the run a session holds, read from the session's run list. A run that failed after it started
   * answers without its id, but the run exists; each trial has a session of its own, so the newest run is its run.
   * Returns '' when it cannot be found. Reading the list calls no model.
   */
  async latestRunId(auth, sessionId) {
    try {
      const list = await jsonFetch(`${this.cfg.baseUrl}/api/v1/AI/agent/sessions/${encodeURIComponent(sessionId)}/runs`, { method: 'GET', headers: auth }, 15000)
      const runs = list.status === 200 && list.body?.status_code === SUCCESS_CODE && Array.isArray(list.body.runs) ? list.body.runs : []
      const newest = [...runs].filter((r) => typeof r?.run_id === 'string' && r.run_id !== '').sort((a, b) => String(a.started_at ?? '').localeCompare(String(b.started_at ?? ''))).at(-1)
      return newest?.run_id ?? ''
    } catch {
      return ''
    }
  }

  /** Delete the trial's session through the product's own API. A failure is written to the ledger, never thrown. */
  async deleteSession(auth, sessionId) {
    try {
      const r = await jsonFetch(`${this.cfg.baseUrl}/api/v1/sessions/${encodeURIComponent(sessionId)}`, { method: 'DELETE', headers: auth }, 15000)
      this.ledgerAppend({ phase: 'cleanup', sessionId, http: r.status })
    } catch (error) {
      this.ledgerAppend({ phase: 'cleanup', sessionId, error: String(error?.message ?? error).slice(0, 160) })
    }
  }

  writeRecord(record) {
    const dir = path.join(this.cfg.rawDir, safeName(record.arm))
    fs.mkdirSync(dir, { recursive: true })
    const file = path.join(dir, `${safeName(record.qid)}.r${record.rep}.json`)
    fs.writeFileSync(file, `${JSON.stringify(record, null, 2)}\n`)
    return file
  }

  /** Ask one question once. Always returns a record (and has written it), never throws. */
  async run({ arm, qid, rep, question }) {
    const base = { schema: RECORD_SCHEMA, arm, qid, rep, env: this.cfg.env }
    const started = Date.now()
    const finish = (extra) => {
      const record = { ...base, durationMs: Date.now() - started, ...extra }
      this.writeRecord(record)
      return record
    }

    const problem = configProblem(this.cfg)
    if (problem) {
      // Nothing is sent, and nothing is written outside a checked directory.
      return { ...base, error: `refused: ${problem}`, durationMs: 0 }
    }
    if (this.cfg.stopFile && fs.existsSync(this.cfg.stopFile)) {
      // Nothing is sent and nothing is recorded as a run: the trial simply did not happen.
      return { ...base, error: `refused: the stop file ${this.cfg.stopFile} exists; the run was stopped`, durationMs: 0 }
    }
    if (this.fakeSessionProvider && this.cfg.env !== 'fake') {
      return finish({ error: 'refused: injected sessions are currently supported only with a fake backend' })
    }
    let sessionId = null
    let auth = null
    try {
      if (!this.token) await this.login()
      auth = { Authorization: `Bearer ${this.token}`, 'Content-Type': 'application/json' }
      const nonce = crypto.randomBytes(6).toString('hex')

      // A new session per trial: a session's history reaches the next prompt and can
      // trigger a summary, which would make trials depend on their neighbours.
      const session = this.fakeSessionProvider ? await this.fakeSessionProvider({ arm, qid, rep }) : await jsonFetch(`${this.cfg.baseUrl}/api/v1/sessions`, {
        method: 'POST',
        headers: auth,
        body: JSON.stringify({ mode: 'learning_agent', title: `l23 ${arm} ${qid}`, requestId: `s-${safeName(arm)}-${safeName(qid)}-${rep}-${nonce}` }),
      }, 15000)
      sessionId = session.body?.data?.id
      if (session.status !== 200 || !sessionId) {
        return finish({ error: `create session failed: http ${session.status}` })
      }

      if (this.cfg.stopFile && fs.existsSync(this.cfg.stopFile)) {
        return finish({ error: 'refused: the stop file appeared while preparing the session' })
      }

      const used = this.ledgerRead().filter((e) => e.phase === 'run').length
      if (!(used < this.cfg.maxRuns)) {
        this.ledgerAppend({ phase: 'refused', arm, qid, rep, used, limit: this.cfg.maxRuns })
        return finish({ error: `budget guard: ${used} runs already recorded, limit ${this.cfg.maxRuns}; /agent/run not called` })
      }
      // Recorded before the request: an attempt is a possible cost whether or not it returns.
      this.ledgerAppend({ phase: 'run', arm, qid, rep, sessionId })

      const run = await jsonFetch(`${this.cfg.baseUrl}/api/v1/AI/agent/run`, {
        method: 'POST',
        headers: auth,
        // The same requestId would be taken for a replay and not call the model again.
        body: JSON.stringify({
          sessionId,
          question,
          requestId: `r-${safeName(arm)}-${safeName(qid)}-${rep}-${nonce}`,
          knowledgePolicy: this.cfg.knowledgePolicy,
          ...(this.cfg.enableThinking === undefined ? {} : { enableThinking: this.cfg.enableThinking }),
        }),
      }, this.cfg.timeoutMs)
      const b = run.body ?? {}
      // The moment the run's turn ended. It is the boundary the answer is
      // split at: calls the product sent before it are part of producing the
      // answer, calls it sent afterwards are background work that the answer
      // caused. A run that did not answer has no boundary.
      const answered = run.status === 200 && b.status_code === SUCCESS_CODE
      const answeredAt = answered ? new Date().toISOString() : null
      this.ledgerAppend({ phase: 'result', arm, qid, rep, http: run.status, statusCode: b.status_code, modelCalls: b.modelCalls, toolCalls: b.toolCalls, runId: b.runId, answeredAt })

      const record = {
        http: run.status,
        statusCode: b.status_code,
        runId: b.runId,
        // The identity of the trial: which session it was made in, and when
        // its answer came back. A model call is accounted for by the run it
        // belongs to, so a record has to say which run that was.
        sessionId,
        answeredAt,
        replayed: b.replayed === true,
        answer: b.answer ?? '',
        answerBasis: b.answerBasis,
        knowledgeStatus: b.knowledgeStatus,
        knowledgeReason: b.knowledgeReason,
        sources: b.sources ?? [],
        toolCalls: b.toolCalls,
        modelCalls: b.modelCalls,
        projectedInput: b.contextBudget?.projectedInput,
        memoryDraft: b.memoryDraft != null,
        // What the request said about thinking: true / false as sent, null when it left the choice to the deployment.
        enableThinking: this.cfg.enableThinking ?? null,
      }
      // The service's own sentence for a failure. Several causes share it, so the run
      // view below is what says which one it was.
      if (typeof b.status_msg === 'string' && b.status_msg !== '') record.statusMsg = b.status_msg.slice(0, 200)
      const failed = run.status !== 200 || b.status_code !== SUCCESS_CODE

      // A run that failed after it started (5003) answers without its id. The session still holds the run,
      // so its id is taken from the session's run list; without it the run view below cannot be read.
      let runId = b.runId
      if (failed && !runId && run.status === 200) {
        const found = await this.latestRunId(auth, sessionId)
        if (found) {
          runId = found
          record.runId = found
          record.runIdFrom = 'session_runs'
        }
      }

      // What the turn did, by name, and how it ended. Reading it calls no model. A run that
      // failed after it started has a run view too, and its termination reason is the only
      // place that tells "unknown_tool" from the other failures that share a status code.
      if (failed && !(run.status === 200 && runId)) {
        return finish({ ...record, error: `agent/run failed: http ${run.status} status_code ${b.status_code}` })
      }
      this.ledgerAppend({ phase: 'steps', arm, qid, rep, runId })
      try {
        const steps = await jsonFetch(`${this.cfg.baseUrl}/api/v1/AI/agent/sessions/${encodeURIComponent(sessionId)}/runs/${encodeURIComponent(runId)}/steps`, {
          method: 'GET',
          headers: auth,
        }, 15000)
        const view = steps.body?.run
        if (steps.status === 200 && steps.body?.status_code === SUCCESS_CODE && Array.isArray(view?.steps)) {
          record.steps = view.steps
          if (typeof view.status === 'string') record.runStatus = view.status
          if (typeof view.termination_reason === 'string' && view.termination_reason !== '') record.terminationReason = view.termination_reason.slice(0, 64)
        } else {
          record.stepsError = `http ${steps.status} status_code ${steps.body?.status_code}`
        }
      } catch (error) {
        record.stepsError = String(error?.message ?? error).slice(0, 160)
      }
      if (failed) return finish({ ...record, error: `agent/run failed: http ${run.status} status_code ${b.status_code}` })
      return finish(record)
    } catch (error) {
      this.ledgerAppend({ phase: 'exception', arm, qid, rep, message: String(error?.message ?? error).slice(0, 160) })
      return finish({ error: `provider exception: ${String(error?.message ?? error).slice(0, 160)}` })
    } finally {
      // After the record is written: the run view was read above, so nothing needs the session any more.
      if (sessionId && auth && this.cfg.deleteSessions) await this.deleteSession(auth, sessionId)
    }
  }
}
