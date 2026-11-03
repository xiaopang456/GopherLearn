import { checkScenario } from './scenarios.mjs'
// Normalize presentation differences centrally, keeping answer keys semantic.
// Only a complete quantity is converted; numbers inside narrative answers still
// require judgment, including negations and comparisons of old/new values.
export function normalizeFact(s) {
  const text = s.normalize('NFKC').replace(/\s+/gu, '').replace(/[。.!！]$/u, '').toLowerCase()
  const quantity = /^(\d+(?:\.\d+)?)(分钟|分鐘|分|mins?|minutes?|小时|小時|时|時|h|hrs?|hours?|秒钟|秒鐘|秒|s|secs?|seconds?)$/u.exec(text)
  if (!quantity) return text
  const [, value, unit] = quantity
  const factor = /^(分钟|分鐘|分|mins?|minutes?)$/u.test(unit) ? 60n : /^(小时|小時|时|時|h|hrs?|hours?)$/u.test(unit) ? 3600n : 1n
  const [integer, fraction = ''] = value.split('.')
  let numerator = BigInt(integer + fraction) * factor, denominator = 10n ** BigInt(fraction.length)
  let a = numerator, b = denominator
  while (b) [a, b] = [b, a % b]
  numerator /= a; denominator /= a
  return `seconds:${numerator}/${denominator}`
}
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
  const answer = normalizeFact(record.answer)
  if ([...scenario.answer.accepted, ...(scenario.answer.aliases ?? [])].some(v => normalizeFact(v) === answer)) return result('pass', 'exact_fact')
  if (scenario.answer.forbidden?.some(v => normalizeFact(v) === answer)) return result('fail', 'explicit_wrong_fact')
  // A sentence mentioning an expected value may negate it or quote an old value.
  return result('needs_judge', 'noncanonical_fact_answer')
}
