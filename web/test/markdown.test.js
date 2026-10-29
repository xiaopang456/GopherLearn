import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { marked } from 'marked'

import MessageContent from '../src/components/MessageContent.vue'
import MessageBubble from '../src/components/MessageBubble.vue'
import { collectNodeKeys, isSafeHref, markdownTree } from '../src/lib/markdown.js'

const NL = String.fromCharCode(10)

/**
 * The reader for model answers.
 *
 * Two properties are worth more than the visual result, so they get the cases:
 * a half-received answer must still render (the learner watches markdown arrive
 * character by character), and formatting must never become markup. The security
 * half lives in `security.test.js`; this file is about what survives while the
 * answer is still arriving, and about the one renderer being the path every answer
 * takes — chat, knowledge and agent turns included.
 */

function render(text, props = {}) {
  return mount(MessageContent, { props: { text, ...props } })
}

const ANSWER = [
  '## 拆包的三种做法',
  '',
  '长度前缀最省事，见 `ReadLoop`。',
  '',
  '1. 定长',
  '2. 分隔符',
  '3. 长度前缀 **推荐**',
  '',
  '> 关键约束：包头必须网络序。',
  '',
  '```go',
  'binary.BigEndian.Uint32(header)',
  '```',
  '',
  '| 方案 | 成本 |',
  '|---|---|',
  '| 定长 | 浪费 |',
  '',
  '---',
  '',
  '细节见 [协议说明](https://example.test/proto)。',
].join(NL)

describe('formatted answer structure', () => {
  it('turns headings, lists, marks, code, quotes, rules and tables into elements', async () => {
    const wrapper = render(ANSWER)
    const body = wrapper.find('[data-test="markdown-body"]')

    expect(body.exists()).toBe(true)
    expect(body.find('h2').text()).toBe('拆包的三种做法')
    expect(body.find('p code').text()).toBe('ReadLoop')
    expect(body.findAll('ol li').map((li) => li.text())).toEqual([
      '定长',
      '分隔符',
      '长度前缀 推荐',
    ])
    expect(body.find('ol li strong').text()).toBe('推荐')
    expect(body.find('blockquote').text()).toContain('包头必须网络序')
    expect(body.find('pre code').attributes('class')).toBe('language-go')
    expect(body.find('pre code').text()).toBe('binary.BigEndian.Uint32(header)')
    expect(body.findAll('table th').map((th) => th.text())).toEqual(['方案', '成本'])
    expect(body.findAll('tbody tr')).toHaveLength(1)
    expect(body.find('hr').exists()).toBe(true)
    expect(body.find('a').attributes('href')).toBe('https://example.test/proto')
    expect(body.find('a').attributes('rel')).toBe('noopener noreferrer')
    wrapper.unmount()
  })

  it('shows formatting rather than the characters that spell it', () => {
    const wrapper = render(ANSWER)
    const text = wrapper.text()

    for (const marker of ['##', '**', '```', '>', '|---|', '[', '](']) {
      expect(text).not.toContain(marker)
    }
    // Cells still read as their own words, just not as pipe syntax.
    expect(text).toContain('长度前缀')
    expect(text).toContain('定长')
  })

  it('keeps single newlines inside a paragraph as line breaks', () => {
    const wrapper = render('第一行' + NL + '第二行')

    expect(wrapper.find('br').exists()).toBe(true)
    expect(wrapper.text()).toContain('第一行')
    expect(wrapper.text()).toContain('第二行')
  })

  it('renders nested lists without flattening the indentation', () => {
    const nested = ['- 甲', '  - 甲一', '- 乙'].join(NL)
    const wrapper = render(nested)

    // Two lists, one inside the other: the reader sees the indent, not a run-on.
    expect(wrapper.findAll('ul')).toHaveLength(2)
    expect(wrapper.findAll('li')).toHaveLength(3)
    expect(wrapper.find('li > ul > li').text()).toBe('甲一')
    expect(wrapper.find('ul').findAll(':scope > li')).toHaveLength(2)
  })

  it('clamps a deep heading, and leaves a non-heading alone', () => {
    const wrapper = render('###### 六级' + NL + NL + '####### 七级')

    // CommonMark stops at six levels, so `#######` is a sentence, not a heading.
    expect(wrapper.findAll('h6')).toHaveLength(1)
    expect(wrapper.find('h7').exists()).toBe(false)
    expect(wrapper.text()).toContain('####### 七级')
  })

  it('keeps a language class only when it can be one', () => {
    const tree = markdownTree('```go' + NL + 'x' + NL + '```')
    expect(JSON.stringify(tree)).toContain('"language-go"')

    const hostile = markdownTree('```"><i src=x onerror=alert(1)>' + NL + 'x' + NL + '```')
    expect(JSON.stringify(hostile)).not.toContain('"class"')
    expect([...collectNodeKeys(hostile).props]).toEqual([])
  })
})

