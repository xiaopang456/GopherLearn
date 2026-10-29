<script>
import { createTextVNode, h } from 'vue'

import { ALLOWED_MARKDOWN_TAGS, isSafeHref } from '../lib/markdown.js'

/**
 * Node tree → vnodes.
 *
 * `markdown.js` already refuses to name a tag outside its whitelist, so this file
 * is where that claim is checked rather than assumed: a node whose tag is not on
 * the list, or whose props are not on the list, is rendered as text. The prop list
 * matters more than it looks — Vue turns an `onXxx` prop into a real DOM event
 * listener, which is exactly the "model output creates a handler" path a renderer
 * opens by accident. An `href` is re-checked here too, because this is the last
 * moment the value is provably the parser's output rather than whatever some later
 * caller could hand in.
 *
 * No HTML string and no raw markup sink: the document is built from element
 * objects, so a hostile version of it cannot exist even briefly.
 */

const ALLOWED_TAGS = new Set(ALLOWED_MARKDOWN_TAGS)
const ALLOWED_PROPS = new Set(['href', 'rel', 'class'])
/** `hr` and `br` cannot hold a child, so the stream caret goes beside them. */
const VOID_TAGS = new Set(['br', 'hr'])

function caretNode() {
  return h('span', { class: 'content__caret', 'aria-hidden': 'true' })
}

function safeProps(tag, props) {
  if (!props || typeof props !== 'object') return null
  const out = {}
  for (const [key, value] of Object.entries(props)) {
    if (!ALLOWED_PROPS.has(key)) continue
    if (key === 'href' && (tag !== 'a' || !isSafeHref(value))) return null
    out[key] = String(value)
  }
  return Object.keys(out).length > 0 ? out : null
}

function toVNode(node, withCaret) {
  if (!node || typeof node !== 'object') return null
  const caret = withCaret ? [caretNode()] : []

  if (typeof node.text === 'string') {
    const text = createTextVNode(node.text)
    return caret.length ? [text, ...caret] : text
  }

  const allowed = typeof node.tag === 'string' && ALLOWED_TAGS.has(node.tag)
  if (!allowed) {
    const text = createTextVNode(typeof node.text === 'string' ? node.text : '')
    return caret.length ? [text, ...caret] : text
  }

  const props = safeProps(node.tag, node.props)
  if (VOID_TAGS.has(node.tag)) return caret.length ? [h(node.tag, props), ...caret] : h(node.tag, props)

  const children = Array.isArray(node.children) ? node.children : []
  const rendered = children
    .map((child, index) => toVNode(child, withCaret && index === children.length - 1))
    .filter((child) => child !== null)
  if (rendered.length === 0 && caret.length) return h(node.tag, props || {}, caret)
  return h(node.tag, props || {}, rendered)
}

export default {
  name: 'MarkdownView',
  props: {
    nodes: { type: Array, default: () => [] },
    /** The streaming cursor, drawn after the last block so it sits in the text. */
    caret: { type: Boolean, default: false },
  },
  render() {
    const nodes = Array.isArray(this.nodes) ? this.nodes : []
    return nodes
      .map((node, index) => toVNode(node, this.caret && index === nodes.length - 1))
      .filter((node) => node !== null)
  },
}
</script>
