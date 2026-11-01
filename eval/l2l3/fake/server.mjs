// A fake GopherLearn backend that plays the part of the model.
//
// It speaks only what the provider uses — login, new session, /agent/run, and the
// run-steps read — and answers from a script. It runs no model, does no retrieval and
// listens on the loopback address only, so a trial against it costs nothing and cannot
// reach anything real. Its job is to make every branch of the provider and of the
// assertions happen on demand: a grounded answer, a refusal, a wrong citation, a tool
// that failed, a timeout, a body that is not JSON, a flaky third repeat.
//
// A script is { scenarios: [{ question, reps: [rep, ...] }] }. The n-th time a question
// is asked it uses reps[n] (the last rep repeats). A rep is
//   { body, http?, steps?, stepsHttp?, stepsBody?, rawText?, delayMs?, inspect? }
// where `body` is the /agent/run response fields (status_code defaults to 1000).

import http from 'node:http'

const SUCCESS = 1000

export async function startFakeBackend(script = { scenarios: [] }, { port = 0 } = {}) {
  const byQuestion = new Map()
  for (const scenario of script.scenarios) byQuestion.set(scenario.question, scenario)

  const stats = {
    logins: 0,
    sessions: 0,
    runs: 0,
    stepReads: 0,
    requestIds: [],
    requests: [],
    asked: new Map(), // question -> how many times it was asked
  }
  const sessions = new Map()
  const runs = new Map() // runId -> { rep }
  const seenRequestIds = new Set()

  const send = (res, status, body, raw) => {
    res.writeHead(status, { 'Content-Type': raw ? 'text/plain' : 'application/json' })
    res.end(raw ?? JSON.stringify(body))
  }

  const server = http.createServer(async (req, res) => {
    const chunks = []
    for await (const chunk of req) chunks.push(chunk)
    const text = Buffer.concat(chunks).toString('utf8')
    const url = new URL(req.url, 'http://fake')
    stats.requests.push(`${req.method} ${url.pathname}`)
    let body = {}
    try {
      body = text ? JSON.parse(text) : {}
    } catch {
      body = {}
    }

    if (req.method === 'POST' && url.pathname === '/api/v1/auth/login') {
      stats.logins += 1
      return send(res, 200, { data: { token: 'fake-token' }, error: null })
    }
    // The fake's own read of what a run searched and read, for the doc-constraint tests.
    // It is the fake's, not the product's, so it needs no login.
    const inspectMatch = req.method === 'GET' && url.pathname.match(/^\/fake\/inspect\/([^/]+)$/)
    if (inspectMatch) {
      const run = runs.get(decodeURIComponent(inspectMatch[1]))
      return send(res, run ? 200 : 404, { steps: run?.rep.inspect ?? [] })
    }

    if (req.headers.authorization !== 'Bearer fake-token') return send(res, 401, { error: { code: 'unauthorized' } })

    if (req.method === 'POST' && url.pathname === '/api/v1/sessions') {
      stats.sessions += 1
      const id = `sess-${stats.sessions}`
      sessions.set(id, { mode: body.mode })
      return send(res, 200, { data: { id }, error: null })
    }

    if (req.method === 'POST' && url.pathname === '/api/v1/AI/agent/run') {
      stats.runs += 1
      stats.requestIds.push(body.requestId)
      const replay = seenRequestIds.has(body.requestId)
      seenRequestIds.add(body.requestId)
      const scenario = byQuestion.get(body.question)
      if (!scenario) return send(res, 200, { status_code: 4001, status_msg: 'no script for this question' })
      const n = stats.asked.get(body.question) ?? 0
      stats.asked.set(body.question, n + 1)
      const rep = scenario.reps[Math.min(n, scenario.reps.length - 1)]
      if (rep.delayMs) await new Promise((resolve) => setTimeout(resolve, rep.delayMs))
      if (rep.rawText !== undefined) return send(res, rep.http ?? 200, null, rep.rawText)
      const runId = `run-${stats.runs}`
      runs.set(runId, { rep, sessionId: body.sessionId })
      const response = { status_code: SUCCESS, runId, ...rep.body }
      if (replay) response.replayed = true
      return send(res, rep.http ?? 200, response)
    }

    const stepsMatch = req.method === 'GET' && url.pathname.match(/^\/api\/v1\/AI\/agent\/sessions\/([^/]+)\/runs\/([^/]+)\/steps$/)
    if (stepsMatch) {
      stats.stepReads += 1
      const run = runs.get(decodeURIComponent(stepsMatch[2]))
      if (!run) return send(res, 200, { status_code: 2009 })
      if (run.rep.stepsHttp && run.rep.stepsHttp !== 200) return send(res, run.rep.stepsHttp, run.rep.stepsBody ?? {})
      if (run.rep.stepsBody) return send(res, 200, run.rep.stepsBody)
      return send(res, 200, { status_code: SUCCESS, run: { run_id: stepsMatch[2], status: 'completed', steps: run.rep.steps ?? [] } })
    }

    return send(res, 404, { error: { code: 'not_found' } })
  })

  await new Promise((resolve) => server.listen(port, '127.0.0.1', resolve))
  const address = server.address()
  return {
    url: `http://127.0.0.1:${address.port}`,
    stats,
    close: () => new Promise((resolve) => server.close(resolve)),
  }
}

/** The fake's implementation of the inspector the doc constraint reads through. */
export function fakeInspector(baseUrl) {
  return {
    async inspect(runId) {
      const response = await fetch(`${baseUrl}/fake/inspect/${encodeURIComponent(runId)}`)
      if (!response.ok) throw new Error(`inspect ${runId}: http ${response.status}`)
      return response.json()
    },
  }
}
