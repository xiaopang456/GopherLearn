const verdicts = new Set(['pass', 'fail', 'needs_judge', 'error'])
function selected(rows, selector) {
  const groups = new Map()
  for (const r of rows.filter(r => r.arm === selector.arm && r.condition === selector.condition)) {
    if (!r.id || !Number.isInteger(r.rep) || r.rep < 0 || r.rep > 2 || !verdicts.has(r.verdict)) throw new Error('invalid outcome')
    if (!groups.has(r.id)) groups.set(r.id, new Map())
    const group = groups.get(r.id)
    if (group.has(r.rep)) throw new Error('duplicate attempt')
    group.set(r.rep, r)
  }
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
// Repeated attempts remain grouped by scenario. Missing repeats never silently
// become extra independent observations or count as an all-success scenario.
export function pairedReport(rows, left, right, { seed = 20261102, resamples = 2000, expectedIds = [] } = {}) {
  if (left.arm === right.arm && left.condition === right.condition) throw new Error('comparison selectors are identical')
  if (!Number.isInteger(resamples) || resamples < 100 || resamples > 100000 || !Number.isInteger(seed)) throw new Error('invalid bootstrap settings')
  const a = selected(rows, left), b = selected(rows, right)
  const ids = [...new Set([...a.keys(), ...b.keys(), ...expectedIds])].sort()
  const complete = ids.filter(id => a.get(id)?.size === 3 && b.get(id)?.size === 3)
  const differences = [], missing = ids.filter(id => !complete.includes(id))
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
  return { schema: 'l4.comparison.v1', left, right, repeats: 3, complete_scenarios: complete.length, missing_scenarios: missing, mean_difference: differences.length ? mean(differences) : null, ci95: ci, only_left_all_pass: onlyLeft, only_right_all_pass: onlyRight, mcnemar_exact_p: onlyLeft + onlyRight ? binomial(onlyLeft + onlyRight, Math.min(onlyLeft, onlyRight)) : null, pending_attempts: pending, error_attempts: errors, seed, resamples, note: 'Complete scenario pairs only; pending and errors are not successes. Missing pairs are listed, not imputed.' }
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
