# Conversation scoring and scenario validation

These offline tools score short-term memory answers and compare repeated attempts. They do not send model requests. Long-term memory quality and semantic judge calibration are outside this module.

`scenarios.mjs` validates history, evidence turn numbers, timestamps, hash-based development/sealed assignment, and paired condition replacements. `materialize` creates a condition's full history without mutating the source. Independent semantic review and actual solvability checks remain necessary.

`score.mjs` returns `pass`, `fail`, `needs_judge`, or `error`. Canonical factual answers can pass by exact normalized comparison; explicit canonical wrong values can fail. Narrative, negated, mixed-value, and open answers remain pending. Real records without a validated background audit are errors. This boolean must come from the run evidence collector; the scorer does not produce that evidence.

Fact normalization handles Unicode width, whitespace, case and equivalent time units with exact rational arithmetic. For example, `66 分钟`, `66分钟`, `６６分钟`, and `66 min` compare equally. Optional `answer.aliases` lists semantic short names separately; answer keys do not need spelling variants. Only complete canonical answers are matched, so a sentence negating or comparing values still needs judgment. Missing-history cases also remain pending for semantic review: acknowledging absent records is acceptable, while inventing specific results is not. The dataset's rubric defines the requested detail and positive control; the scorer does not infer fabrication from a keyword.

```powershell
# Paths must be outside every Git worktree. Output paths must be new.
node eval/l4/cli.mjs score --dataset C:\evaluation\dev.jsonl --records C:\evaluation\records.jsonl --out C:\evaluation\scored
node eval/l4/cli.mjs report --run C:\evaluation\scored --out C:\evaluation\single.json --left current:main
# Within-configuration paired interference comparison:
node eval/l4/cli.mjs report --run C:\evaluation\scored --out C:\evaluation\paired.json --left current:control --right current:distractor
```

Sealed input requires `--split sealed` at both stages. A score manifest binds the dataset, raw records, outcomes and expected scenario inventory. Reports reject modified outcomes. Unknown options, duplicate attempts and output overwrites are refused.

Each scenario expects three attempts, numbered 0 through 2. Single-configuration summaries show conservative and adjudicated denominators, pass^3, complete-case pass^2 and missing scenarios. Pair differences average each scenario's three attempts first, then bootstrap scenarios, preserving repeated-attempt dependence. Paired all-success counts use exact McNemar. Incomplete pairs are listed and excluded from the complete-pair estimate; do not interpret that estimate without the missingness report. One complete pair has no confidence interval. Seed and bootstrap count are recorded.

```powershell
node --test "eval/l4/test/*.test.mjs"
```

Synthetic fixtures verify arithmetic and control flow only. Fake results are not quality evidence. This implementation still needs integration with final-question records and calibrated semantic judgments, additional malformed-input review, missing-pair sensitivity analysis and minimum-detectable-effect reporting before a final evaluation report.
