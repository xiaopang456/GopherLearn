import { onBeforeUnmount, ref } from 'vue'

/**
 * A reactive media query.
 *
 * `window.innerWidth` is a plain number: reading it inside a computed evaluates
 * once and never re-runs, which is why crossing a breakpoint used to leave the
 * session column in the wrong state until a reload. The listener is removed on
 * unmount so mounted views cannot accumulate subscriptions.
 *
 * @param {string} query e.g. `(min-width: 1021px)`
 * @returns {import('vue').Ref<boolean>}
 */
export function useMediaQuery(query) {
  const list =
    typeof window !== 'undefined' && typeof window.matchMedia === 'function'
      ? window.matchMedia(query)
      : null
  const matches = ref(list ? !!list.matches : false)

  if (!list) return matches // host without matchMedia: stay on the mobile path

  const onChange = (event) => {
    matches.value = !!event.matches
  }
  list.addEventListener('change', onChange)
  onBeforeUnmount(() => list.removeEventListener('change', onChange))

  return matches
}
