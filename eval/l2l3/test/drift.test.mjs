// The tables in lib/ restate things the product defines. These tests read the product's
// source and fail when the two disagree, so adding a tool or a state in the product
// without updating the evaluation is a red test and not a silent mis-count.

import test from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { TOOL_CAPABILITIES, Capability } from '../lib/capabilities.mjs'
import { AnswerBasis, KnowledgeReason, KnowledgeStatus, KnowledgePolicy, SUCCESS_CODE } from '../lib/enums.mjs'

const repo = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..', '..')
const read = (...parts) => fs.readFileSync(path.join(repo, ...parts), 'utf8')

/** `Name  Type = "value"` constants (and plain `name = "value"` ones) of a Go file. */
function goStringConstants(source) {
  const out = new Map()
  for (const match of source.matchAll(/^\s*([A-Za-z][A-Za-z0-9]*)\s+(?:[A-Za-z][A-Za-z0-9]*\s+)?=\s*"([^"]*)"\s*$/gm)) {
    out.set(match[1], match[2])
  }
  return out
}

test('every tool name the native entry defines is in the capability table, with the same capability', () => {
  const capabilityFile = read('v2', 'service', 'agent', 'tool_capability.go')
  const runnerFile = read('v2', 'service', 'agent', 'runner.go')
  const constants = new Map([...goStringConstants(runnerFile), ...goStringConstants(capabilityFile)])

  // The Go side: name constant -> capability constant, then capability constant -> its string.
  const mapping = capabilityFile.match(/var toolCapabilities = map\[string\]MemoryToolCapability\{([\s\S]*?)\n\}/)
  assert.ok(mapping, 'toolCapabilities was not found in tool_capability.go')
  const goNameToCapability = new Map()
  for (const entry of mapping[1].matchAll(/([A-Za-z][A-Za-z0-9]*)\s*:\s*(Capability[A-Za-z]+)/g)) {
    const name = constants.get(entry[1])
    const capability = constants.get(entry[2])
    assert.ok(name, `no string value for the tool-name constant ${entry[1]}`)
    assert.ok(capability, `no string value for ${entry[2]}`)
    goNameToCapability.set(name, capability)
  }
  assert.ok(goNameToCapability.size >= 9, `only found ${goNameToCapability.size} mapped names; the parser is stale`)

  for (const [name, capability] of goNameToCapability) {
    assert.equal(TOOL_CAPABILITIES[name], capability, `tool ${name}: Go says ${capability}, the table says ${TOOL_CAPABILITIES[name]}`)
  }
  // And every name the Runner declares at all is mapped.
  for (const [constant, value] of goStringConstants(runnerFile)) {
    if (/ToolName$/.test(constant)) assert.ok(Object.hasOwn(TOOL_CAPABILITIES, value), `${constant} = ${value} is not in the capability table`)
  }
})

test('every tool name of the integrated entry is in the capability table', () => {
  const dir = path.join(repo, 'internal', 'agent')
  const names = []
  for (const file of fs.readdirSync(dir).filter((f) => /^tool_.*\.go$/.test(f) && !f.endsWith('_test.go'))) {
    for (const match of fs.readFileSync(path.join(dir, file), 'utf8').matchAll(/^const\s+[A-Za-z]+Name\s*=\s*"([a-z_]+)"/gm)) names.push(match[1])
  }
  assert.ok(names.length >= 5, `found only ${names.length} integrated tool names: ${names}`)
  for (const name of names) assert.ok(Object.hasOwn(TOOL_CAPABILITIES, name), `integrated tool ${name} is not in the capability table`)
})

test('the capability names the table uses are the ones the Go side defines', () => {
  const constants = goStringConstants(read('v2', 'service', 'agent', 'tool_capability.go'))
  const goCapabilities = new Set([...constants].filter(([k]) => k.startsWith('Capability') && k !== 'CapabilityUnknown').map(([, v]) => v))
  const ours = new Set(Object.values(TOOL_CAPABILITIES))
  assert.deepEqual([...ours].sort(), [...goCapabilities].sort())
  assert.ok(ours.has(Capability.KnowledgeSearch))
})

function enumValues(source, typeName) {
  const values = []
  for (const match of source.matchAll(new RegExp(`^\\s*[A-Za-z][A-Za-z0-9]*\\s+${typeName}\\s*=\\s*"([^"]*)"`, 'gm'))) values.push(match[1])
  return values.sort()
}

test('the state enums equal answer_policy.go', () => {
  const source = read('v2', 'common', 'rag', 'answer_policy.go')
  assert.deepEqual(Object.values(AnswerBasis).sort(), enumValues(source, 'AnswerBasis'))
  assert.deepEqual(Object.values(KnowledgeStatus).sort(), enumValues(source, 'KnowledgeStatus'))
  assert.deepEqual(Object.values(KnowledgeReason).sort(), enumValues(source, 'KnowledgeReason'))
  assert.deepEqual(Object.values(KnowledgePolicy).sort(), enumValues(source, 'KnowledgePolicy'))
})

test('the success code is the one the product returns', () => {
  const code = read('v2', 'common', 'code', 'code.go')
  assert.match(code, new RegExp(`CodeSuccess[^\\n]*${SUCCESS_CODE}`))
})
