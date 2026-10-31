// Package validate checks a generated question set against the rules in
// eval-design.md §5.2. The structural checks (file exists, line in range,
// answerable/unanswerable agreement, duplicate id, split name) already live
// in the dataset loader; this package adds the ones that need the corpus
// content, and re-runs the structural ones so a single report covers all of
// §5.2.
//
// Every check is a rule with an id, so a report can be read rule by rule and a
// generation run can be told exactly which rules its output broke.
package validate

import (
	"fmt"
	"sort"
	"strings"

	"github.com/xiaopang456/GopherLearn/eval/internal/dataset"
)

// Severity is how serious a finding is.
type Severity int

const (
	// SeverityError fails the validation.
	SeverityError Severity = iota
	// SeverityWarning is reported but does not fail the validation.
	SeverityWarning
)

func (s Severity) String() string {
	if s == SeverityError {
		return "ERROR"
	}
	return "WARN"
}

// Rule ids. They are stable strings so a report can be diffed across runs.
const (
	RuleEvidenceNoContent = "evidence.no_content"
	RuleEvidenceImageOnly = "evidence.image_only"
	RuleEvidenceHeading   = "evidence.heading"
	RuleEvidenceBlank     = "evidence.blank"

	RuleRefusalMarker = "question.refusal_marker"
	RuleHistoryRecall = "question.history_recall"
	RuleDuplicate     = "question.duplicate"
	// RuleSimilarPair marks a pair of questions that are close but below the
	// duplicate threshold. It is a list for the reviewer, not a defect.
	RuleSimilarPair     = "question.similar_pair"
	RuleQuestionEmpty   = "question.empty"
	RuleWhyEachNeeded   = "meta.why_each_needed_missing"
	RuleNuggetMissing   = "nugget.keyword_missing"
	RuleNuggetNotRun    = "nugget.not_checked"
	RuleUnanswerableHit = "unanswerable.has_answer"
	// RuleUnanswerableSuspect marks a question whose keywords partly turn up
	// in the corpus: the corpus may still know the answer.
	RuleUnanswerableSuspect = "unanswerable.suspect"
	// RuleUnanswerableInconclusive marks a question the keyword screen cannot
	// decide either way.
	RuleUnanswerableInconclusive = "unanswerable.inconclusive"

	RuleQuotaTotal        = "quota.total"
	RuleQuotaType         = "quota.type"
	RuleQuotaUnanswerable = "quota.unanswerable_ratio"
	RuleQuotaParaphrase   = "quota.paraphrase"
	RuleQuotaRephrased    = "quota.min_rephrased_ratio"
	RuleQuotaLang         = "quota.lang"
	RuleQuotaTopic        = "quota.topic"
	RuleQuotaSplit        = "quota.split"
	RuleMetaMissing       = "meta.missing"
	// RuleMetaModelMissing marks a question that does not record the model
	// and the generation parameters it was produced with.
	RuleMetaModelMissing = "meta.model_missing"
	// RuleMetaModelMismatch marks a question set whose questions were not all
	// produced the same way.
	RuleMetaModelMismatch = "meta.model_mismatch"
)

// Finding is one problem with one question.
type Finding struct {
	Severity Severity `json:"severity"`
	Rule     string   `json:"rule"`
	ID       string   `json:"id,omitempty"`
	File     string   `json:"file,omitempty"`
	Line     int      `json:"line,omitempty"`
	Detail   string   `json:"detail"`
}

func (f Finding) String() string {
	where := f.ID
	if f.File != "" {
		where = f.File
		if f.Line > 0 {
			where = fmt.Sprintf("%s:%d", f.File, f.Line)
		}
	}
	if where == "" {
		return fmt.Sprintf("%s %s: %s", f.Severity, f.Rule, f.Detail)
	}
	return fmt.Sprintf("%s %s [%s]: %s", f.Severity, f.Rule, where, f.Detail)
}

// Report is the result of validating one question set.
type Report struct {
	Questions int            `json:"questions"`
	Findings  []Finding      `json:"findings"`
	Counts    map[string]int `json:"counts"`
}

// Errors returns only the findings that fail the validation.
func (r *Report) Errors() []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Severity == SeverityError {
			out = append(out, f)
		}
	}
	return out
}

// OK reports whether the validation passed.
func (r *Report) OK() bool { return len(r.Errors()) == 0 }

