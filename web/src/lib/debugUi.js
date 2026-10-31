/**
 * Whether the page may show what a developer needs to see.
 *
 * Some of what the server sends is meant for a developer and not for a learner:
 * a run id, a message id, the raw result object. Those are facts about the
 * plumbing, not about the answer, and a learner reading a run id learns nothing
 * about the answer they asked for. They are kept — they are how a problem is
 * diagnosed — but they are kept behind a switch that is off unless the page was
 * built for development.
 *
 * The switch is `import.meta.env.DEV`, which is what the rest of this client
 * already uses to tell a development build from a production one: `devlog.js`
 * keeps its diagnostics behind the same flag. There was no separate debug
 * switch to reuse, so this is that switch, and nothing more: one place decides
 * whether developer detail may be shown, and the answer is yes only in a
 * development build.
 *
 * The flag is read when it is asked for, not when this module is loaded, so a
 * test can turn it on and off around a single render.
 */

export function debugUiEnabled() {
  return import.meta.env.DEV === true
}
