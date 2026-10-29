/**
 * A controllable `window.matchMedia`, close enough to the browser for the
 * breakpoint behaviour: one shared list per query string, and a real listener
 * set so a test can assert that unmounting removed the subscription.
 */
export function installMedia(initialMatches = {}) {
  const state = { ...initialMatches }
  const listeners = new Map() // query -> Set<handler>

  const previous = window.matchMedia

  window.matchMedia = (query) => {
    if (!listeners.has(query)) listeners.set(query, new Set())
    return {
      media: query,
      get matches() {
        return !!state[query]
      },
      addEventListener(_type, handler) {
        listeners.get(query).add(handler)
      },
      removeEventListener(_type, handler) {
        listeners.get(query).delete(handler)
      },
    }
  }

  return {
    /** Resize across a breakpoint and let every live subscriber react. */
    set(query, matches) {
      state[query] = matches
      for (const handler of listeners.get(query) ?? []) handler({ matches })
    },
    count(query) {
      return (listeners.get(query) ?? new Set()).size
    },
    restore() {
      window.matchMedia = previous
    },
  }
}

export const DESKTOP_QUERY = '(min-width: 1021px)'
