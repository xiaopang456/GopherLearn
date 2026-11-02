// One trial -> one outcome.
//
// A trial is judged on up to two layers, each only when the question carries what the
// layer needs: L2 when the question has gold evidence or is marked unanswerable, L3
// when it has a tool label. A question can have both, and each layer gets its own
// verdict, so "answered well but searched when it should not have" is visible as
// exactly that.

import { Verdict } from './enums.mjs'
import { answerableAssertions } from './answer.mjs'
import { unanswerableAssertion } from './unanswerable.mjs'
import { toolAssertions } from './tools.mjs'
import { recordProblems, runFailure } from './record.mjs'

export const OUTCOME_SCHEMA = 'outcome.v1'

/**
 * The layer's verdict from its assertions: an error anywhere makes it an error (the
 * service did not answer, which says nothing about the answer), then any failure makes
 * it a failure, then anything left to the judging model makes it `needs_judge`.
 * Informational results (numbers, flags) never move it.
 */
export function combine(assertions) {
  const decisive = assertions.filter((a) => !a.informational)
  for (const verdict of [Verdict.Error, Verdict.Fail, Verdict.NeedsJudge]) {
    if (decisive.some((a) => a.verdict === verdict)) return verdict
  }
  return Verdict.Pass
}

function layerOf(assertions) {
  const verdict = combine(assertions)
  const decisive = assertions.find((a) => a.verdict === verdict && !a.informational && a.label)
  const coverage = assertions.find((a) => a.name === 'citation_coverage')
  const tier = assertions.find((a) => a.tier)?.tier
  return {
    verdict,
    class: verdict === Verdict.Pass ? '' : decisive?.label ?? '',
    ...(coverage ? { coverage: coverage.value } : {}),
    ...(tier ? { tier } : {}),
    assertions: assertions.map(({ name, verdict: v, label, reason, informational }) => ({
      name,
      verdict: v,
      ...(label ? { label } : {}),
      ...(informational ? { informational: true } : {}),
      reason,
    })),
  }
}

/**
 * The class of a call that produced no answer. The status code is shared by several
 * causes, so when the service said how the run ended (its termination reason) the class
 * carries it: `run_failed:unknown_tool` is counted apart from `run_failed:model_call_limit`.
 */
function failureLabel(record) {
  const reason = record.terminationReason
  return typeof reason === 'string' && /^[a-z0-9_]{1,64}$/.test(reason) ? `run_failed:${reason}` : 'run_failed'
}

function brokenLayer(label, reason) {
  return { verdict: Verdict.Error, class: label, assertions: [{ name: 'record', verdict: Verdict.Error, label, reason }] }
}

/**
 * @param record   a record.v1
 * @param question see question.mjs
 * @param context  { corpus, defaultMaxToolCalls, inspector }
 */
export async function judgeTrial(record, question, context) {
  const outcome = {
    schema: OUTCOME_SCHEMA,
    arm: record.arm,
    qid: record.qid,
    rep: record.rep,
    env: record.env,
    cluster: question.cluster,
    category: question.category,
    layers: { l2: null, l3: null },
    cost: {
      modelCalls: Number.isInteger(record.modelCalls) ? record.modelCalls : null,
      toolCalls: Number.isInteger(record.toolCalls) ? record.toolCalls : null,
      durationMs: Number.isFinite(record.durationMs) ? record.durationMs : null,
      projectedInput: Number.isFinite(record.projectedInput) ? record.projectedInput : null,
    },
  }

  const problems = recordProblems(record)
  const failure = problems.length === 0 ? runFailure(record) : ''
  const broken = problems.length > 0 ? brokenLayer('bad_record', problems.join('; ')) : failure ? brokenLayer(failureLabel(record), failure) : null

  if (question.layers.l2) {
    outcome.layers.l2 = broken ?? layerOf(question.answerable
      ? answerableAssertions(record, question, context.corpus)
      : [unanswerableAssertion(record)])
  }
  if (question.layers.l3) {
    outcome.layers.l3 = broken ?? layerOf(await toolAssertions(record, question.label, {
      defaultMaxToolCalls: context.defaultMaxToolCalls,
      inspector: context.inspector,
    }))
  }
  return outcome
}
