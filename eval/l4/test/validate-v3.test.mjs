import { test } from 'node:test'
import assert from 'node:assert/strict'
import {validateDataset} from '../validate-v3.mjs'
import {splitForId} from '../scenarios.mjs'

// label is digit-free, so a fixture never invents a number the validator would
// have to explain.
const label = index => `${String.fromCharCode(65 + (index % 26))}${String.fromCharCode(97 + Math.floor(index / 26))}`
const START = '2026-02-09T09:00:00+00:00'
const stamp = (index, gap = 0) => new Date(Date.parse(START) + index * 300000 + gap * 60000).toISOString().replace('Z', '+00:00')

// Each fixture history carries its own id in every line, so two scenarios never
// look alike unless a test deliberately copies a turn.
function turns(who, length) {
 return Array.from({length}, (_, i) => ({user: `question ${who}-${label(i)}`, assistant: `answer ${who}-${label(i)}`, user_at: stamp(i), assistant_at: stamp(i, 1)}))
}

// r1 states one fact once, in turn 35.
function r1(id = 'example') {
 const history = turns(id, 60)
 history[34] = {...history[34], user: `name is Lantern for ${id}`, assistant: 'the archive tag is Lantern'}
 return {id, split: splitForId(id), schema_version: 'l4.scenario.draft.v3', category: 'R1', entity_name: 'Lantern', turns: history, question: 'What was the name?', fact_turn: 35, distance_to_question: 26, evidence_turns: [35], answer: {mode: 'fact', accepted: ['Lantern']}}
}

// r2 sets 41 minutes in turn 30 and replaces it with 26 minutes in turn 46.
function r2(id = 'update-example') {
 const history = turns(id, 85)
 history[29] = {...history[29], user: 'Torch 计划每次 41分钟', assistant: '先按每次 41分钟'}
 history[45] = {...history[45], user: '日程冲突，Torch 每次改成 26分钟', assistant: '现在每次 26分钟'}
 return {id, split: splitForId(id), schema_version: 'l4.scenario.draft.v3', category: 'R2', entity_name: 'Torch', turns: history, question: 'Torch 计划现在每次多长？', original_turn: 30, superseded_turn: 46, superseded_value: '41分钟', distance_to_question: 40, evidence_turns: [30, 46], answer: {mode: 'fact', accepted: ['26分钟'], forbidden: ['41分钟'], aliases: [], derived: false}}
}

// r3 asks for the ORIGINAL value: 19 minutes set in turn 30, replaced by 62 in
// turn 46.
function r3(id = 'original-example') {
 const history = turns(id, 85)
 history[29] = {...history[29], user: '错题重做叫菱潭，每次 19分钟', assistant: '先按每次 19分钟'}
 history[45] = {...history[45], user: '菱潭改成每次 62分钟，替代原来的安排', assistant: '更新为每次 62分钟'}
 return {id, split: splitForId(id), schema_version: 'l4.scenario.draft.v3', category: 'R3', entity_name: '菱潭', turns: history, question: '菱潭计划我最初每次定了多长？', original_turn: 30, superseded_turn: 46, superseded_value: '19分钟', distance_to_question: 40, evidence_turns: [30, 46], answer: {mode: 'fact', accepted: ['19分钟'], forbidden: ['62分钟'], aliases: [], derived: false}}
}

test('the turn that changed the value has to state the new one, whichever key it sits in', () => {
 const clean = r3()
 assert.deepEqual(validateDataset([clean]).errors, [])
 const moved = r3('moved-example')
 moved.turns[45] = {...moved.turns[45], user: '菱潭的时间安排我后面再说', assistant: '好'}
 assert.ok(validateDataset([moved]).errors.some(x => x.message.includes('current value absent from superseded turn 46')), JSON.stringify(validateDataset([moved]).errors))
 const wrongWay = r2()
 wrongWay.turns[45] = {...wrongWay.turns[45], user: '日程冲突，Torch 每次改成 25分钟', assistant: '先按 25分钟'}
 assert.ok(validateDataset([wrongWay]).errors.some(x => x.message.includes('current value absent from superseded turn 46')), JSON.stringify(validateDataset([wrongWay]).errors))
})

const errorsOf = report => report.errors.map(error => error.message)

