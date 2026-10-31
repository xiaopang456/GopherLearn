import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import AgentResult from '../src/components/AgentResult.vue'
import CitationList from '../src/components/CitationList.vue'
import { normalizeSourceList } from '../src/lib/sources.js'
import { useChat } from '../src/composables/useChat.js'
import { setTokenProvider } from '../src/api/http.js'
import { jsonResponse, sseFrame, sseResponse } from './helpers/transport.js'

const BIG = '9007199254740993123'
const original = { runId: 'real-run', answer: '落库正文', answerBasis: 'model',
  knowledgeStatus: 'not_requested', contextBudget: { history: 1 }, modelCalls: 1,
  sources: [{ citation: 1, doc_id: 'original-doc', doc_version: 7,
    file_name: '原文', title_path: ['标题'], start_line: 2, end_line: 3, snippet: '证据' }],
  usedTopicKeys: ['confirmed-topic'], replayed: true }
/**
 * A result whose three fields carry the server's own values, so the card has
 * something specific to say. `original` above carries two values this client
 * cannot name — that is the point of the second card test.
 */
const informed = { ...original, answerBasis: 'personal_knowledge',
  knowledgeStatus: 'no_evidence', knowledgeReason: 'no_candidate' }
let chat, bodies, createBodies, handleStream, handleCreate
beforeEach(() => {
  vi.stubEnv('VITE_API_BASE_URL', 'https://api.test')
  setTokenProvider(() => 'synthetic-test-token')
  bodies = []; createBodies = []
  handleCreate = async (body) => jsonResponse({ data: { id: BIG, title: body.title, mode: body.mode } })
  handleStream = async () => sseResponse([sseFrame('message.started', { run_id: 'real-run' }),
    sseFrame('message.completed', { run_id: 'real-run', message_id: BIG, content: '落库正文', original_v2: original, sources: original.sources })])
  vi.stubGlobal('fetch', async (url, init = {}) => {
    const path = new URL(url).pathname
    if (path.endsWith('/stream')) { const body = JSON.parse(init.body); bodies.push(body); return handleStream(body, init) }
    if (init.method === 'POST') { const body = JSON.parse(init.body); createBodies.push(body); return handleCreate(body) }
    if (path.endsWith('/messages')) return jsonResponse({ data: { items: [{ id: BIG, role: 'assistant', content: '落库正文', sources: original.sources }] } })
    return jsonResponse({ data: { items: [{ id: BIG, mode: 'learning_agent', title: '学习 Agent' }, { id: 'normal', mode: 'chat', title: '对话' }] } })
  })
  chat = useChat()
})
afterEach(() => { vi.unstubAllGlobals(); vi.unstubAllEnvs() })

it('creation uses its own immutable key; duplicate clicks suppressed; explicit retry reuses it', async () => {
  let reject
  handleCreate = () => new Promise((_resolve, fail) => { reject = fail })
  const pending = chat.createSession('learning_agent')
  await chat.createSession('learning_agent')
  expect(createBodies).toHaveLength(1)
  reject(new Error('lost response')); await pending
  const first = structuredClone(createBodies[0])
  handleCreate = async (body) => jsonResponse({ data: { id: BIG, mode: body.mode } })
  await chat.retryCreate()
  expect(createBodies[1]).toEqual(first)
  await chat.createSession('learning_agent')
  expect(createBodies[2].requestId).not.toBe(first.requestId)
  await chat.send('问题')
  expect(bodies[0].requestId).not.toBe(createBodies[2].requestId)
  expect(bodies[0]).toEqual({ content: '问题', requestId: expect.stringMatching(/^send-/), stream_steps: true })
})

it('failed/busy operation retries original fields; new operation gets a fresh key with no default consent', async () => {
  await chat.loadSessions(); await chat.openSession(BIG)
  handleStream = async () => sseResponse([sseFrame('message.failed', { code: 'agent_run_busy', message: 'busy' })])
  await chat.send('原请求', null, { knowledgePolicy: { mode: 'explicit' }, topicConsent: { ticket: 'original-ticket', selectedTopicKeys: ['t'] } })
  const first = structuredClone(bodies[0])
  const transcriptCount = chat.messages.value.length
  expect(chat.run.value.error.code).toBe('agent_run_busy')
  chat.draft.value = '不能替换原重试'
  await chat.retrySend()
  expect(bodies[1]).toEqual(first)
  expect(chat.messages.value).toHaveLength(transcriptCount)
  await chat.send('新操作')
  expect(bodies[2].requestId).not.toBe(first.requestId)
  expect(bodies[2]).not.toHaveProperty('knowledgePolicy')
  expect(bodies[2]).not.toHaveProperty('topicConsent')
})

