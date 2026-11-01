// Promptfoo custom provider for L2/L3: log in, open a new learning-agent session, ask the
// question once, and return the raw record in `metadata.record`.
//
// Promptfoo is only the running shell. The record is the truth, and the assertions are
// pure functions over it (lib/judge.mjs), so scoring can be repeated offline without
// Promptfoo and the two cannot disagree. All spending guards live in TrialRunner.

import { resolveConfig, TrialRunner } from './trial.mjs'

export default class GopherLearnL23Provider {
  constructor(options = {}) {
    this.config = options.config || {}
    this.label = options.label || this.config.label || 'gl'
    this.providerId = options.id || `gopherlearn-l23:${this.label}`
    this.runner = new TrialRunner(resolveConfig(this.config))
  }

  id() {
    return this.providerId
  }

  async callApi(prompt, context) {
    const vars = context?.vars || {}
    const record = await this.runner.run({
      arm: this.label,
      qid: String(vars.id || 'q'),
      rep: context?.repeatIndex ?? 0,
      question: vars.question || prompt,
    })
    const response = { output: record.answer ?? '', metadata: { record } }
    if (record.error) response.error = record.error
    return response
  }
}
