import { checkScenario } from './scenarios.mjs'
const norm = s => s.normalize('NFKC').replace(/\s+/gu, '').replace(/[。.!！]$/u, '')
export function scoreAttempt(record, scenario, condition = 'main') {
  const base = { schema: 'l4.outcome.v1', id: scenario?.id, condition, rep: record?.rep, arm: record?.arm, env: record?.env }
  const result = (verdict, reason) => ({ ...base, verdict, reason })
  if (checkScenario(scenario).length) return result('error', 'invalid_scenario')
  if (record?.qid !== scenario.id || !Number.isInteger(record?.rep) || record.rep < 0 || record.rep > 2) return result('error', 'identity_mismatch')
  if (!(scenario.category === 'irrelevant' ? ['control', 'distractor'].includes(condition) : condition === 'main')) return result('error', 'condition_mismatch')
  if (record.error || typeof record.answer !== 'string' || !record.answer.trim()) return result('error', 'answer_unavailable')
  if (!['fake', 'real'].includes(record.env)) return result('error', 'unknown_environment')
  if (record.env === 'real' && record.backgroundAudit?.valid !== true) return result('error', 'background_unverified')
  if (scenario.answer.mode === 'needs_judge') return result('needs_judge', 'semantic_judgment_required')
  const answer = norm(record.answer)
  if (scenario.answer.accepted.some(v => norm(v) === answer)) return result('pass', 'exact_fact')
  if (scenario.answer.forbidden?.some(v => norm(v) === answer)) return result('fail', 'explicit_wrong_fact')
  // A sentence mentioning an expected value may negate it or quote an old value.
  return result('needs_judge', 'noncanonical_fact_answer')
}