describe('an answer that is still arriving', () => {
  const prefixes = []
  for (let length = 1; length <= ANSWER.length; length += 7) prefixes.push(ANSWER.slice(0, length))
  prefixes.push(ANSWER)

  it.each(prefixes.map((p) => [p]))('renders the prefix %s without throwing', (part) => {
    const wrapper = render(part, { caret: true })
    const body = wrapper.find('[data-test="markdown-body"]')

    // A prefix that happens to be only markup punctuation (`##`) legitimately
    // shows nothing yet — but it must still be the formatted path, and it must
    // never fall back to the "empty reply" placeholder.
    expect(body.exists()).toBe(true)
    expect(wrapper.find('.content__empty').exists()).toBe(false)
    // The stream cursor is drawn inside the block being written.
    expect(body.find('.content__caret').exists()).toBe(true)
    wrapper.unmount()
  })

  it('never leaks a fence or heading marker into the visible text', () => {
    for (const part of prefixes) {
      const wrapper = render(part)
      const text = wrapper.text()
      expect(text).not.toContain('```')
      expect(text).not.toContain('##')
      wrapper.unmount()
    }
  })

  it('renders a heading cut mid-word as a heading, not as a literal hash', () => {
    const wrapper = render('### 标')

    expect(wrapper.find('h3').text()).toBe('标')
    expect(wrapper.text()).not.toContain('#')
  })

  it('renders an open code fence as code while it is still open', () => {
    const wrapper = render('```go' + NL + 'func main() {')

    expect(wrapper.find('pre code').exists()).toBe(true)
    expect(wrapper.find('pre code').text()).toBe('func main() {')
    expect(wrapper.text()).not.toContain('```')
  })

  it('holds an unfinished emphasis run as literal text, then resolves it', () => {
    const partial = render('这里 **加强')
    expect(partial.text()).toBe('这里 **加强')

    const done = render('这里 **加强**')
    expect(done.find('strong').text()).toBe('加强')
    expect(done.text()).not.toContain('**')
  })

  it('ends in exactly the document a single complete render produces', async () => {
    const streamed = render(ANSWER.slice(0, 40))
    await streamed.setProps({ text: ANSWER.slice(0, 90) })
    await streamed.setProps({ text: ANSWER })
    const whole = render(ANSWER)

    expect(streamed.html()).toBe(whole.html())
  })

  it('converges instead of restarting: the DOM is patched, not replaced', async () => {
    const wrapper = render('## 标题' + NL + NL + '正文')
    const heading = wrapper.find('h2').element
    const firstParagraph = wrapper.findAll('p')[0].element

    await wrapper.setProps({ text: '## 标题（补充）' + NL + NL + '正文' })

    // The same nodes are still there, so the reader's scroll position and any
    // text they had selected survive the next delta.
    expect(wrapper.find('h2').element).toBe(heading)
    expect(wrapper.findAll('p')[0].element).toBe(firstParagraph)
    expect(wrapper.find('h2').text()).toBe('标题（补充）')
  })
})

