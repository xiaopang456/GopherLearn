import { describe, expect, it } from 'vitest'

import {
  allText,
  applyTextBlockEvent,
  confirmAnswerBlock,
  createBlockState,
  finishBlocks,
  isBuilding,
  isTextBlockEventName,
  processNotes,
  sealBlocks,
  TextBlockKind,
  toTextBlockEvent,
  turnTranscript,
} from '../src/lib/agentTextBlocks.js'

/**
 * The agent turn's published text, kept as the blocks it arrived in.
 *
 * Every case here is about a claim the page is allowed to make. A block is only an answer
 * once `message.completed` names it; a note is never folded away while the answer is still
 * being watched being written; a fragment that arrives after the turn ended cannot lengthen
 * a paragraph the learner already has an explanation for; and a kind the server did not name
 * is dropped rather than guessed, because guessing is how a failed round ends up displayed
 * as a finished answer.
 */

function frame(event, payload) {
  return { event, data: JSON.stringify(payload) }
}

function started(blockId, round, runId = 'run-1') {
  return { phase: 'agent.text.started', blockId, round, runId, content: '', kind: '' }
}

function delta(blockId, content, runId = 'run-1') {
  return { phase: 'agent.text.delta', blockId, round: 0, runId, content, kind: '' }
}

function ended(blockId, kind, runId = 'run-1') {
  return { phase: 'agent.text.ended', blockId, round: 0, runId, content: '', kind }
}

function opened(...fragments) {
  return fragments.reduce((state, event) => applyTextBlockEvent(state, event), createBlockState())
}

describe('reading a text frame', () => {
  it('claims only the three names it knows', () => {
    expect(isTextBlockEventName('agent.text.started')).toBe(true)
    expect(isTextBlockEventName('agent.text.delta')).toBe(true)
    expect(isTextBlockEventName('agent.text.ended')).toBe(true)
    for (const other of ['message.delta', 'step_started', 'tool_result', 'agent.text.hidden']) {
      expect(isTextBlockEventName(other)).toBe(false)
      expect(toTextBlockEvent(frame(other, { block_id: 'blk-1', content: 'x' }))).toBe(null)
    }
  })

  it('reads the keys it is allowed to know and nothing else', () => {
    const event = toTextBlockEvent(
      frame('agent.text.delta', {
        run_id: 'run-8',
        block_id: 'blk-2',
        content: '不会保留消息边界',
        session_id: 'sess-smuggled',
        score: 0.9,
      }),
    )
    expect(event).toEqual({
      phase: 'agent.text.delta',
      blockId: 'blk-2',
      round: 0,
      runId: 'run-8',
      content: '不会保留消息边界',
      kind: '',
    })
    expect(JSON.stringify(event)).not.toContain('sess-smuggled')
    expect(JSON.stringify(event)).not.toContain('score')
  })

  it('refuses a frame that cannot say which block it belongs to', () => {
    expect(toTextBlockEvent(frame('agent.text.delta', { content: '无主的一段' }))).toBe(null)
    expect(toTextBlockEvent(frame('agent.text.delta', { block_id: '   ', content: 'x' }))).toBe(null)
  })

  it('keeps a round number only when it is one', () => {
    expect(toTextBlockEvent(frame('agent.text.started', { block_id: 'b', round: 3 })).round).toBe(3)
    for (const bad of [0, -1, 1.5, '2', null]) {
      expect(toTextBlockEvent(frame('agent.text.started', { block_id: 'b', round: bad })).round).toBe(0)
    }
  })

  it('accepts only the closed set of kinds, and reports nothing else as no kind', () => {
    expect(ended('b', toTextBlockEvent(frame('agent.text.ended', { block_id: 'b', kind: 'process' })).kind).kind)
      .toBe(TextBlockKind.Process)
    for (const bogus of ['answer', 'completed', 'PROCESS', '', 'ready_to_answer']) {
      expect(toTextBlockEvent(frame('agent.text.ended', { block_id: 'b', kind: bogus })).kind).toBe('')
    }
  })

  it('drops a frame whose body cannot be read', () => {
    expect(toTextBlockEvent({ event: 'agent.text.delta', data: '{not json' })).toBe(null)
    expect(toTextBlockEvent({ event: 'agent.text.delta', data: '[1,2]' })).toBe(null)
  })
})