// Text renders the report for a terminal.
func (r *Report) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "questions: %d\n", r.Questions)
	if len(r.Findings) == 0 {
		b.WriteString("no findings\n")
		return b.String()
	}
	byRule := map[string]int{}
	for _, f := range r.Findings {
		byRule[f.Rule]++
	}
	rules := make([]string, 0, len(byRule))
	for rule := range byRule {
		rules = append(rules, rule)
	}
	sort.Strings(rules)
	b.WriteString("findings by rule:\n")
	for _, rule := range rules {
		fmt.Fprintf(&b, "  %-32s %d\n", rule, byRule[rule])
	}
	b.WriteString("\nfindings:\n")
	for _, f := range r.Findings {
		fmt.Fprintf(&b, "  %s\n", f.String())
	}
	return b.String()
}

// Params tunes the checks that have a threshold.
type Params struct {
	// DuplicateThreshold is the Jaccard similarity above which two questions
	// count as duplicates. 0.75 by default.
	DuplicateThreshold float64
	// SimilarPairMin is the similarity below which two questions are only
	// close, not duplicates. The pairs in [SimilarPairMin, DuplicateThreshold)
	// are listed for the reviewer. 0.6 by default.
	SimilarPairMin float64
	// NuggetWindow is how many lines around an evidence span are searched for
	// a nugget keyword. 3 by default.
	NuggetWindow int
	// UnanswerableMinDistinctive is how many distinctive keywords a corpus line
	// must carry before the line is treated as a suspect answer. 2 by default.
	UnanswerableMinDistinctive int
	// UnanswerableMaxDocFreq drops keywords that occur in more than this many
	// corpus lines: a keyword that common proves nothing. 120 by default.
	UnanswerableMaxDocFreq int
	// UnanswerableStrongTermMinDocFreq is the corpus frequency at which a
	// single ASCII identifier is enough to treat a line as the answer.
	// 60 by default.
	UnanswerableStrongTermMinDocFreq int
	// Excluded are the knowledge points the first question set must stay
	// away from. Empty means no topic is excluded.
	Excluded ExcludedTopics
}

// DefaultParams returns the parameters the plan documents.
func DefaultParams() Params {
	return Params{
		DuplicateThreshold:               0.75,
		SimilarPairMin:                   0.6,
		NuggetWindow:                     3,
		UnanswerableMinDistinctive:       2,
		UnanswerableMaxDocFreq:           120,
		UnanswerableStrongTermMinDocFreq: 60,
	}
}

// Validate runs every §5.2 check over the questions.
//
// meta holds the per-question annotations that are not part of the question
// file's schema (paraphrase class, language, required points). It may be nil,
// in which case the checks that need it report that they could not run.
func Validate(corpus *dataset.Corpus, questions []dataset.Question, meta []Meta, quota Quota, params Params) (*Report, error) {
	docs, err := loadCorpusDocuments(corpus)
	if err != nil {
		return nil, err
	}
	report := &Report{Questions: len(questions), Counts: map[string]int{}}
	byID := map[string]Meta{}
	for _, m := range meta {
		byID[m.ID] = m
	}

	for _, q := range questions {
		m := byID[q.ID]
		if m.Author != "" && m.Author != "qwen" && m.Author != "e4" {
			report.Findings = append(report.Findings, Finding{Severity: SeverityError, Rule: "meta.author_invalid", ID: q.ID, Detail: "author must be qwen or e4"})
		}
		checkQuestionText(report, q)
		checkQuestionLanguage(report, q, m)
		checkMultiQuestion(report, q)
		checkEvidence(report, q, docs)
		checkUnanswerable(report, q, docs, params)
		checkUnanswerableMetaLeak(report, q, docs)
		checkExcludedTopicNear(report, q, docs, params.Excluded)
		checkCrossDocEvidence(report, q, m)
		checkWhyEachNeeded(report, q, m)
		if len(meta) > 0 {
			checkNuggets(report, q, docs, byID, params)
		}
	}
	if len(meta) == 0 {
		// The keyword check needs the sidecar. Without it the rule cannot run,
		// and a report that stays silent about that looks like a clean run.
		report.Findings = append(report.Findings, Finding{
			Severity: SeverityWarning, Rule: RuleNuggetNotRun,
			Detail: "no sidecar file was given, so the required-point keyword check did not run"})
	}
	checkDuplicates(report, questions, params)
	checkMetaConsistency(report, questions, meta)
	if !quota.Empty() {
		checkQuotas(report, questions, meta, quota)
	}

	for _, f := range report.Findings {
		report.Counts[f.Rule]++
	}
	return report, nil
}

