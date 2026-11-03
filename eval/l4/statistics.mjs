const verdicts = new Set(['pass', 'fail', 'needs_judge', 'error'])
function selected(rows, selector) {
  if (!Array.isArray(rows) || !selector || typeof selector.arm !== 'string' || !selector.arm || !['main', 'control', 'distractor'].includes(selector.condition)) throw new Error('invalid report selection')
  const groups = new Map()
  const environments = new Set()
  for (const r of rows) {
    if (!r || typeof r !== 'object') throw new Error('invalid outcome')
    if (r.arm !== selector.arm || r.condition !== selector.condition) continue
    if (!['fake', 'real'].includes(r.env)) throw new Error('unknown outcome environment')
    environments.add(r.env)
    if (!r.id || !Number.isInteger(r.rep) || r.rep < 0 || r.rep > 2 || !verdicts.has(r.verdict)) throw new Error('invalid outcome')
    if (!groups.has(r.id)) groups.set(r.id, new Map())
    const group = groups.get(r.id)
    if (group.has(r.rep)) throw new Error('duplicate attempt')
    group.set(r.rep, r)
  }
  if (environments.size > 1) throw new Error('mixed fake and real outcomes')
  return groups
}
function aggregate(group) {
  const values = [...group.values()]
  const success = values.filter(v => v.verdict === 'pass').length
  return { rate: success / 3, all: success === 3, pending: values.filter(v => v.verdict === 'needs_judge').length, errors: values.filter(v => v.verdict === 'error').length }
}
function binomial(n, k) {
  let p = 2 ** -n, total = p
  for (let i = 1; i <= k; i++) { p *= (n - i + 1) / i; total += p }
  return Math.min(1, 2 * total)
}
// Conditional exact McNemar power, given the observed number of discordant
// scenario-level all-pass outcomes. This is a pilot estimate, not a guarantee
// for a future experiment whose discordance rate may differ.
export function discordantMDE(n, discordant) {
  const base = { metric: 'scenario_all_three_pass', alpha: .05, target_power: .8, complete_scenarios: n, discordant_scenarios: discordant }
  if (!Number.isInteger(n) || !Number.isInteger(discordant) || n < 0 || discordant < 0 || discordant > n) throw new Error('invalid discordance counts')
  if (!discordant) return { ...base, difference: null, reason: 'no_observed_discordance' }
  const rejects = k => binomial(discordant, Math.min(k, discordant - k)) <= .05
  if (!rejects(discordant)) return { ...base, difference: null, reason: 'too_few_discordant_scenarios_for_exact_rejection' }
  const logChoose = [0]
  for (let k = 1; k <= discordant; k++) logChoose[k] = logChoose[k - 1] + Math.log(discordant - k + 1) - Math.log(k)
  const power = q => {
    if (q === 1) return 1
    let total = 0
    for (let k = 0; k <= discordant; k++) if (rejects(k)) total += Math.exp(logChoose[k] + k * Math.log(q) + (discordant - k) * Math.log1p(-q))
    return total
  }
  let low = .5, high = 1
  for (let i = 0; i < 60; i++) { const q = (low + high) / 2; if (power(q) >= .8) high = q; else low = q }
  return { ...base, difference: discordant / n * (2 * high - 1), conditional_favor_left_probability: high, conditional_power: power(high), reason: 'conditional_on_observed_discordance' }
}
// Repeated attempts remain grouped by scenario. Missing repeats never silently
// become extra independent observations or count as an all-success scenario.
export function pairedReport(rows, left, right, { seed = 20261102, resamples = 2000, expectedIds = [] } = {}) {
  if (left.arm === right.arm && left.condition === right.condition) throw new Error('comparison selectors are identical')
  if (!Number.isInteger(resamples) || resamples < 100 || resamples > 100000 || !Number.isInteger(seed)) throw new Error('invalid bootstrap settings')
  const a = selected(rows, left), b = selected(rows, right)
  if (new Set([...a.values(), ...b.values()].flatMap(g => [...g.values()].map(r => r.env))).size > 1) throw new Error('mixed fake and real comparison')
  const ids = [...new Set([...a.keys(), ...b.keys(), ...expectedIds])].sort()
  const complete = ids.filter(id => a.get(id)?.size === 3 && b.get(id)?.size === 3)
  const differences = [], missing = ids.filter(id => !complete.includes(id))
  let lower = 0, upper = 0, conservative = 0, allPending = 0, allErrors = 0
  const absent = { left: 0, right: 0 }
  for (const id of ids) {
    const x = [...(a.get(id)?.values() ?? [])], y = [...(b.get(id)?.values() ?? [])]
    const passes = values => values.filter(r => r.verdict === 'pass').length
    const unresolved = values => 3 - values.filter(r => ['pass', 'fail'].includes(r.verdict)).length
    lower += (passes(x) - passes(y) - unresolved(y)) / 3
    upper += (passes(x) + unresolved(x) - passes(y)) / 3
    conservative += (passes(x) - passes(y)) / 3
    absent.left += 3 - x.length; absent.right += 3 - y.length
    allPending += [...x, ...y].filter(r => r.verdict === 'needs_judge').length
    allErrors += [...x, ...y].filter(r => r.verdict === 'error').length
  }
  let onlyLeft = 0, onlyRight = 0, pending = 0, errors = 0
  for (const id of complete) {
    const x = aggregate(a.get(id)), y = aggregate(b.get(id))
    differences.push(x.rate - y.rate)
    onlyLeft += Number(x.all && !y.all); onlyRight += Number(y.all && !x.all)
    pending += x.pending + y.pending; errors += x.errors + y.errors
  }
  const mean = values => values.reduce((x, y) => x + y, 0) / values.length
  let state = seed >>> 0 || 1
  const random = () => { state ^= state << 13; state ^= state >>> 17; state ^= state << 5; return (state >>> 0) / 4294967296 }
  let ci = null
  if (differences.length >= 2) {
    const draws = Array.from({ length: resamples }, () => mean(differences.map(() => differences[Math.floor(random() * differences.length)]))).sort((x, y) => x - y)
    ci = [draws[Math.floor(.025 * resamples)], draws[Math.floor(.975 * resamples)]]
  }
  let rateMDE = null
  if (differences.length >= 2) {
    const center = mean(differences), variance = differences.reduce((total, d) => total + (d - center) ** 2, 0) / (differences.length - 1)
    // Fixed two-sided alpha=.05 and power=.8, normal approximation on the
    // independent scenario means (never treating the three repeats as 3n).
    if (variance > 0) rateMDE = (1.959963984540054 + .8416212335729143) * Math.sqrt(variance / differences.length)
  }
  return { schema: 'l4.comparison.v1', left, right, repeats: 3, complete_scenarios: complete.length, missing_scenarios: missing, mean_difference: differences.length ? mean(differences) : null, ci95: ci, only_left_all_pass: onlyLeft, only_right_all_pass: onlyRight, mcnemar_exact_p: onlyLeft + onlyRight ? binomial(onlyLeft + onlyRight, Math.min(onlyLeft, onlyRight)) : null, pending_attempts: pending, error_attempts: errors,
    sensitivity: { expected_scenarios: ids.length, missing_scenario_fraction: ids.length ? missing.length / ids.length : null, missing_attempts: absent, pending_attempts: allPending, error_attempts: allErrors, all_unresolved_as_non_success_difference: ids.length ? conservative / ids.length : null, unresolved_outcome_bounds: ids.length ? [lower / ids.length, upper / ids.length] : null, note: 'Bounds assign missing, pending and error outcomes opposite success extremes; these are identification bounds, not confidence intervals.' },
    mde: { clustered_rate_normal_approx: { alpha: .05, target_power: .8, difference: rateMDE, within_feasible_effect_range: rateMDE === null ? null : rateMDE <= 1, note: 'Exploratory observed-variance approximation on scenario means; null for insufficient or zero variance. It does not guarantee power in a small or future sample.' }, conditional_exact_all_pass: discordantMDE(complete.length, onlyLeft + onlyRight) },
    pairs: ids.map(id => ({ id, left: [0, 1, 2].map(rep => a.get(id)?.get(rep)?.verdict ?? null), right: [0, 1, 2].map(rep => b.get(id)?.get(rep)?.verdict ?? null) })),
    seed, resamples, note: 'Primary estimate uses complete scenario pairs; pending and errors are not successes. Missing/unresolved outcomes are also reported with extreme-case sensitivity bounds.' }
}

