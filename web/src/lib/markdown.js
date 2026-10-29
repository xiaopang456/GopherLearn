import { marked } from 'marked'

/**
 * Model output → a *node tree*, never an HTML string.
 *
 * The one rule this module exists to enforce: untrusted text cannot become markup.
 * A parser plus a sanitiser would also work, but "render then scrub" leaves a
 * window where a hostile document has already been assembled, and it needs a raw
 * markup sink (the directive and property this client bans outright in
 * `eslint.config.js`).
 * So an answer's shape is built here from a closed whitelist: tags and attributes
 * are named by this file and by nothing else, and anything the whitelist does not
 * name is rendered as **text**. An `<img onerror>` in a model answer therefore
 * reaches the reader as the literal characters it was written as, and
 * `[x](javascript:…)` becomes its label with no anchor at all.
 *
 * Streaming matters as much as the finished document: answers arrive token by
 * token, so a half-open fence, a half-written `**bold` or a heading cut at
 * `### 标` must render as far as it currently reads and converge when the rest
 * arrives. `marked`'s lexer tolerates all three; the fallback below covers the
 * case it ever does not — a parser throw may cost the reader their formatting,
 * never their text.
 */

const MAX_HEADING_DEPTH = 6
const ALLOWED_SCHEMES = ['http:', 'https:', 'mailto:', 'tel:']

/**
 * A real URL contains no control characters and no whitespace, so removing them is
 * loss-free — and it is what stops `java` + tab + `script:` from reading as a
 * scheme. Written without a control-character regex so the lint rule that keeps
 * such patterns out of this codebase stays enabled everywhere else.
 */
function compact(value) {
  let out = ''
  for (const character of String(value ?? '')) {
    const point = character.codePointAt(0)
    if (point > 0x20 && point !== 0x7f) out += character
  }
  return out
}

/**
 * Absolute `http(s)`, `mailto:` and `tel:` only. Scheme-less and relative links
 * are refused as well: an answer has no reason to name a route in this app, and
 * "refused" is a smaller set of behaviours to reason about than "rewritten".
 */
export function isSafeHref(value) {
  const cleaned = compact(value)
  if (cleaned === '') return false
  const scheme = /^([a-z][a-z0-9+.-]*:)/i.exec(cleaned)
  if (!scheme) return false
  return ALLOWED_SCHEMES.includes(scheme[1].toLowerCase())
}

function textNode(value) {
  return { text: String(value ?? '') }
}

function element(tag, children, props = null) {
  return { tag, props, children: Array.isArray(children) ? children : [children] }
}

/** `lang` becomes a class name, so only something a class can hold is kept. */
function languageClass(lang) {
  const first = compact(lang).split(/\s+/)[0].toLowerCase()
  if (!/^[a-z0-9][a-z0-9+._-]{0,31}$/.test(first)) return null
  return `language-${first}`
}

function inlineNodes(tokens) {
  if (!Array.isArray(tokens)) return []
  const out = []
  for (const token of tokens) out.push(...inlineNode(token))
  return out
}

function emphasis(token) {
  const nested = inlineNodes(token.tokens)
  return [element(token.type, nested.length ? nested : [textNode(token.text)])]
}

function inlineNode(token) {
  if (!token || typeof token !== 'object') return []
  switch (token.type) {
    case 'strong':
    case 'em':
    case 'del':
      return emphasis(token)
    case 'codespan':
      return [element('code', [textNode(token.text)])]
    case 'br':
      return [element('br', [])]
    case 'link': {
      const nested = inlineNodes(token.tokens)
      const label = nested.length ? nested : [textNode(token.text)]
      if (!isSafeHref(token.href)) return label
      return [element('a', label, { href: compact(token.href), rel: 'noopener noreferrer' })]
    }
    case 'image':
      // An inline image is a third-party fetch and a load-failure handler hook
      // (`onerror` on a broken src). The alt text is the useful half of it.
      return [textNode(token.text ? `[图片：${token.text}]` : '[图片]')]
    case 'escape':
      return [textNode(token.text)]
    case 'html':
      return [textNode(token.raw ?? token.text ?? '')]
    case 'text': {
      const nested = inlineNodes(token.tokens)
      return nested.length ? nested : [textNode(token.text ?? token.raw ?? '')]
    }
    default:
      return [textNode(token.text ?? token.raw ?? '')]
  }
}

function blockNodes(tokens) {
  if (!Array.isArray(tokens)) return []
  const out = []
  for (const token of tokens) {
    const node = blockNode(token)
    if (node) out.push(node)
  }
  return out
}