func loadCorpusDocuments(corpus *dataset.Corpus) (map[string]*Document, error) {
	paths := map[string]string{}
	for _, doc := range corpus.Documents {
		paths[doc.File] = doc.Path
	}
	return LoadDocuments(paths)
}

// checkQuestionText applies the rules about the question string itself.
func checkQuestionText(report *Report, q dataset.Question) {
	text := strings.TrimSpace(q.Question)
	if text == "" {
		report.Findings = append(report.Findings, Finding{
			Severity: SeverityError, Rule: RuleQuestionEmpty, ID: q.ID,
			Detail: "question text is empty"})
		return
	}
	// The product refuses these questions outright, so a question that contains
	// one of the markers can never be answered by a document.
	normalized := strings.ToLower(strings.TrimSpace(q.Question))
	for _, marker := range RefusalMarkers {
		if strings.Contains(normalized, marker) {
			report.Findings = append(report.Findings, Finding{
				Severity: SeverityError, Rule: RuleRefusalMarker, ID: q.ID,
				Detail: fmt.Sprintf("question contains %q, which the product refuses (requiresExactHistoryEvidence)", marker)})
			break
		}
	}
	for _, marker := range HistoryRecallMarkers {
		if strings.Contains(normalized, marker) {
			report.Findings = append(report.Findings, Finding{
				Severity: SeverityWarning, Rule: RuleHistoryRecall, ID: q.ID,
				Detail: fmt.Sprintf("question contains %q, which the product routes to memory recall rather than to a document search", marker)})
			break
		}
	}
}

// checkMetaConsistency checks that the questions of one set were produced the
// same way.
//
// Every question records the model and the generation parameters it came
// from. A set that mixes models or parameters cannot be reproduced and its
// questions cannot be compared with each other, so that is an error. A
// question that records neither is a warning: the run is not reproducible,
// but nothing in it contradicts anything else.
func checkMetaConsistency(report *Report, questions []dataset.Question, meta []Meta) {
	if len(meta) == 0 {
		return
	}
	byID := map[string]Meta{}
	for _, m := range meta {
		byID[m.ID] = m
	}
	// The fingerprint is compared **within** a batch kind. The kinds need
	// different generation settings (the multi-evidence and cross-document
	// batches think longer), so two kinds with different settings are not a
	// defect; two kinds that disagree with themselves are.
	fingerprints := map[string]map[string]int{}
	missing := 0
	for _, q := range questions {
		m, ok := byID[q.ID]
		if !ok {
			continue
		}
		// A question that records the model but no parameters is missing
		// them, not "produced in a different way": counting it as a second
		// combination would report a difference that is not there.
		if m.Author == "e4" {
			continue
		}
		if m.Model == "" || m.Params.Empty() {
			missing++
			continue
		}
		kind := m.Params.BatchKind
		if kind == "" {
			kind = "(unspecified)"
		}
		if fingerprints[kind] == nil {
			fingerprints[kind] = map[string]int{}
		}
		fingerprints[kind][m.Params.Fingerprint(m.Model)]++
	}
	if missing > 0 {
		report.Findings = append(report.Findings, Finding{
			Severity: SeverityWarning, Rule: RuleMetaModelMissing,
			Detail: fmt.Sprintf("%d question(s) do not record the generation model and parameters (the model is missing, or the parameters are); the run cannot be reproduced",
				missing)})
	}
	kinds := make([]string, 0, len(fingerprints))
	for kind := range fingerprints {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		if len(fingerprints[kind]) <= 1 {
			continue
		}
		combinations := make([]string, 0, len(fingerprints[kind]))
		for fingerprint := range fingerprints[kind] {
			combinations = append(combinations, fingerprint)
		}
		sort.Strings(combinations)
		report.Findings = append(report.Findings, Finding{
			Severity: SeverityError, Rule: RuleMetaModelMismatch,
			Detail: fmt.Sprintf("the questions of batch kind %q were not all produced the same way (%d different model/parameter combinations): %s",
				kind, len(combinations), strings.Join(combinations, "; "))})
	}
}

