// The corpus manifest, read the way the Go side reads it (internal/dataset), so the hash
// a run pins and the hash the Go tools print are the same number.

import crypto from 'node:crypto'
import fs from 'node:fs'
import path from 'node:path'

import { makeCorpus } from './citations.mjs'

/** Lines of a document as evidence spans number them: "\n" ends a line, a final line without one still counts. */
export function countLines(buffer) {
  if (buffer.length === 0) return 0
  let n = 0
  for (const byte of buffer) if (byte === 0x0a) n += 1
  return buffer[buffer.length - 1] === 0x0a ? n : n + 1
}

/**
 * @returns { name, hash, files: {file: lines}, corpus }, where `corpus` is what the
 * citation checks take. `uploadNames` (cited name -> manifest name) is optional.
 */
export function loadCorpusManifest(manifestPath, uploadNames = {}) {
  const manifest = JSON.parse(fs.readFileSync(manifestPath, 'utf8').replace(/^﻿/, ''))
  if (!Array.isArray(manifest.documents) || manifest.documents.length === 0) throw new Error(`${manifestPath}: documents is empty`)
  const root = path.join(path.dirname(manifestPath), manifest.root ?? '')
  const files = {}
  const pairs = []
  for (const entry of manifest.documents) {
    const data = fs.readFileSync(path.join(root, ...String(entry.file).split('/')))
    files[entry.file] = countLines(data)
    pairs.push(`${entry.file}\u0000${crypto.createHash('sha256').update(data).digest('hex')}`)
  }
  pairs.sort()
  const hash = crypto.createHash('sha256').update(pairs.join('\n')).digest('hex')
  return { name: manifest.name, hash, files, corpus: makeCorpus({ files, uploadNames }) }
}
