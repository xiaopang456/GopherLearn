import { describe, expect, it } from 'vitest'

import { createSSEParser } from '../src/lib/sseParser.js'

function collect(chunks) {
  const parser = createSSEParser()
  const frames = []
  for (const chunk of chunks) frames.push(...parser.consume(chunk))
  frames.push(...parser.end())
  return frames
}

describe('createSSEParser', () => {
  it('parses a named event with a JSON data line', () => {
    const frames = collect(['event: message.delta\ndata: {"delta":"hi"}\n\n'])
    expect(frames).toEqual([
      { event: 'message.delta', data: '{"delta":"hi"}', id: '' },
    ])
  })

  it('treats a missing event name as the default `message` type', () => {
    const [frame] = collect(['data: {}\n\n'])
    expect(frame.event).toBe('message')
  })

  it('joins multiple data lines with a newline', () => {
    const [frame] = collect(['data: line one\ndata: line two\n\n'])
    expect(frame.data).toBe('line one\nline two')
  })

  it('strips exactly one leading space after the colon', () => {
    const [frame] = collect(['data:  two leading spaces\n\n'])
    expect(frame.data).toBe(' two leading spaces')
  })

  it('accepts CR LF and lone CR terminators', () => {
    const [crlf] = collect(['event: a\r\ndata: 1\r\n\r\n'])
    const [cr] = collect(['event: b\rdata: 2\r\r'])
    expect(crlf).toMatchObject({ event: 'a', data: '1' })
    expect(cr).toMatchObject({ event: 'b', data: '2' })
  })

  it('reassembles a frame split across chunk boundaries', () => {
    const frames = collect(['event: mes', 'sage.delta\nda', 'ta: {"delta":', '"x"}', '\n\n'])
    expect(frames).toHaveLength(1)
    expect(frames[0]).toMatchObject({ event: 'message.delta', data: '{"delta":"x"}' })
  })

  it('does not emit anything for a partial frame', () => {
    const parser = createSSEParser()
    expect(parser.consume('event: message.delta\ndata: nope\n')).toEqual([])
    expect(parser.end()).toEqual([
      { event: 'message.delta', data: 'nope', id: '' },
    ])
  })

  it('keeps a trailing CR when the next chunk starts with LF', () => {
    // A chunk ending on the CR of a CRLF pair must not be read as a line
    // terminator: doing so would dispatch `one` early and split the payload.
    const frames = collect(['data: one\r', '\ndata: two\r\n\r\n'])
    expect(frames).toHaveLength(1)
    expect(frames[0].data).toBe('one\ntwo')
  })

  it('ignores comment lines used as heartbeats', () => {
    const frames = collect([': keep-alive\n\n', 'event: a\ndata: 1\n\n'])
    expect(frames).toHaveLength(1)
    expect(frames[0].event).toBe('a')
  })

  it('ignores unknown fields such as retry and id-only blocks', () => {
    const frames = collect(['retry: 3000\nid: 42\n\n'])
    expect(frames).toEqual([])
  })

  it('dispatches a named event that carries no data', () => {
    const [frame] = collect(['event: message.completed\n\n'])
    expect(frame).toMatchObject({ event: 'message.completed', data: '' })
  })

  it('emits a data-less frame that the stream closes mid-block', () => {
    const parser = createSSEParser()
    parser.consume('event: message.delta\ndata: tail')
    expect(parser.end()).toEqual([
      { event: 'message.delta', data: 'tail', id: '' },
    ])
  })
})
