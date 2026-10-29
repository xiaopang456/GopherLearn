/**
 * Only in-site paths may be used as a post-login redirect target. A raw
 * `?redirect=` is attacker-controllable, so absolute and protocol-relative
 * forms are rejected rather than followed.
 */
export function safeRedirectTarget(raw, fallback = '/chat') {
  if (typeof raw !== 'string' || raw === '') return fallback
  if (!raw.startsWith('/')) return fallback
  if (raw.startsWith('//') || raw.startsWith('/\\')) return fallback
  if (raw.includes('\n') || raw.includes('\r')) return fallback
  return raw
}