describe('growing a block', () => {
  it('appends fragments in the order they arrived', () => {
    const state = opened(
      started('blk-1', 1),
      delta('blk-1', 'TCP 是字节流，'),
      delta('blk-1', '不会保留消息边界'),
      delta('blk-1', '。'),
    )
    expect(allText(state)).toBe('TCP 是字节流，不会保留消息边界。')
    expect(isBuilding(state)).toBe(true)
  })

  it('keeps two rounds in two blocks', () => {
    const state = opened(
      started('blk-1', 1),
      delta('blk-1', '我先查一下。'),
      ended('blk-1', TextBlockKind.Process),
      started('blk-2', 2),
      delta('blk-2', '答案是 everysec。'),
      ended('blk-2', TextBlockKind.Candidate),
    )
    expect(state.blocks.map((block) => `${block.blockId}:${block.text}`)).toEqual([
      'blk-1:我先查一下。',
      'blk-2:答案是 everysec。',
    ])
  })

  it('refuses to reopen a block, and refuses to write into one that has finished', () => {
    const before = opened(started('blk-1', 1), delta('blk-1', '一句'))
    expect(applyTextBlockEvent(before, started('blk-1', 1)).ignoredEvents).toBe(1)
    const closed = applyTextBlockEvent(before, ended('blk-1', TextBlockKind.Candidate))
    expect(applyTextBlockEvent(closed, delta('blk-1', '迟到的一句')).blocks[0].text).toBe('一句')
  })

  it('shows a fragment whose opening frame never arrived, rather than losing it', () => {
    const state = opened(delta('blk-3', '半路来的文字'))
    expect(allText(state)).toBe('半路来的文字')
    expect(state.blocks[0].round).toBe(0)
    expect(isBuilding(state)).toBe(true)
  })

  it('counts a frame from another turn instead of drawing it', () => {
    const mine = opened(started('blk-1', 1), delta('blk-1', '我的'))
    const after = applyTextBlockEvent(mine, delta('blk-1', '别人的', 'run-other'))
    expect(allText(after)).toBe('我的')
    expect(after.ignoredEvents).toBe(1)
  })

  it('refuses an ending frame that cannot say what the text was', () => {
    const before = opened(started('blk-1', 1), delta('blk-1', '一句话'))
    const after = applyTextBlockEvent(before, { ...ended('blk-1', ''), kind: '' })
    expect(after.ignoredEvents).toBe(1)
    expect(after.blocks[0].ended).toBe(false)
  })
})

describe('confirming the answer', () => {
  const candidate = opened(
    started('blk-1', 1),
    delta('blk-1', '我先查一下笔记。'),
    ended('blk-1', TextBlockKind.Process),
    started('blk-2', 2),
    delta('blk-2', 'TCP 是字节流，'),
    delta('blk-2', '不会保留消息边界。'),
    ended('blk-2', TextBlockKind.Candidate),
  )

  it('promotes only the block the completed frame named', () => {
    const confirmed = confirmAnswerBlock(candidate, 'blk-2')
    expect(confirmed.confirmedId).toBe('blk-2')
    expect(turnTranscript(confirmed)).toEqual({
      content: 'TCP 是字节流，不会保留消息边界。',
      processNotes: [{ blockId: 'blk-1', round: 1, text: '我先查一下笔记。' }],
      confirmed: true,
    })
  })

  it('will not name a note as the answer', () => {
    expect(confirmAnswerBlock(candidate, 'blk-1').confirmedId).toBe('')
    expect(confirmAnswerBlock(candidate, 'blk-9').confirmedId).toBe('')
  })

  it('will not confirm a block that is still being written', () => {
    const writing = opened(started('blk-1', 1), delta('blk-1', '还没写完'))
    expect(confirmAnswerBlock(writing, 'blk-1').confirmedId).toBe('')
  })

  it('will not confirm a round that broke', () => {
    const broken = opened(
      started('blk-1', 1),
      delta('blk-1', '写了一半'),
      ended('blk-1', TextBlockKind.Failed),
    )
    expect(confirmAnswerBlock(broken, 'blk-1').confirmedId).toBe('')
    // What the learner already read stays on screen, labelled by the ending frame alone.
    expect(allText(broken)).toBe('写了一半')
  })

  it('holds one answer per turn, so a second confirmation cannot move it', () => {
    const twice = opened(
      started('blk-1', 1),
      delta('blk-1', '第一段'),
      ended('blk-1', TextBlockKind.Candidate),
      started('blk-2', 2),
      delta('blk-2', '第二段'),
      ended('blk-2', TextBlockKind.Candidate),
    )
    const once = confirmAnswerBlock(twice, 'blk-1')
    expect(confirmAnswerBlock(once, 'blk-2').confirmedId).toBe('blk-1')
    expect(turnTranscript(once).content).toBe('第一段')
  })

  it('keeps everything inline while nothing is confirmed', () => {
    const view = turnTranscript(candidate)
    expect(view.content).toBe('我先查一下笔记。TCP 是字节流，不会保留消息边界。')
    // A note folded beside a body that still contains it would be the same sentence twice.
    expect(view.processNotes).toEqual([])
    expect(view.confirmed).toBe(false)
  })

  it('reports no text at all for a turn that published none', () => {
    // This is the signal the caller uses to fall back to the answer channel a chat turn,
    // a knowledge turn, and a legacy agent turn still rely on.
    expect(turnTranscript(createBlockState())).toEqual({
      content: '',
      processNotes: [],
      confirmed: false,
    })
  })
})

