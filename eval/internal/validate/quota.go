package validate

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/xiaopang456/GopherLearn/eval/internal/dataset"
)

// Meta is one line of the sidecar annotation file.
//
// The question file's schema is fixed (the dataset loader rejects unknown
// fields), so the annotations that the design asks for but the schema has no
// room for live next to it, keyed by question id.
type Meta struct {
	ID         string `json:"id"`
	Paraphrase string `json:"paraphrase,omitempty"`
	Lang       string `json:"lang,omitempty"`
	// Layer is the measurement layer the question belongs to (L1, L2, ...).
	// The question file's schema has no room for it, so it lives here.
	// The first question set is L1 only, but the split already separates by
	// layer, so L4 can be added later without touching the split.
	Layer  string    `json:"layer,omitempty"`
	Points []Point   `json:"points,omitempty"`
	Batch  string    `json:"batch,omitempty"`
	Model  string    `json:"model,omitempty"`
	Params GenParams `json:"params,omitempty"`
	Note   string    `json:"note,omitempty"`
}

// GenParams are the generation parameters a question was produced with.
//
// They are recorded per question, next to the model name, so a generation run
// can be reproduced and so a question set that was produced under different
// conditions cannot slip through unnoticed.
type GenParams struct {
	Temperature float64 `json:"temperature,omitempty"`
	TopP        float64 `json:"top_p,omitempty"`
	MaxTokens   int     `json:"max_tokens,omitempty"`
	Seed        int64   `json:"seed,omitempty"`
}

// Fingerprint is the string the consistency check compares: the model name
// and the parameters in a fixed order.
func (p GenParams) Fingerprint(model string) string {
	return fmt.Sprintf("%s|temperature=%.2f|top_p=%.2f|max_tokens=%d|seed=%d",
		model, p.Temperature, p.TopP, p.MaxTokens, p.Seed)
}

// Point is one required point of an answer, with the keywords that must be
// findable near the evidence.
type Point struct {
	Text     string   `json:"text"`
	Keywords []string `json:"keywords,omitempty"`
}

// Quota is the plan's target shape of the question set.
type Quota struct {
	Name  string `json:"name,omitempty"`
	Total struct {
		Min int `json:"min"`
		Max int `json:"max"`
	} `json:"total"`
	// Types are the expected counts per question type.
	Types map[string]int `json:"types,omitempty"`
	// UnanswerableRatio bounds the share of unanswerable questions.
	UnanswerableRatio struct {
		Min float64 `json:"min"`
		Max float64 `json:"max"`
	} `json:"unanswerable_ratio"`
	// Paraphrase are the expected counts per paraphrase class.
	Paraphrase map[string]int `json:"paraphrase,omitempty"`
	// MinRephrasedRatio is the floor for the share of rephrased questions.
	// The design requires at least one third.
	MinRephrasedRatio float64 `json:"min_rephrased_ratio"`
	// Lang are the expected counts per language class.
	Lang map[string]int `json:"lang,omitempty"`
	// PerTopic are the expected counts per topic.
	PerTopic map[string]int `json:"per_topic,omitempty"`
	// PerTopicMin is the floor per topic. It never pushes a topic above its
	// own target: a topic planned with zero questions needs zero questions.
	PerTopicMin int `json:"per_topic_min"`
	// Split are the expected counts per split.
	Split map[string]int `json:"split,omitempty"`
	// SplitTolerance is how far a split count may drift from the target.
	// Without it, a split that is off by one would be reported like a split
	// that is off by fifty.
	SplitTolerance int `json:"split_tolerance"`
}

// Empty reports whether the quota asks for nothing at all.
//
// A run without a quota file must not report every observed type, topic and
// split as "not in the plan": the quota checks are about the plan, so without
// a plan there is nothing to check.
func (q Quota) Empty() bool {
	return len(q.Types) == 0 &&
		len(q.Paraphrase) == 0 &&
		len(q.Lang) == 0 &&
		len(q.PerTopic) == 0 &&
		len(q.Split) == 0 &&
		q.Total.Min == 0 && q.Total.Max == 0 &&
		q.UnanswerableRatio.Min == 0 && q.UnanswerableRatio.Max == 0 &&
		q.MinRephrasedRatio == 0
}