test('a topic map has to describe the history it is attached to', () => {
 const s = r1()
 s.segments = [{topic: 'Go', start_turn: 1, end_turn: 30}, {topic: '学习方法', start_turn: 31, end_turn: 60}]
 assert.deepEqual(validateDataset([s]).errors, [])
 s.segments = [{topic: 'Go', start_turn: 1, end_turn: 30}, {topic: '学习方法', start_turn: 31, end_turn: 59}]
 assert.ok(validateDataset([s]).errors.some(x => x.message.includes('segments reach turn 59')), JSON.stringify(validateDataset([s]).errors))
 s.segments = [{topic: 'Go', start_turn: 1, end_turn: 20}, {topic: '学习方法', start_turn: 22, end_turn: 60}]
 assert.ok(validateDataset([s]).errors.some(x => x.message.includes('starts at 22, expected 21')), JSON.stringify(validateDataset([s]).errors))
 s.segments = [{topic: '', start_turn: 1, end_turn: 60}]
 assert.ok(validateDataset([s]).errors.some(x => x.message.includes('has no topic')), JSON.stringify(validateDataset([s]).errors))
 delete s.segments
 const review = validateDataset([s]).review
 assert.ok(review.some(item => item.kind === 'no topic map'), JSON.stringify(review))
})

const kindsAt = (report, turn) => report.review.filter(item => item.turn === turn).map(item => item.kind)

test('a clean dataset reports neither errors nor value leaks', () => {
 const report = validateDataset([r1(), r2()])
 assert.deepEqual(errorsOf(report), [])
 assert.ok(report.review.every(item => item.kind !== 'near value' && item.kind !== 'derived value'))
})

test('flags answer leakage, reused names, and earlier names', () => {
 const s = r1()
 assert.equal(validateDataset([s]).errors.length, 0)
 s.turns[38].user = 'Lantern'
 assert.ok(validateDataset([s]).errors.some(x => x.message.includes('outside evidence')))
 assert.ok(validateDataset([r1()], ['the lantern project']).errors.some(x => x.message.includes('earlier')))
})

test('the declared key turn has to carry the answer', () => {
 const s = r1()
 s.turns[34] = {...s.turns[34], user: 'name is unknown', assistant: 'the archive tag is unknown'}
 assert.ok(validateDataset([s]).errors.some(x => x.message.includes('accepted value absent from key turn 35')))
 const u = r2()
 u.turns[29] = {...u.turns[29], user: 'Torch 计划每次 40分钟', assistant: '先按每次 40分钟'}
 assert.ok(validateDataset([u]).errors.some(x => x.message.includes('old value absent from original turn 30')))
})

test('the same quantity in another spelling is still the answer', () => {
 const u = r2()
 u.turns[60] = {...u.turns[60], user: '我一次读 1560秒 的材料', assistant: '可以按这个长度安排'}
 const report = validateDataset([u])
 assert.ok(report.errors.some(x => x.message.includes('in another unit')), JSON.stringify(report.errors))
})

test('near and derived numbers are listed for a reader, never silently allowed', () => {
 const u = r2()
 // turns[i] is turn i+1: 24 is within three of the current 26, 67 is the total
 // of 41 and 26, 52 is twice the current value.
 u.turns[61] = {...u.turns[61], user: '另一项安排每次 24分钟', assistant: '好'}
 u.turns[62] = {...u.turns[62], user: '两项合计 67分钟', assistant: '记录下来了'}
 u.turns[63] = {...u.turns[63], user: '拆分后每次 52分钟', assistant: '明白'}
 const report = validateDataset([u])
 assert.deepEqual(kindsAt(report, 62), ['near value'])
 assert.deepEqual(kindsAt(report, 63), ['derived value'])
 assert.deepEqual(kindsAt(report, 64), ['derived value'])
 assert.equal(report.errors.length, 0)
})

test('a question may not carry the number it is asking for', () => {
 const u = r2()
 u.question = 'Torch 计划现在每次 26分钟还是多久？'
 assert.ok(validateDataset([u]).errors.some(x => x.message.includes('question contains an answer')))
 u.question = 'Torch 计划现在每次 20 分钟以上吗？'
 assert.ok(validateDataset([u]).errors.some(x => x.message.includes('question carries a number')))
})

test('history text cannot be pasted from another scenario', () => {
 const first = r1(), second = r1('example-two')
 second.entity_name = 'Beacon'
 second.answer = {mode: 'fact', accepted: ['Beacon']}
 second.turns[34] = {...second.turns[34], user: 'name is Beacon', assistant: 'the archive tag is Beacon'}
 second.turns[10] = {...first.turns[10]}
 const report = validateDataset([first, second])
 assert.ok(report.errors.some(x => x.message.includes('reuses history text')), JSON.stringify(report.errors))
})

test('shared sentences are reported even without an exact copy', () => {
 const first = r1(), second = r1('example-three')
 second.entity_name = 'Beacon'
 second.answer = {mode: 'fact', accepted: ['Beacon']}
 second.turns[34] = {...second.turns[34], user: 'name is Beacon', assistant: 'the archive tag is Beacon'}
 second.turns[11] = {...second.turns[11], user: '我想先把重复笔记挑出来再决定怎么归档', assistant: '先定义重复：逐字相同、标准化后相同，还是语义相近；每种结果的可信度不同。'}
 first.turns[11] = {...first.turns[11], user: '重复检测用什么思路', assistant: '先定义重复：逐字相同、标准化后相同，还是语义相近；每种结果的可信度不同。'}
 const report = validateDataset([first, second])
 assert.ok(report.review.some(item => item.kind === 'shared history'), JSON.stringify(report.review))
})

