import { describe, expect, it } from 'vitest'

import {
  draftActionText,
  learningLevelText,
  learningStatusText,
  memoryDurabilityText,
  memoryScopeText,
  statusTag,
  statusText,
} from '../src/lib/displayText.js'

describe('the shared wording tables', () => {
  it('says what a known state means', () => {
    expect(statusTag('ready')).toEqual({ text: '就绪', tone: 'success' })
    expect(statusTag('active')).toEqual({ text: '生效中', tone: 'success' })
    expect(statusText('failed')).toBe('失败')
    expect(learningStatusText('waiting_answer')).toBe('等你作答')
    expect(learningLevelText('advanced')).toBe('熟练')
    expect(memoryScopeText('topic')).toBe('主题')
    expect(memoryDurabilityText('time_bound')).toBe('有期限')
    expect(draftActionText('add')).toBe('确认新增')
  })

  it('never shows a machine word it does not know', () => {
    expect(statusTag('cleanup_pending_v2').text).toBe('状态未识别')
    expect(statusTag('x').text).toBe('状态未识别')
    expect(statusTag('').text).toBe('状态未识别')
    // A value that arrived padded or capitalised is the same value.
    expect(statusTag('READY ').text).toBe('就绪')
    expect(learningStatusText('brand_new')).toBe('状态未识别')
    expect(learningLevelText('expert')).toBe('等级未识别')
    expect(memoryScopeText('everything')).toBe('范围未说明')
    expect(memoryDurabilityText('forever')).toBe('有效期未说明')
    expect(draftActionText('merge')).toBe('确认')
  })
})
