import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'

import BaselineSurfaceNotice from '../src/components/BaselineSurfaceNotice.vue'

describe('BaselineSurfaceNotice', () => {
  it('says which entry the page belongs to', () => {
    const wrapper = mount(BaselineSurfaceNotice, {
      props: { replacement: { href: '/memory/v2', label: '记忆治理' } },
    })
    expect(wrapper.get('[data-test="baseline-surface-notice"]').text()).toContain('历史基线入口')
    expect(wrapper.text()).toContain('整合入口不提供这组接口')
    wrapper.unmount()
  })

  it('points at the page that covers the same ground', () => {
    const wrapper = mount(BaselineSurfaceNotice, {
      props: { replacement: { href: '/memory/v2', label: '记忆治理' } },
    })
    const link = wrapper.get('a')
    expect(link.attributes('href')).toBe('/memory/v2')
    expect(link.text()).toBe('记忆治理')
    wrapper.unmount()
  })

  it('omits the link when no replacement is named', () => {
    const wrapper = mount(BaselineSurfaceNotice)
    expect(wrapper.find('a').exists()).toBe(false)
    expect(wrapper.get('[data-test="baseline-surface-notice"]').exists()).toBe(true)
    wrapper.unmount()
  })
})