test('another scenario may not quote this scenario name or answer', () => {
 const first = r1(), second = r1('example-four')
 second.entity_name = 'Beacon'
 second.answer = {mode: 'fact', accepted: ['Beacon']}
 second.turns[34] = {...second.turns[34], user: 'name is Beacon', assistant: 'the archive tag is Beacon'}
 second.turns[20] = {...second.turns[20], user: 'Lantern 那个标签还能用吗', assistant: '这是另一件事的安排'}
 const report = validateDataset([first, second])
 assert.ok(report.errors.some(x => x.id === 'example-four' && x.message.includes('carries the name "Lantern" of example')), JSON.stringify(report.errors))
})

test('another scenario may not carry this scenario answer value', () => {
 const update = r2()
 const other = r1('example-six')
 other.turns[21] = {...other.turns[21], user: '我把每次的 26分钟 拆开记录', assistant: '好'}
 const report = validateDataset([update, other])
 assert.ok(report.errors.some(x => x.id === 'example-six' && x.message.includes('carries the answer "26分钟" of update-example')), JSON.stringify(report.errors))
})

test('relative time is listed for a person to resolve', () => {
 const s = r1()
 s.turns[5] = {...s.turns[5], user: '下周三我再试一次', assistant: '到时候把结果记下来'}
 const report = validateDataset([s])
 assert.ok(kindsAt(report, 6).includes('relative time'), JSON.stringify(report.review))
 assert.equal(report.errors.length, 0)
})

test('dates are checked against the calendar and against a year', () => {
 const s = r1()
 s.turns[6] = {...s.turns[6], user: '2026-02-09 星期一 开始', assistant: '好的'}
 assert.equal(validateDataset([s]).errors.length, 0)
 s.turns[6].user = '2026-02-09 星期二 开始'
 assert.ok(validateDataset([s]).errors.some(x => x.message.includes('weekday')))
 s.turns[6].user = '2026-02-30 星期一 开始'
 assert.ok(validateDataset([s]).errors.some(x => x.message.includes('impossible date')))
 s.turns[6].user = '3月14日 星期六 交稿'
 const report = validateDataset([s])
 assert.equal(report.errors.length, 0)
 assert.ok(kindsAt(report, 7).includes('date without a year'), JSON.stringify(report.review))
})

test('names that look alike are listed', () => {
 const first = r1(), second = r1('example-five')
 second.entity_name = 'Lattern'
 second.answer = {mode: 'fact', accepted: ['Lattern']}
 second.turns[34] = {...second.turns[34], user: 'name is Lattern', assistant: 'the archive tag is Lattern'}
 const report = validateDataset([first, second])
 assert.ok(report.review.some(item => item.kind === 'similar names'), JSON.stringify(report.review))
 assert.deepEqual(report.errors.filter(x => x.message.includes('duplicate name')), [])
})

test('a question that states the constraint it is testing is refused', () => {
 const id = 'constraint-example'
 const history = turns(id, 60)
 history[34] = {...history[34], user: '以后为 Lantern 安排练习时只用 Go 标准库', assistant: '后续练习遵守这条约定'}
 const s = {id, split: splitForId(id), schema_version: 'l4.scenario.draft.v3', category: 'R5', entity_name: 'Lantern', turns: history, question: '请只用 Go 标准库安排一个小练习', fact_turn: 35, distance_to_question: 26, evidence_turns: [35], answer: {mode: 'needs_judge', required: ['只用 Go 标准库']}}
 const report = validateDataset([s])
 assert.ok(report.errors.some(x => x.message.includes('question states the constraint')), JSON.stringify(report.errors))
 s.question = '围绕并发给我安排一个能动手验证的小练习'
 const clean = validateDataset([s])
 assert.deepEqual(errorsOf(clean), [])
 assert.ok(clean.review.some(item => item.kind === 'key turn needs a reader'))
 assert.ok(clean.review.some(item => item.kind === 'dev count below target'))
})

test('a contradictory weekday is still detected', () => {
 const s = r1()
 s.turns[0].user = '2026-01-05 星期二'
 assert.ok(validateDataset([s]).errors.some(x => x.message.includes('weekday')))
})

test('duplicate turns inside one history are errors', () => {
 const s = r1()
 s.turns[40] = {user: s.turns[41].user, assistant: s.turns[41].assistant, user_at: s.turns[40].user_at, assistant_at: s.turns[40].assistant_at}
 const report = validateDataset([s])
 assert.ok(report.errors.some(x => x.message === 'duplicate turn 42'), JSON.stringify(report.errors))
})