// checkEvidence applies the rules about the lines an evidence span covers.
func checkEvidence(report *Report, q dataset.Question, docs map[string]*Document) {
	for setIndex, set := range q.Evidence {
		for spanIndex, span := range set {
			doc, ok := docs[span.File]
			if !ok {
				continue // the dataset loader already reports unknown files
			}
			content, images, headings, blanks := 0, 0, 0, 0
			var rules, fences int
			var imageLines, headingLines, blankLines []int
			for n := span.Start; n <= span.End; n++ {
				switch doc.Kind(n) {
				case LineContent:
					content++
				case LineImage:
					images++
					imageLines = append(imageLines, n)
				case LineHeading:
					headings++
					headingLines = append(headingLines, n)
				case LineBlank:
					blanks++
					blankLines = append(blankLines, n)
				case LineRule:
					rules++
				case LineFence:
					fences++
				}
			}
			where := fmt.Sprintf("evidence_sets[%d][%d]", setIndex, spanIndex)
			if content == 0 {
				report.Findings = append(report.Findings, Finding{
					Severity: SeverityError, Rule: RuleEvidenceNoContent, ID: q.ID, File: span.File, Line: span.Start,
					Detail: fmt.Sprintf("%s covers lines %d-%d, which contain no body text (%d image, %d heading, %d blank, %d rule, %d fence)",
						where, span.Start, span.End, images, headings, blanks, rules, fences)})
			}
			if images > 0 {
				report.Findings = append(report.Findings, Finding{
					Severity: SeverityWarning, Rule: RuleEvidenceImageOnly, ID: q.ID, File: span.File, Line: imageLines[0],
					Detail: fmt.Sprintf("%s covers line(s) %v that carry nothing but an image link; trim them so the range is only body text (an answer that only exists in the picture cannot be read)",
						where, imageLines)})
			}
			if headings > 0 {
				report.Findings = append(report.Findings, Finding{
					Severity: SeverityWarning, Rule: RuleEvidenceHeading, ID: q.ID, File: span.File, Line: headingLines[0],
					Detail: fmt.Sprintf("%s covers heading line(s) %v; a heading must not be part of the required range (it would tie coverage to chunking)",
						where, headingLines)})
			}
			if blanks > 0 {
				report.Findings = append(report.Findings, Finding{
					Severity: SeverityWarning, Rule: RuleEvidenceBlank, ID: q.ID, File: span.File, Line: blankLines[0],
					Detail: fmt.Sprintf("%s covers blank line(s) %v; trim them so the range is only body text", where, blankLines)})
			}
		}
	}
}

// checkNuggets checks that each required point's keywords really appear near
// the evidence. It is a loose check meant to catch an obviously wrong range,
// not a semantic judgement.
func checkNuggets(report *Report, q dataset.Question, docs map[string]*Document, byID map[string]Meta, params Params) {
	m, ok := byID[q.ID]
	if !ok {
		report.Findings = append(report.Findings, Finding{
			Severity: SeverityError, Rule: RuleMetaMissing, ID: q.ID,
			Detail: "no annotation record in the sidecar file"})
		return
	}
	if len(m.Points) == 0 {
		return
	}
	window := params.NuggetWindow
	if window <= 0 {
		window = 3
	}
	for _, point := range m.Points {
		keywords := []string{}
		for _, keyword := range point.Keywords {
			if strings.TrimSpace(keyword) != "" {
				keywords = append(keywords, keyword)
			}
		}
		if len(keywords) == 0 {
			continue
		}
		for i, set := range q.Evidence {
			found := false
			single := q
			single.Evidence = []dataset.EvidenceSet{set}
			for _, keyword := range keywords {
				if keywordNearEvidence(single, keyword, docs, window) {
					found = true
					break
				}
			}
			if !found {
				report.Findings = append(report.Findings, Finding{Severity: SeverityError, Rule: RuleNuggetMissing, ID: q.ID,
					Detail: fmt.Sprintf("required point %q: no alternative keyword %q found near evidence set %d", point.Text, keywords, i)})
			}
		}
	}
}

// keywordNearEvidence reports whether keyword appears in the evidence spans or
// within window lines around them.
func keywordNearEvidence(q dataset.Question, keyword string, docs map[string]*Document, window int) bool {
	needle := strings.ToLower(keyword)
	for _, set := range q.Evidence {
		for _, span := range set {
			doc, ok := docs[span.File]
			if !ok {
				continue
			}
			from := span.Start - window
			if from < 1 {
				from = 1
			}
			to := span.End + window
			if to > len(doc.Lines) {
				to = len(doc.Lines)
			}
			for n := from; n <= to; n++ {
				if strings.Contains(strings.ToLower(doc.Line(n)), needle) {
					return true
				}
			}
		}
	}
	return false
}