// LoadQuota reads a quota file.
func LoadQuota(path string) (Quota, error) {
	var quota Quota
	raw, err := os.ReadFile(path)
	if err != nil {
		return quota, err
	}
	if err := json.Unmarshal(raw, &quota); err != nil {
		return quota, fmt.Errorf("parse quota %s: %w", path, err)
	}
	return quota, nil
}

// LoadMeta reads the sidecar annotation file.
func LoadMeta(path string) ([]Meta, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []Meta
	for i, line := range splitJSONLines(string(raw)) {
		var m Meta
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			return nil, fmt.Errorf("parse meta line %d: %w", i+1, err)
		}
		out = append(out, m)
	}
	return out, nil
}

func splitJSONLines(text string) []string {
	var out []string
	for _, line := range splitLines(text) {
		if trimmed := trimSpace(line); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && isSpace(s[start]) {
		start++
	}
	for end > start && isSpace(s[end-1]) {
		end--
	}
	return s[start:end]
}

func isSpace(b byte) bool {
	switch b {
	case ' ', '\t', '\r', '\n', '\v', '\f':
		return true
	}
	return false
}

// checkQuotas verifies that the question set has the shape the plan asks for.
//
// Counts are checked against the quota's targets. A count below a stated floor
// is an error; a count that differs from the target without breaking a floor
// is a warning, so the report shows drift without failing a run that is merely
// uneven.
func checkQuotas(report *Report, questions []dataset.Question, meta []Meta, quota Quota) {
	byID := map[string]Meta{}
	for _, m := range meta {
		byID[m.ID] = m
	}

	if quota.Total.Min > 0 && len(questions) < quota.Total.Min {
		report.Findings = append(report.Findings, Finding{
			Severity: SeverityError, Rule: RuleQuotaTotal,
			Detail: fmt.Sprintf("have %d questions, want at least %d", len(questions), quota.Total.Min)})
	}
	if quota.Total.Max > 0 && len(questions) > quota.Total.Max {
		report.Findings = append(report.Findings, Finding{
			Severity: SeverityError, Rule: RuleQuotaTotal,
			Detail: fmt.Sprintf("have %d questions, want at most %d", len(questions), quota.Total.Max)})
	}

	typeCounts := map[string]int{}
	paraphraseCounts := map[string]int{}
	langCounts := map[string]int{}
	topicCounts := map[string]int{}
	splitCounts := map[string]int{}
	unanswerable := 0
	for _, q := range questions {
		typeCounts[q.Type]++
		topicCounts[q.Topic]++
		splitCounts[q.Split]++
		if !q.Answerable {
			unanswerable++
		}
		if m, ok := byID[q.ID]; ok {
			paraphraseCounts[m.Paraphrase]++
			langCounts[m.Lang]++
		}
	}

	checkCount(report, RuleQuotaType, "type", typeCounts, quota.Types)
	checkCount(report, RuleQuotaParaphrase, "paraphrase", paraphraseCounts, quota.Paraphrase)
	checkCount(report, RuleQuotaLang, "lang", langCounts, quota.Lang)
	checkCountWithTolerance(report, RuleQuotaSplit, "split", splitCounts, quota.Split, quota.SplitTolerance)

	if quota.UnanswerableRatio.Max > 0 && len(questions) > 0 {
		ratio := float64(unanswerable) / float64(len(questions))
		if ratio > quota.UnanswerableRatio.Max {
			report.Findings = append(report.Findings, Finding{
				Severity: SeverityError, Rule: RuleQuotaUnanswerable,
				Detail: fmt.Sprintf("unanswerable share is %.1f%%, want at most %.1f%%",
					ratio*100, quota.UnanswerableRatio.Max*100)})
		}
		if quota.UnanswerableRatio.Min > 0 && ratio < quota.UnanswerableRatio.Min {
			report.Findings = append(report.Findings, Finding{
				Severity: SeverityError, Rule: RuleQuotaUnanswerable,
				Detail: fmt.Sprintf("unanswerable share is %.1f%%, want at least %.1f%%",
					ratio*100, quota.UnanswerableRatio.Min*100)})
		}
	}

	if quota.MinRephrasedRatio > 0 && len(questions) > 0 {
		ratio := float64(paraphraseCounts["rephrased"]) / float64(len(questions))
		if ratio+1e-9 < quota.MinRephrasedRatio {
			report.Findings = append(report.Findings, Finding{
				Severity: SeverityError, Rule: RuleQuotaRephrased,
				Detail: fmt.Sprintf("rephrased share is %.1f%%, want at least %.1f%%",
					ratio*100, quota.MinRephrasedRatio*100)})
		}
	}

	// Topics: every topic the plan names must be present, with at least the
	// stated floor, and the ones with an exact target should match it.
	//
	// The floor never exceeds the topic's own target: a topic the plan gives
	// zero questions cannot also be required to have some. Otherwise a plan
	// that gives a document a smaller share (because it has too little
	// material) could never be satisfied.
	for _, topic := range sortedKeys(quota.PerTopic) {
		got := topicCounts[topic]
		want := quota.PerTopic[topic]
		floor := quota.PerTopicMin
		if want < floor {
			floor = want
		}
		if floor > 0 && got < floor {
			report.Findings = append(report.Findings, Finding{
				Severity: SeverityError, Rule: RuleQuotaTopic,
				Detail: fmt.Sprintf("topic %q has %d questions, want at least %d", topic, got, floor)})
			continue
		}
		if got != want {
			report.Findings = append(report.Findings, Finding{
				Severity: SeverityWarning, Rule: RuleQuotaTopic,
				Detail: fmt.Sprintf("topic %q has %d questions, plan says %d", topic, got, want)})
		}
	}
	// A topic that is not in the plan is a deviation too.
	for _, topic := range sortedKeys(topicCounts) {
		if _, ok := quota.PerTopic[topic]; ok {
			continue
		}
		report.Findings = append(report.Findings, Finding{
			Severity: SeverityWarning, Rule: RuleQuotaTopic,
			Detail: fmt.Sprintf("topic %q is not in the plan (has %d questions)", topic, topicCounts[topic])})
	}
}

func checkCount(report *Report, rule, label string, got, want map[string]int) {
	for _, key := range sortedKeys(want) {
		if got[key] != want[key] {
			report.Findings = append(report.Findings, Finding{
				Severity: SeverityWarning, Rule: rule,
				Detail: fmt.Sprintf("%s %q has %d questions, plan says %d", label, key, got[key], want[key])})
		}
	}
	for _, key := range sortedKeys(got) {
		if _, ok := want[key]; ok {
			continue
		}
		report.Findings = append(report.Findings, Finding{
			Severity: SeverityWarning, Rule: rule,
			Detail: fmt.Sprintf("%s %q has %d questions and is not in the plan", label, key, got[key])})
	}
}

// checkCountWithTolerance is checkCount with a slack: a count that drifts from
// the target by at most tolerance is accepted, a larger drift is reported.
func checkCountWithTolerance(report *Report, rule, label string, got, want map[string]int, tolerance int) {
	for _, key := range sortedKeys(want) {
		diff := got[key] - want[key]
		if diff < 0 {
			diff = -diff
		}
		if diff > tolerance {
			report.Findings = append(report.Findings, Finding{
				Severity: SeverityWarning, Rule: rule,
				Detail: fmt.Sprintf("%s %q has %d questions, plan says %d (off by %d, tolerance %d)",
					label, key, got[key], want[key], diff, tolerance)})
		}
	}
	for _, key := range sortedKeys(got) {
		if _, ok := want[key]; ok {
			continue
		}
		report.Findings = append(report.Findings, Finding{
			Severity: SeverityWarning, Rule: rule,
			Detail: fmt.Sprintf("%s %q has %d questions and is not in the plan", label, key, got[key])})
	}
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