function blockNode(token) {
  if (!token || typeof token !== 'object') return null
  switch (token.type) {
    case 'space':
      return null
    case 'heading': {
      const depth = Math.min(Math.max(Number(token.depth) || 1, 1), MAX_HEADING_DEPTH)
      return element(`h${depth}`, inlineNodes(token.tokens))
    }
    case 'paragraph':
      return element('p', inlineNodes(token.tokens))
    case 'blockquote':
      return element('blockquote', blockNodes(token.tokens))
    case 'hr':
      return element('hr', [])
    case 'code': {
      const lang = languageClass(token.lang)
      return element('pre', [element('code', [textNode(token.text)], lang ? { class: lang } : null)])
    }
    case 'list': {
      const items = (Array.isArray(token.items) ? token.items : []).map((item) =>
        element('li', listItemChildren(item)),
      )
      return element(token.ordered ? 'ol' : 'ul', items)
    }
    case 'table':
      return tableNode(token)
    case 'html':
      return textNode(token.raw ?? token.text ?? '')
    case 'text': {
      const nested = inlineNodes(token.tokens)
      return nested.length ? element('p', nested) : textNode(token.text ?? token.raw ?? '')
    }
    default:
      // An unknown block is still content the learner meant to read. Rendering it
      // as text loses formatting; dropping it would lose the sentence, silently.
      return textNode(token.text ?? token.raw ?? '')
  }
}

/**
 * A tight list item carries a `text` token whose own `tokens` hold the inline
 * marks; a loose one carries whole blocks. Both shapes come from the same source
 * text depending on nothing but blank lines, so neither may be assumed away.
 */
function listItemChildren(item) {
  const tokens = Array.isArray(item?.tokens) ? item.tokens : []
  if (tokens.length === 0) return [textNode(item?.text ?? '')]
  const out = []
  for (const token of tokens) {
    if (token && token.type === 'text' && Array.isArray(token.tokens) && token.tokens.length > 0) {
      out.push(...inlineNodes(token.tokens))
    } else {
      const node = blockNode(token)
      if (node) out.push(node)
    }
  }
  return out.length ? out : [textNode(item?.text ?? '')]
}

function tableNode(token) {
  const head = (Array.isArray(token.header) ? token.header : []).map((cell) =>
    element('th', cellNodes(cell)),
  )
  const body = (Array.isArray(token.rows) ? token.rows : []).map((row) =>
    element(
      'tr',
      (Array.isArray(row) ? row : []).map((cell) => element('td', cellNodes(cell))),
    ),
  )
  const children = []
  if (head.length > 0) children.push(element('thead', [element('tr', head)]))
  if (body.length > 0) children.push(element('tbody', body))
  // The scroller is this file's own wrapper, not markup from the answer: a wide
  // table must scroll inside its row instead of widening the page.
  return element('div', [element('table', children)], { class: 'md-scroll' })
}

function cellNodes(cell) {
  const nested = inlineNodes(cell?.tokens)
  return nested.length ? nested : [textNode(cell?.text ?? '')]
}

/**
 * @param {string} source raw model text
 * @returns {Array<object>} whitelist nodes, `[]` when there is nothing to show
 */
export function markdownTree(source) {
  const text = typeof source === 'string' ? source : ''
  if (text.trim() === '') return []
  try {
    return blockNodes(marked.lexer(text, { gfm: true, breaks: true }))
  } catch {
    return [textNode(text)]
  }
}

/**
 * What a hostile document is allowed to produce. Exported so the guard is a fact
 * a test can enumerate over payloads rather than a comment nobody re-reads.
 */
export const ALLOWED_MARKDOWN_TAGS = Object.freeze([
  'div',
  'p',
  'h1',
  'h2',
  'h3',
  'h4',
  'h5',
  'h6',
  'ul',
  'ol',
  'li',
  'blockquote',
  'pre',
  'code',
  'hr',
  'br',
  'strong',
  'em',
  'del',
  'a',
  'table',
  'thead',
  'tbody',
  'tr',
  'th',
  'td',
])

/** Every tag and every prop key the tree actually uses — for the whitelist test. */
export function collectNodeKeys(nodes, tags = new Set(), props = new Set()) {
  for (const node of nodes || []) {
    if (!node || typeof node !== 'object') continue
    if (node.text !== undefined) continue
    if (node.tag) tags.add(node.tag)
    for (const key of Object.keys(node.props || {})) props.add(key)
    collectNodeKeys(node.children, tags, props)
  }
  return { tags, props }
}
