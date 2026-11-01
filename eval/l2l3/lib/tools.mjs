// L3 assertions: which tools a turn used, judged by outcome and not by order.
//
// Expectations are about capabilities (see capabilities.mjs). A question carries
//   expect  — capabilities a completed step must show,
//   forbid  — capabilities no step may show, whether it succeeded or not,
//   max_tool_calls — the turn's tool budget,
//   doc_constraint — optional: a document read must come from this turn's own search.
// The steps come from the run-steps read path, which names each tool and says whether
// it completed or failed; it carries no arguments, which is why `doc_constraint` goes
// through an inspector (below) and is simply not evaluated when none is supplied.

import { capabilityOf, Capability } from './capabilities.mjs'
import { Verdict } from './enums.mjs'
import { toolSteps } from './record.mjs'

const result = (name, verdict, reason, extra = {}) => ({ name, verdict, reason, ...extra })

const COMPLETED = 'completed'

function capabilitiesOf(steps) {
  return steps.map((step) => ({ step, capability: capabilityOf(step.tool_name) }))
}

/** A required capability counts only if a step that used it completed. */
export function toolsRequired(record, label) {
  const expect = label.expect ?? []
  if (expect.length === 0) return result('tools_required', Verdict.Pass, 'no tool is required')
  const done = new Set(capabilitiesOf(toolSteps(record)).filter(({ step }) => step.status === COMPLETED).map((c) => c.capability))
  const missing = expect.filter((capability) => !done.has(capability))
  return missing.length === 0
    ? result('tools_required', Verdict.Pass, `completed: ${expect.join(', ')}`)
    : result('tools_required', Verdict.Fail, `no completed step for: ${missing.join(', ')}`, { label: 'missing_required_tool', missing })
}

/** A forbidden capability is a violation if any step used it — a failed call was still a call. */
export function toolsForbidden(record, label) {
  const forbid = new Set(label.forbid ?? [])
  if (forbid.size === 0) return result('tools_forbidden', Verdict.Pass, 'no tool is forbidden')
  const used = capabilitiesOf(toolSteps(record)).filter(({ capability }) => forbid.has(capability))
  return used.length === 0
    ? result('tools_forbidden', Verdict.Pass, 'no forbidden capability was used')
    : result('tools_forbidden', Verdict.Fail, `used forbidden: ${used.map(({ step, capability }) => `${step.tool_name} (${capability}, ${step.status})`).join('; ')}`, {
        label: 'forbidden_tool_used',
        used: [...new Set(used.map((u) => u.capability))],
      })
}

/** The turn finished within its tool budget. */
export function toolsBudget(record, label, defaultMax) {
  const max = Number.isInteger(label.max_tool_calls) ? label.max_tool_calls : defaultMax
  if (!Number.isInteger(max)) return result('tools_budget', Verdict.Pass, 'no tool budget is set', { informational: true })
  return record.toolCalls <= max
    ? result('tools_budget', Verdict.Pass, `${record.toolCalls} tool call(s), budget ${max}`)
    : result('tools_budget', Verdict.Fail, `${record.toolCalls} tool call(s) is over the budget of ${max}`, { label: 'over_tool_budget' })
}

/**
 * The step list and the run's own count must agree. When they do not, one of them is
 * wrong and the other assertions would be reading a wrong list, so it is an error: it
 * is loud, and it is not charged to the answer.
 */
export function toolsConsistency(record) {
  if (record.stepsError) return result('tools_consistency', Verdict.Error, `the steps could not be read: ${String(record.stepsError).slice(0, 120)}`, { label: 'steps_unreadable' })
  const counted = toolSteps(record).length
  return counted === record.toolCalls
    ? result('tools_consistency', Verdict.Pass, `${counted} tool step(s) match toolCalls`)
    : result('tools_consistency', Verdict.Error, `${counted} tool step(s) but toolCalls is ${record.toolCalls}`, { label: 'steps_disagree' })
}

/**
 * A document read must name a document this turn's own search returned.
 *
 * @param inspector `{ inspect(runId) -> { steps: [{ sequence, capability, docIds }] } }`.
 *   For a knowledge_search step `docIds` are the documents it returned; for a
 *   document_read step, the document it read. Nothing here knows how the real
 *   implementation gets them (the stored arguments are not on the public read path).
 */
export async function docConstraint(record, label, inspector) {
  if (!label.doc_constraint) return null
  if (!inspector) return result('doc_constraint', Verdict.Pass, 'not evaluated: no inspector for the stored arguments', { notEvaluated: true, informational: true })
  let inspected
  try {
    inspected = await inspector.inspect(record.runId)
  } catch (error) {
    return result('doc_constraint', Verdict.Error, `the stored steps could not be read: ${String(error?.message ?? error).slice(0, 120)}`, { label: 'inspector_failed' })
  }
  const steps = [...(inspected?.steps ?? [])].sort((a, b) => a.sequence - b.sequence)
  const retrieved = new Set()
  const stray = []
  for (const step of steps) {
    if (step.capability === Capability.KnowledgeSearch) for (const id of step.docIds ?? []) retrieved.add(id)
    if (step.capability === Capability.DocumentRead) {
      for (const id of step.docIds ?? []) if (!retrieved.has(id)) stray.push(id)
    }
  }
  return stray.length === 0
    ? result('doc_constraint', Verdict.Pass, 'every document read came from this turn\'s search')
    : result('doc_constraint', Verdict.Fail, `read document(s) the search never returned: ${stray.join(', ')}`, { label: 'document_not_retrieved' })
}

/** All L3 assertions for one trial, in a fixed order. */
export async function toolAssertions(record, label, { defaultMaxToolCalls, inspector } = {}) {
  const consistency = toolsConsistency(record)
  const out = [consistency]
  // With an unreliable step list the other checks would be judging the wrong thing.
  if (consistency.verdict === Verdict.Error) return out
  out.push(toolsRequired(record, label), toolsForbidden(record, label), toolsBudget(record, label, defaultMaxToolCalls))
  const doc = await docConstraint(record, label, inspector)
  if (doc) out.push(doc)
  return out
}
