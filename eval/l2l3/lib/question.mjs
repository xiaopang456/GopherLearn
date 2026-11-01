// The questions a trial is judged against: the L1 question file (answerable or not,
// with its gold evidence), the L3 labels beside it, and the L3-only questions.
//
// The three files stay separate on disk — the L3 labels never edit the L1 question
// set — and are joined here by question id.

import crypto from 'node:crypto'
import fs from 'node:fs'
import { isKnownCapability } from './capabilities.mjs'

export function sha256File(path) {
  return crypto.createHash('sha256').update(fs.readFileSync(path)).digest('hex')
}

export function readJsonl(path) {
  const rows = []
  const lines = fs.readFileSync(path, 'utf8').replace(/^﻿/, '').split(/\r?\n/)
  lines.forEach((line, index) => {
    if (line.trim() === '') return
    try {
      rows.push(JSON.parse(line))
    } catch (error) {
      throw new Error(`${path} line ${index + 1}: ${error.message}`)
    }
  })
  return rows
}

/** A label is well formed: capabilities are known, the budget is a whole number. */
export function labelProblems(label) {
  const problems = []
  if (typeof label?.id !== 'string' || label.id === '') problems.push('id is missing')
  for (const field of ['expect', 'forbid']) {
    if (!Array.isArray(label?.[field])) {
      problems.push(`${field} is not a list`)
      continue
    }
    for (const capability of label[field]) {
      if (!isKnownCapability(capability)) problems.push(`${field} names unknown capability ${JSON.stringify(capability)}`)
    }
  }
  if (Array.isArray(label?.expect) && Array.isArray(label?.forbid)) {
    const both = label.expect.filter((c) => label.forbid.includes(c))
    if (both.length > 0) problems.push(`${both.join(', ')} is both expected and forbidden`)
  }
  if (!Number.isInteger(label?.max_tool_calls) || label.max_tool_calls < 0) problems.push('max_tool_calls is not a non-negative integer')
  if (label?.author !== undefined && typeof label.author !== 'string') problems.push('author is not a string')
  return problems
}

/**
 * Join the three sources into questions keyed by id.
 *
 * @param datasetRows  the L1 question file (answerable, evidence_sets, type)
 * @param labels       the L3 labels
 * @param l3OnlyRows   questions written for L3 only (no gold evidence)
 */
export function buildQuestions({ datasetRows = [], labels = [], l3OnlyRows = [] }) {
  const questions = new Map()
  for (const row of datasetRows) {
    const first = row.evidence_sets?.[0]?.[0]?.file
    questions.set(row.id, {
      id: row.id,
      text: row.question,
      answerable: row.answerable === true,
      evidenceSets: row.evidence_sets ?? [],
      cluster: first ?? row.topic ?? row.id,
      category: row.type ?? '',
      label: null,
      layers: { l2: true, l3: false },
    })
  }
  for (const row of l3OnlyRows) {
    if (questions.has(row.id)) throw new Error(`question id ${row.id} is in both the L1 question file and the L3-only file`)
    questions.set(row.id, {
      id: row.id,
      text: row.question,
      answerable: null,
      evidenceSets: [],
      cluster: row.category ?? row.id,
      category: row.category ?? '',
      label: null,
      layers: { l2: false, l3: false },
    })
  }
  for (const label of labels) {
    const question = questions.get(label.id)
    if (!question) throw new Error(`label for ${label.id}, which no question file has`)
    if (question.label) throw new Error(`two labels for ${label.id}`)
    question.label = label
    question.layers.l3 = true
    if (label.category && question.category === '') question.category = label.category
  }
  return questions
}
