// The checker for the L3 labels: structure, joins and balance. Offline, free, and the first
// thing to run on a label file before it is used.
//
// It checks what a program can: that every label names known capabilities, that every
// label points at a question that exists, that the "should search" and "should not search"
// questions are about half each (so a system cannot score well by searching everything or
// by searching nothing), and that the questions written for L3 do not use wording the
// product treats specially. It cannot check that a label is *right*; that is a reviewer's job.

import { pathToFileURL } from 'node:url'

import { Capability } from './lib/capabilities.mjs'
import { labelProblems, readJsonl } from './lib/question.mjs'

// Wording the product answers by rule (v2/service/agent/runner.go, as of the commit this
// checker was written against). A question that uses it measures the rule, not the model.
const EXACT_HISTORY = ['原话', '一字不差', '逐字', '原文', '我说了什么', '我怎么说', '当时怎么说']
const KNOWLEDGE_ONLY = ['只根据我的笔记', '只能根据我的笔记', '仅根据我的笔记', '只根据我的知识库', '只能根据我的知识库', '仅根据我的知识库', '只用我的笔记', '只能用我的笔记', '只用知识库', '只能用知识库']
const SESSION_CONTEXT = ['请记住', '帮我记住', '记一下', '记住这个', '你还记得', '还记得我', '刚才', '之前我们', '前面我们', '本次对话', '当前对话', '本会话', '当前会话', '会话事实', '会话记忆', '记忆检查', 'recent messages', 'session summary', '不需要知识库', '无需知识库', '不要检索知识库', '只使用 recent', '只使用 session']
const GREETINGS = new Set(['你好', '您好', '嗨', '哈喽', 'hello', 'hi', 'hey', '在吗', '早上好', '下午好', '晚上好', '谢谢', '谢谢你', '感谢', '多谢', '不客气', '再见', '拜拜', '你是谁', '你叫什么', '你叫什么名字', '介绍一下自己', '请介绍一下自己', '你能做什么', '你会做什么', '请问你能做什么', 'help', '帮助'])

/** The shortcut the product takes for this wording, or '' when none. */
export function productShortcut(text) {
  const compact = [...text.trim().toLowerCase()].filter((ch) => /[\p{L}\p{N}]/u.test(ch)).join('')
  if (GREETINGS.has(compact)) return 'direct_greeting'
  const lower = text.trim().toLowerCase()
  if (SESSION_CONTEXT.some((m) => lower.includes(m))) return 'session_context'
  return ''
}

/** Share of the labelled questions that must expect a knowledge search: within this of one half. */
export const BALANCE_TOLERANCE = 0.1

/**
 * @param opts.labels       the label rows
 * @param opts.datasetRows  the L1 dev question rows
 * @param opts.l3OnlyRows   the L3-only question rows
 * @returns { errors, warnings, summary }
 */
export function checkLabels({ labels, datasetRows = [], l3OnlyRows = [] }) {
  const errors = []
  const warnings = []
  const datasetIds = new Set(datasetRows.map((r) => r.id))
  const l3Rows = new Map(l3OnlyRows.map((r) => [r.id, r]))
  const seen = new Set()

  for (const label of labels) {
    for (const problem of labelProblems(label)) errors.push(`${label.id ?? '?'}: ${problem}`)
    if (seen.has(label.id)) errors.push(`${label.id}: two labels`)
    seen.add(label.id)
    if (label.source === 'dev' && !datasetIds.has(label.id)) errors.push(`${label.id}: labelled as a dev question, but the dev question file has no such id`)
    if (label.source === 'l3only' && !l3Rows.has(label.id)) errors.push(`${label.id}: labelled as an L3-only question, but the L3-only file has no such id`)
    if (label.source !== 'dev' && label.source !== 'l3only') errors.push(`${label.id}: source must be "dev" or "l3only"`)
    if (label.source === 'dev' && datasetIds.has(label.id) && l3Rows.has(label.id)) errors.push(`${label.id}: the id is in both question files`)
  }
  for (const row of l3OnlyRows) {
    if (!seen.has(row.id)) errors.push(`${row.id}: an L3-only question with no label`)
    if (row.author !== 'human') errors.push(`${row.id}: a question written for L3 must say author "human"`)
    if (row.l3_only !== true) errors.push(`${row.id}: l3_only must be true`)
    for (const marker of [...EXACT_HISTORY, ...KNOWLEDGE_ONLY]) {
      if (row.question.includes(marker)) errors.push(`${row.id}: the wording "${marker}" makes the product answer by rule`)
    }
    const shortcut = productShortcut(row.question)
    if (shortcut) warnings.push(`${row.id}: the product has a fixed path for this wording (${shortcut}); it measures the rule, not the model`)
  }
  const texts = new Map()
  for (const row of [...datasetRows, ...l3OnlyRows]) {
    if (texts.has(row.question)) warnings.push(`${row.id} and ${texts.get(row.question)} have the same text`)
    texts.set(row.question, row.id)
  }

  const shouldSearch = labels.filter((l) => Array.isArray(l.expect) && l.expect.includes(Capability.KnowledgeSearch)).length
  const share = labels.length === 0 ? 0 : shouldSearch / labels.length
  if (labels.length > 0 && Math.abs(share - 0.5) > BALANCE_TOLERANCE) {
    errors.push(`${shouldSearch} of ${labels.length} labelled questions expect a knowledge search (${(share * 100).toFixed(0)}%); it must be within ${BALANCE_TOLERANCE * 100} points of half, or searching everything (or nothing) would score well`)
  }
  const categories = {}
  for (const label of labels) categories[label.category ?? ''] = (categories[label.category ?? ''] ?? 0) + 1

  return {
    errors,
    warnings,
    summary: { labels: labels.length, shouldSearch, shouldNotSearchOrOtherTool: labels.length - shouldSearch, shareShouldSearch: share, categories },
  }
}

async function main(argv) {
  const args = {}
  for (let i = 0; i < argv.length; i += 2) args[argv[i].replace(/^--/, '')] = argv[i + 1]
  if (!args.labels) throw new Error('usage: node check.mjs --labels F [--dataset F] [--l3-only F]')
  const result = checkLabels({
    labels: readJsonl(args.labels),
    datasetRows: args.dataset ? readJsonl(args.dataset) : [],
    l3OnlyRows: args['l3-only'] ? readJsonl(args['l3-only']) : [],
  })
  console.log(JSON.stringify(result.summary, null, 2))
  for (const w of result.warnings) console.log(`warning: ${w}`)
  for (const e of result.errors) console.error(`error: ${e}`)
  if (result.errors.length > 0) process.exit(1)
  console.log('ok: the labels are well formed')
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? '').href) {
  main(process.argv.slice(2)).catch((error) => {
    console.error(`check: ${error.message}`)
    process.exit(1)
  })
}