describe('one renderer for every answer', () => {
  it('is the only place model text becomes a node tree', async () => {
    const files = await import('node:fs').then((fs) => fs)
    const path = await import('node:path').then((p) => p)
    // The components and screens, not the library: `lib/markdown.js` is the
    // definition, and this guard is about how many *callers* exist.
    const roots = ['src/components', 'src/views']
    const walk = (dir) =>
      files.readdirSync(dir).flatMap((entry) => {
        const full = path.join(dir, entry)
        if (files.statSync(full).isDirectory()) return walk(full)
        return /\.(js|vue)$/.test(entry) ? [full] : []
      })
    const named = (pattern) =>
      roots
        .flatMap((dir) => walk(path.resolve(process.cwd(), dir)))
        .filter((file) => pattern.test(files.readFileSync(file, 'utf8')))
        .map((file) => path.basename(file))
        .sort()

    // Exactly one component parses an answer…
    expect(named(/markdownTree\(/)).toEqual(['MessageContent.vue'])
    // …one component turns nodes into elements…
    expect(named(/from '\.\/MarkdownView\.vue'/)).toEqual(['MessageContent.vue'])
    // …and one bubble mounts it. A second of any of these would be a second
    // formatting path to audit, and the whole security argument is that there is
    // exactly one.
    expect(named(/MessageContent\.vue'/)).toEqual(['MessageBubble.vue'])
  })

  it('renders chat, knowledge and agent answers with the same structure', async () => {
    const bubble = mount(MessageBubble, {
      props: {
        message: {
          id: 'a1',
          role: 'assistant',
          content: ANSWER,
          status: 'done',
          error: null,
          sources: [
            { sourceId: 'src-1', documentId: 'doc-1', title: 'RPC.md', snippet: '长度前缀' },
          ],
        },
      },
    })

    expect(bubble.find('[data-test="markdown-body"] h2').exists()).toBe(true)
    expect(bubble.find('pre code').exists()).toBe(true)
    // Citations ride beside the formatted body, unchanged: the toggle still counts
    // them, and expanding still names the document.
    const list = bubble.find('[data-test="citation-list"]')
    expect(list.exists()).toBe(true)
    expect(list.text()).toContain('参考来源')
    await bubble.find('[data-test="citation-toggle"]').trigger('click')
    expect(bubble.text()).toContain('RPC.md')
  })

  it('leaves the learner’s own words literal', () => {
    const bubble = mount(MessageBubble, {
      props: {
        message: { id: 'u1', role: 'user', content: '## 我打的字', status: 'done', error: null },
      },
    })

    expect(bubble.find('h2').exists()).toBe(false)
    expect(bubble.find('.content__text').text()).toBe('## 我打的字')
  })
})

describe('input that is not an answer', () => {
  it('shows the empty marker only when there is nothing to show', () => {
    expect(render('').find('.content__empty').text()).toContain('空回复')
    expect(render('   ').find('.content__empty').exists()).toBe(true)
    expect(render(NL + NL).find('.content__empty').exists()).toBe(true)
    expect(render('x').find('.content__empty').exists()).toBe(false)
  })

  it('falls back to the raw text if the parser ever throws', () => {
    const spy = vi.spyOn(marked, 'lexer').mockImplementationOnce(() => {
      throw new Error('parser exploded')
    })

    // Asserted twice on purpose, because there are two independent safety nets:
    // the library hands back the text as one node…
    expect(markdownTree('仍然要看得见的正文')).toEqual([{ text: '仍然要看得见的正文' }])
    // …and the component would still show it even if the library returned nothing.
    const wrapper = render('仍然要看得见的正文')
    expect(wrapper.text()).toBe('仍然要看得见的正文')
    expect(wrapper.find('.content__empty').exists()).toBe(false)
    spy.mockRestore()
    expect(markdownTree('## ok').length).toBe(1)
  })

  it('accepts only absolute safe hrefs, and says so in one place', () => {
    expect(isSafeHref('https://a.test/x')).toBe(true)
    expect(isSafeHref('HTTP://a.test')).toBe(true)
    expect(isSafeHref('mailto:me@example.test')).toBe(true)
    expect(isSafeHref('tel:+123')).toBe(true)
    for (const bad of ['', '  ', '/local', '#frag', '//host', 'data:text/html,x', 'javascript:alert(1)']) {
      expect(isSafeHref(bad)).toBe(false)
    }
  })
})