export function singleReport(rows, selector, expectedIds = []) {
  const groups = selected(rows, selector)
  const ids = [...new Set([...groups.keys(), ...expectedIds])].sort()
  let pass = 0, fail = 0, pending = 0, errors = 0, all = 0, pass2 = 0, complete = 0
  const missing = []
  for (const id of ids) {
    const values = [...(groups.get(id)?.values() ?? [])]
    const c = values.filter(r => r.verdict === 'pass').length
    pass += c; fail += values.filter(r => r.verdict === 'fail').length
    pending += values.filter(r => r.verdict === 'needs_judge').length
    errors += values.filter(r => r.verdict === 'error').length
    if (values.length !== 3) { missing.push(id); continue }
    complete++; all += Number(c === 3); pass2 += c * (c - 1) / 6
  }
  return { selector, scenarios: ids.length, expected_attempts: ids.length * 3, observed_attempts: pass + fail + pending + errors, pass, fail, needs_judge: pending, errors, success_rate_conservative: ids.length ? pass / (ids.length * 3) : null, success_rate_adjudicated: pass + fail ? pass / (pass + fail) : null, pass3_conservative: ids.length ? all / ids.length : null, pass2_complete_only: complete ? pass2 / complete : null, complete_scenarios: complete, missing_scenarios: missing }
}
