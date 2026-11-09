# Conversation scoring and scenario validation

These offline tools score short-term memory answers and compare repeated attempts. They do not send model requests. Long-term memory quality and semantic judge calibration are outside this module.

`scenarios.mjs` validates history, evidence turn numbers, timestamps, hash-based development/sealed assignment, and paired condition replacements. `materialize` creates a condition's full history without mutating the source. Independent semantic review and actual solvability checks remain necessary.

`score.mjs` returns `pass`, `fail`, `needs_judge`, or `error`. A scenario whose answer key names its values is judged by what the answer states: it passes when it states the expected value or one of its aliases and states none of the forbidden values, it fails when it states a forbidden value and no expected value, and it stays pending when it states both or when it states neither. A value the answer denies or withdraws does not count as stated, and a mention whose denial is not clear enough to call stays pending rather than being decided. Narrative, open and mixed answers stay pending. Real records without a validated background audit are errors. This boolean must come from the run evidence collector; the scorer does not produce that evidence.

Fact normalization handles Unicode width, whitespace, case and equivalent time units with exact rational arithmetic. For example, `66 分钟`, `66分钟`, `６６分钟`, and `66 min` compare equally. Optional `answer.aliases` lists semantic short names separately; answer keys do not need spelling variants. A stated value is found inside the answer, so a sentence that gives the value passes and a sentence that gives both the expected value and a forbidden value stays pending. A denial binds only inside the clause it stands in: `不是 93 分钟` does not state 93 minutes, `原来的每次 123 分钟不再执行` does not state 123 minutes, and a value that only the answer calls the old one is not confident enough to call either way. Missing-history cases also remain pending for semantic review: acknowledging absent records is acceptable, while inventing specific results is not. The dataset's rubric defines the requested detail and positive control; the scorer does not infer fabrication from a keyword.

The measure words and the normalization itself live in `normalize.mjs`, and both the scorer and the answer key's own comparison call it. One table, one function: `9道` and `九道` are the same value in an answer, in an accepted list and in an alias list. Every quantity with a measure word is normalized - the number becomes Arabic digits (Chinese numerals up to 万 are read), a length of time becomes seconds as an exact fraction, and a count becomes `quantity:<number>:<unit>`. A value whose number cannot be read is not guessed at: it keeps its written form and is matched literally. The answer key compares its own values after the normalization, so a key that accepts one spelling of a value cannot forbid another spelling of the same value.

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

## Comparing two rolling-summary policies

A summary that outgrew its compression budget is trimmed instead of refused. The tools above answer whether the answers got worse; these three answer what the trimming cost and whether what it cost was the evidence a question is about.

`truncation-report` reads the warmup report of every prepared conversation and the frozen scenarios, and matches the messages the dropped items cited to the turns they belong to. A scenario's evidence turns are known by their position in the history, so a lost evidence turn is a turn whose message was cited by an item the budget dropped. The report says, per scenario, how many items and messages were dropped, which evidence turns were among them, and how many messages could not be matched to a turn at all.

`judge-input` prepares the rows a judge has to read for the scenarios whose answers are not decided by the structure. Each row is one check point of one attempt, and the model reads the `input` object and nothing else:

```json
{
  "item_id": "l4-constraint-01",
  "arm": "baseline",
  "condition": "main",
  "rep": 0,
  "check_point_index": 0,
  "input": {
    "question": "...",
    "answer_key": { "mode": "needs_judge", "required": ["只用 Go 标准库"] },
    "evidence": [{ "turn": 6, "user": "...", "assistant": "..." }],
    "response": "...",
    "check_point": "只用 Go 标准库"
  }
}
```

The identity of the attempt — the scenario, the arm, the condition, the repeat and the check point — sits beside the payload and is not part of what the model reads: the judge is asked about an answer, not about which run produced it. The evidence is the history the question is about, taken whole and uncut. The check points are the ones the scenario requires, one call each; a scenario that requires nothing and asks for a current answer gets one synthetic check point.

The evidence follows one fixed rule. The fabrication scenarios are read at every turn that mentions the name their question quotes, matched as a plain substring of the raw text, and the whole turn is taken. The other categories are read at the turns the scenario names, with no context added. A scenario where the rule finds nothing is written as a row with an error and no input, and the command says so and exits non-zero: nothing is guessed and nothing is silently dropped.

The tool does not judge. Whether an answer obeyed a stated constraint is a matter of meaning, and a script that pretended to decide it would report a number it did not earn.

`category-report` compares two arms per category. A judgement settles one check point of an attempt, and an attempt is decided only when every one of its check points is judged: one "no" fails it, otherwise one "error" fails it as a run, otherwise one "unsure" leaves it undecided, and only "yes" everywhere passes it. A judgement never overwrites an outcome the structure decided, and without judgements the pending category stays pending and is reported apart from failure. Each category is compared over the conditions its own attempts run under, and the conditions are named in the row: the paired scenarios run under two conditions and the rest under one, so a selector naming a single condition would otherwise leave a whole category empty and read as "nothing to compare".

```powershell
node eval/l4/cli.mjs truncation-report --dataset C:\evaluation\dev.jsonl --prepare C:\evaluation\prepare --out C:\evaluation\truncation.json --arm truncation
node eval/l4/cli.mjs judge-input --dataset C:\evaluation\dev.jsonl --records C:\evaluation\records.jsonl --out C:\evaluation\judge-input.jsonl
node eval/l4/cli.mjs judge-input --dataset C:\evaluation\dev.jsonl --records C:\evaluation\records.jsonl --outcomes C:\evaluation\scored\outcomes.jsonl --out C:\evaluation\judge-pending.jsonl
node eval/l4/cli.mjs category-report --run C:\evaluation\scored --dataset C:\evaluation\dev.jsonl --out C:\evaluation\categories.json --left baseline:main --right truncation:main --judgments C:\evaluation\judgments.jsonl
```

`judge-input` covers the four categories the structure cannot decide — the constraint, the irrelevance, the fabrication and the stale categories — and `--categories` narrows that list. With `--outcomes` it prepares only the attempts that are still pending. `category-report` reads the judgements as records of `{item_id, arm, condition, rep, check_point_index, verdict, reason}`, where the verdict is `yes`, `no`, `unsure` or `error`; the identity and the reason are kept beside the verdict and are not part of what the model reads.

The two arms of one comparison are two `arm` values in the same records file; the categories are kept apart because the question is not whether the answers got worse but whether the answers that depend on what the summary kept got worse.

Input records carry `qid`, `arm`, `rep`, `condition`, `env`, `answer`, optional `error`, and the runner's `backgroundAudit`. Malformed identities, duplicate attempts, unknown conditions, inconsistent outcome inventories and modified outcome files are rejected. The final-question runner can export this contract directly as `record.jsonl`; concatenate such files when scoring multiple attempts.

Synthetic fixtures verify arithmetic and control flow only. Fake results are not quality evidence. Semantic judge calibration, independent dataset review and separately authorized real execution remain prerequisites for a final quality evaluation. Open answers remain `needs_judge` until an audited judgment is supplied.

When one run directory holds several scored directories, the later one supersedes the earlier: `score-fact2/` is authoritative and `score-fact/` is the superseded first round, kept for comparison only. A report names the scored directory it used.