describe('a turn that ended without saying how', () => {
  it('labels unfinished text as broken rather than as an answer', () => {
    const mid = opened(started('blk-1', 1), delta('blk-1', '写到一半的句子'))
    const closed = finishBlocks(mid)
    expect(closed.blocks[0].kind).toBe(TextBlockKind.Failed)
    expect(closed.blocks[0].ended).toBe(true)
    expect(allText(closed)).toBe('写到一半的句子')
    expect(confirmAnswerBlock(closed, 'blk-1').confirmedId).toBe('')
  })

  it('refuses every frame after the turn is over', () => {
    const sealed = sealBlocks(opened(started('blk-1', 1), delta('blk-1', '正文'), ended('blk-1', TextBlockKind.Candidate)))
    const after = applyTextBlockEvent(sealed, delta('blk-1', '迟到的追加'))
    expect(allText(after)).toBe('正文')
    expect(after.ignoredEvents).toBe(1)
  })

  it('refuses a round that opened after the turn ended', () => {
    // A block already on screen is protected by its own ending frame. This is the other case:
    // a provider that started a new paragraph after the learner cancelled. The page has
    // already said what happened in that turn, so a paragraph cannot be added to it.
    const sealed = sealBlocks(opened(started('blk-1', 1), delta('blk-1', '正文')))
    const after = applyTextBlockEvent(applyTextBlockEvent(sealed, started('blk-2', 2)), delta('blk-2', '新开的一段'))
    expect(allText(after)).toBe('正文')
    expect(after.blocks.map((block) => block.blockId)).toEqual(['blk-1'])
    expect(after.ignoredEvents).toBe(2)
  })

  it('leaves the blocks it was given alone when nothing is open', () => {
    const done = opened(started('blk-1', 1), delta('blk-1', '一句'), ended('blk-1', TextBlockKind.Process))
    expect(finishBlocks(done).blocks[0].kind).toBe(TextBlockKind.Process)
  })
})

describe('selectors', () => {
  it('report the notes in the order the model wrote them', () => {
    const state = opened(
      started('blk-1', 1),
      delta('blk-1', '第一条说明'),
      ended('blk-1', TextBlockKind.Process),
      started('blk-2', 2),
      delta('blk-2', '空的一段'),
      ended('blk-2', TextBlockKind.Process),
      started('blk-3', 3),
      delta('blk-3', '第三条说明'),
      ended('blk-3', TextBlockKind.Process),
    )
    expect(processNotes(state).map((note) => note.round)).toEqual([1, 2, 3])
    expect(processNotes(state)).toHaveLength(3)
    expect(isBuilding(state)).toBe(false)
  })

  it('drops a note with nothing in it', () => {
    const state = opened(started('blk-1', 1), ended('blk-1', TextBlockKind.Process))
    expect(processNotes(state)).toEqual([])
  })
})