it.each([undefined, 'wrong', 'old-candidate'])('persisted replacement wins without valid matching final block (%s)', async (finalId) => {
  await chat.loadSessions(); await chat.openSession(BIG)
  handleStream = async () => sseResponse([
    sseFrame('message.started', { run_id: 'real-run' }),
    sseFrame('agent.text.started', { run_id: 'real-run', block_id: 'old-candidate', round: 1 }),
    sseFrame('agent.text.delta', { run_id: 'real-run', block_id: 'old-candidate', content: '错误候选' }),
    sseFrame('agent.text.ended', { run_id: 'real-run', block_id: 'old-candidate', kind: 'answer_candidate' }),
    sseFrame('message.completed', { run_id: 'real-run', message_id: BIG, content: '落库正文', final_block_id: finalId, original_v2: original, sources: original.sources }),
  ])
  await chat.send('问题')
  const reply = chat.messages.value.at(-1)
  expect(reply.content).toBe('落库正文')
  expect(reply.serverId).toBe(BIG)
  expect(reply.runId).toBe('real-run')
  expect(reply.originalV2).toEqual(original)
  expect(reply.sources[0]).toMatchObject({ documentId: 'original-doc', documentVersion: '7', collectionId: '', chunkId: '', navigationUnavailable: true })
  chat.closeSession(); await chat.openSession(BIG)
  expect(chat.messages.value[0].id).toBe(BIG)
  expect(chat.messages.value[0].content).toBe(reply.content)
})

it('normal Chat keeps the original request and streaming contract', async () => {
  await chat.loadSessions(); await chat.openSession('normal')
  handleStream = async () => sseResponse([sseFrame('message.delta', { delta: '普通' }), sseFrame('message.completed', { content: '普通回答' })])
  await chat.send('普通问题', null, { knowledgePolicy: { mode: 'all' }, topicConsent: { ticket: 'must-not-send' } })
  expect(bodies).toEqual([{ content: '普通问题' }])
  expect(chat.messages.value.at(-1).content).toBe('普通回答')
})

it('process is never confirmed and repaired candidate alone becomes final', async () => {
  await chat.loadSessions(); await chat.openSession(BIG)
  const block = (id, kind, text, round) => [
    sseFrame('agent.text.started', { run_id: 'real-run', block_id: id, round }),
    sseFrame('agent.text.delta', { run_id: 'real-run', block_id: id, content: text }),
    sseFrame('agent.text.ended', { run_id: 'real-run', block_id: id, kind }),
  ]
  handleStream = async () => sseResponse([
    sseFrame('message.started', { run_id: 'real-run' }),
    ...block('tool-round', 'process', '公开过程', 1),
    ...block('invalid', 'answer_candidate', '引用错误答案', 2),
    ...block('repaired', 'answer_candidate', '落库正文', 3),
    sseFrame('message.completed', { run_id: 'real-run', message_id: BIG, content: '落库正文', final_block_id: 'repaired', original_v2: original }),
  ])
  await chat.send('问题')
  const reply = chat.messages.value.at(-1)
  expect(reply.content).toBe('落库正文')
  expect(reply.processNotes).toEqual([{ blockId: 'tool-round', round: 1, text: '公开过程' }])
  expect(reply.content).not.toContain('引用错误答案')
})

it('a final_block_id that names process does not confirm it', async () => {
  await chat.loadSessions(); await chat.openSession(BIG)
  handleStream = async () => sseResponse([
    sseFrame('message.started', { run_id: 'real-run' }),
    sseFrame('agent.text.started', { run_id: 'real-run', block_id: 'process', round: 1 }),
    sseFrame('agent.text.delta', { run_id: 'real-run', block_id: 'process', content: '落库正文' }),
    sseFrame('agent.text.ended', { run_id: 'real-run', block_id: 'process', kind: 'process' }),
    sseFrame('message.completed', { run_id: 'real-run', content: '落库正文', final_block_id: 'process' }),
  ])
  await chat.send('问题')
  expect(chat.messages.value.at(-1).processNotes).toEqual([])
  expect(chat.messages.value.at(-1).content).toBe('落库正文')
})

