// Evaluation data never goes inside a git working tree.
//
// The raw records hold answers and cited passages from the user's private documents. The
// same rule the Go side enforces (internal/pathguard) applies here: a configured
// directory that sits inside any repository is refused, so a stray `git add .` cannot
// publish it. A directory that does not exist yet is judged by its nearest existing
// ancestor, links followed, and both a `.git` directory and a linked worktree's `.git`
// file count.

import fs from 'node:fs'
import path from 'node:path'

export function insideWorktree(target) {
  let dir = path.resolve(target)
  while (!fs.existsSync(dir)) {
    const parent = path.dirname(dir)
    if (parent === dir) return null
    dir = parent
  }
  dir = fs.realpathSync(dir)
  if (!fs.statSync(dir).isDirectory()) dir = path.dirname(dir)
  for (;;) {
    if (fs.existsSync(path.join(dir, '.git'))) return dir
    const parent = path.dirname(dir)
    if (parent === dir) return null
    dir = parent
  }
}

/** Throws unless `target` is outside every git working tree. `name` labels it in the error. */
export function assertOutsideWorktree(name, target) {
  if (typeof target !== 'string' || target === '') throw new Error(`${name} is not set`)
  const tree = insideWorktree(target)
  if (tree !== null) throw new Error(`${name} = ${target} is inside a git working tree (${tree}); keep evaluation data outside every repository`)
}
