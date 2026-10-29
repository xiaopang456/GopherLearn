/**
 * A bounded, in-memory log for conditions the user should never have to read
 * about but a developer must be able to find. Unknown session modes are the
 * motivating case: the UI degrades safely and silently for the user, while the
 * offending value stays inspectable in tests and in a dev console.
 */

const LIMIT = 50

const issues = []

export function recordDevIssue(kind, detail = {}) {
  const entry = { kind, detail, at: new Date().toISOString() }
  if (issues.length < LIMIT) issues.push(entry)
  if (import.meta.env?.DEV && typeof console !== 'undefined') {
    // eslint-disable-next-line no-console -- single dev-only diagnostic sink
    console.warn(`[gopherlearn] ${kind}`, detail)
  }
  return entry
}

export function devIssues() {
  return issues.map((entry) => ({ ...entry }))
}

export function clearDevIssues() {
  issues.length = 0
}
