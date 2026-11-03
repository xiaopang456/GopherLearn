# Conversation scoring and scenario validation

These offline tools score short-term memory answers and compare repeated attempts. They do not send model requests. Long-term memory quality and semantic judge calibration are outside this module.

`scenarios.mjs` validates history, evidence turn numbers, timestamps, hash-based development/sealed assignment, and paired condition replacements. `materialize` creates a condition's full history without mutating the source. Independent semantic review and actual solvability checks remain necessary.

`score.mjs` returns `pass`, `fail`, `needs_judge`, or `error`. A scenario whose answer key names its values is judged by what the answer states: it passes when it states the expected value or one of its aliases and states none of the forbidden values, it fails when it states a forbidden value and no expected value, and it stays pending when it states both or when it states neither. A value the answer denies or withdraws does not count as stated, and a mention whose denial is not clear enough to call stays pending rather than being decided. Narrative, open and mixed answers stay pending. Real records without a validated background audit are errors. This boolean must come from the run evidence collector; the scorer does not produce that evidence.

Fact normalization handles Unicode width, whitespace, case and equivalent time units with exact rational arithmetic. For example, `66 分钟`, `66分钟`, `６６分钟`, and `66 min` compare equally. Optional `answer.aliases` lists semantic short names separately; answer keys do not need spelling variants. A stated value is found inside the answer, so a sentence that gives the value passes and a sentence that gives both the expected value and a forbidden value stays pending. A denial binds only inside the clause it stands in: `不是 93 分钟` does not state 93 minutes, `原来的每次 123 分钟不再执行` does not state 123 minutes, and a value that only the answer calls the old one is not confident enough to call either way. Missing-history cases also remain pending for semantic review: acknowledging absent records is acceptable, while inventing specific results is not. The dataset's rubric defines the requested detail and positive control; the scorer does not infer fabrication from a keyword.

```powershell
# Paths must be outside every Git worktree. Output paths must be new.
node eval/l4/cli.mjs score --dataset C:\evaluation\dev.jsonl --records C:\evaluation\records.jsonl --out C:\evaluation\scored
node eval/l4/cli.mjs report --run C:\evaluation\scored --out C:\evaluation\single.json --left current:main
# Within-configuration paired interference comparison:
node eval/l4/cli.mjs report --run C:\evaluation\scored --out C:\evaluation\paired.json --left current:control --right current:distractor
```

Sealed input requires `--split sealed` at both stages. A score manifest binds the dataset, raw records, outcomes and expected scenario inventory. Reports reject modified outcomes. Unknown options, duplicate attempts and output overwrites are refused.

Each scenario expects three attempts, numbered 0 through 2. Single-configuration summaries show conservative and adjudicated denominators, pass^3, complete-case pass^2 and missing scenarios. Pair differences average each scenario's three attempts first, then bootstrap scenarios, preserving repeated-attempt dependence. Paired all-success counts use exact McNemar. Reports retain each scenario's raw three-verdict pairing and distinguish missing attempts, pending judgments and errors. Fake and real observations cannot be mixed in a report.

The primary estimate uses complete pairs. A separate sensitivity section includes every expected scenario: it reports the difference when all unresolved outcomes count as non-success, and bounds from assigning missing/pending/error outcomes opposite success extremes. Those bounds are not confidence intervals. One complete pair has no bootstrap interval. Seed and bootstrap count are recorded.

Sensitivity planning includes an exploratory normal approximation using the variance of independent scenario mean differences, with fixed two-sided alpha .05 and target power .8. It returns null for insufficient/zero observed variance and flags an estimate outside the feasible effect range. An additional exact conditional McNemar estimate uses observed scenario-level discordance for the all-three-pass metric. Zero or too few discordants produce null with a reason, rather than a claim of zero detectable effect. These pilot estimates do not guarantee power in a small or future sample.

```powershell
node --test "eval/l4/test/*.test.mjs"
```

Input records carry `qid`, `arm`, `rep`, `condition`, `env`, `answer`, optional `error`, and the runner's `backgroundAudit`. Malformed identities, duplicate attempts, unknown conditions, inconsistent outcome inventories and modified outcome files are rejected. The final-question runner can export this contract directly as `record.jsonl`; concatenate such files when scoring multiple attempts.

Synthetic fixtures verify arithmetic and control flow only. Fake results are not quality evidence. Semantic judge calibration, independent dataset review and separately authorized real execution remain prerequisites for a final quality evaluation. Open answers remain `needs_judge` until an audited judgment is supplied.

When one run directory holds several scored directories, the later one supersedes the earlier: `score-fact2/` is authoritative and `score-fact/` is the superseded first round, kept for comparison only. A report names the scored directory it used.