it('duplicate sends are suppressed; disconnect is failure and cancelled callbacks stay isolated', async () => {
  await chat.loadSessions(); await chat.openSession(BIG)
  let release
  handleStream = () => new Promise((resolve) => { release = resolve })
  const pending = chat.send('同一次')
  await chat.send('双击')
  expect(bodies).toHaveLength(1)
  chat.cancel()
  handleStream = async () => sseResponse([sseFrame('message.started', { run_id: 'new-run' })])
  await chat.send('下一轮')
  expect(chat.run.value.error.code).toBe('stream_closed')
  release(sseResponse([sseFrame('message.completed', { run_id: 'old-run', content: '不得污染' })]))
  await pending
  expect(chat.messages.value.at(-1).content).not.toContain('不得污染')
  expect(bodies[1].requestId).not.toBe(bodies[0].requestId)
})

it('the learner view names the basis in plain words and keeps internal identifiers out', async () => {
  vi.stubEnv('DEV', false)
  const result = mount(AgentResult, { props: { result: informed, runId: 'real-run', messageId: BIG } })
  const text = result.text()
  expect(text).toContain('来自你上传的资料')
  expect(text).toContain('资料里没有找到支撑这句话的内容')
  expect(text).toContain('资料里没有匹配的段落')
  expect(text).toContain('原请求重放')
  // The server's own words are not the learner's: they are translated, not shown.
  expect(text).not.toContain('personal_knowledge')
  expect(text).not.toContain('no_evidence')
  expect(text).not.toContain('no_candidate')
  // Neither is anything that identifies the run or the message.
  expect(text).not.toContain('real-run')
  expect(text).not.toContain(BIG)
  expect(result.find('[data-test="agent-result-debug"]').exists()).toBe(false)
  result.unmount()
})

it('a value this client cannot name is worded neutrally instead of shown raw', async () => {
  vi.stubEnv('DEV', false)
  const result = mount(AgentResult, { props: { result: original, runId: 'real-run', messageId: BIG } })
  const text = result.text()
  expect(text).toContain('未说明')
  expect(text).not.toContain('model')
  expect(text).not.toContain('not_requested')
  expect(text).not.toContain('real-run')
  expect(text).not.toContain(BIG)
  result.unmount()
})

it('debug mode still renders the original identifiers, exactly', async () => {
  vi.stubEnv('DEV', true)
  const result = mount(AgentResult, { props: { result: original, runId: 'real-run', messageId: BIG } })
  const text = result.text()
  expect(text).toContain('real-run')
  // An id past the safe integer range is shown as it arrived: 9007199254740993123
  // is not a number this client may turn into 9007199254740993000.
  expect(text).toContain(BIG)
  expect(text).not.toContain('9.00719925474099')
  expect(text).toContain('confirmed-topic')
  result.unmount()
})

it('a card the server recorded nothing for is not shown at all', async () => {
  vi.stubEnv('DEV', false)
  const result = mount(AgentResult, { props: { result: { replayed: true } } })
  expect(result.find('[data-test="agent-result"]').exists()).toBe(false)
  result.unmount()
})

it('an answer stored before the server kept these fields shows no unnamed line', async () => {
  vi.stubEnv('DEV', false)
  // `legacy_unspecified` is what the server wrote for an answer it stored
  // before it kept the basis and the knowledge status. Nothing was recorded,
  // so nothing is claimed — not even "未说明".
  const legacy = mount(AgentResult, { props: { result: { answerBasis: 'legacy_unspecified',
    knowledgeStatus: 'legacy_unspecified', knowledgeReason: 'none' } } })
  expect(legacy.text()).not.toContain('未说明')
  expect(legacy.find('[data-test="agent-result"]').exists()).toBe(false)
  legacy.unmount()

  // One field having nothing does not silence the others.
  const mixed = mount(AgentResult, { props: { result: { answerBasis: 'legacy_unspecified',
    knowledgeStatus: 'grounded' } } })
  expect(mixed.text()).not.toContain('未说明')
  expect(mixed.text()).toContain('答案能在你的资料里找到出处')
  mixed.unmount()
})

it('sources render original identity/version without fabricated navigation', async () => {
  const citations = mount(CitationList, { props: { sources: normalizeSourceList(original.sources) } })
  await citations.get('[data-test="citation-toggle"]').trigger('click')
  expect(citations.text()).toContain('original-doc')
  expect(citations.text()).toContain('版本 7')
  expect(citations.text()).toContain('尚未接通')
  expect(citations.find('a').exists()).toBe(false)
  citations.unmount()
})
