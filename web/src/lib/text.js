const encoder = typeof TextEncoder !== 'undefined' ? new TextEncoder() : null

/** Password policy is measured in bytes, matching the Go side's `len([]byte)`. */
export function utf8ByteLength(value) {
  const text = String(value ?? '')
  if (encoder) return encoder.encode(text).length
  // Fallback for a host without TextEncoder.
  let bytes = 0
  for (let i = 0; i < text.length; i += 1) {
    const code = text.codePointAt(i)
    if (code < 0x80) bytes += 1
    else if (code < 0x800) bytes += 2
    else if (code < 0x10000) bytes += 3
    else {
      bytes += 4
      i += 1
    }
  }
  return bytes
}
