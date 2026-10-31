package validate

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/xiaopang456/GopherLearn/eval/internal/dataset"
)

// Rule ids of this file.
const (
	// RuleMultiQuestion marks a multi-evidence question whose text asks
	// several small questions at once. A multi-evidence question is one
	// question that needs two places to answer; a question that strings
	// three small questions together with question marks is not that.
	RuleMultiQuestion = "question.multi_question"
	// RuleUnanswerableMetaLeak marks an unanswerable question whose text
	// names a file, a path or a section of the corpus. A real user does not
	// know how the corpus is cut up.
	RuleUnanswerableMetaLeak = "unanswerable.meta_leak"
	// RuleExcludedTopicNear marks a question that sits too close to a
	// knowledge point the plan excludes from the first question set.
	RuleExcludedTopicNear = "question.excluded_topic_near"
)

// ExcludedTopic is a knowledge point the first question set must not cover.
//
// The file and the line say where in the corpus it sits, so a question whose
// evidence lands next to it is reported. The terms catch the same topic
// asked about from a different angle: a question naming every term in the
// list is about the topic, wherever its evidence points.
type ExcludedTopic struct {
	File string `json:"file"`
	// Line is the line the knowledge point is on.
	Line int `json:"line"`
	// Radius is how many lines around it still count as "next to it".
	Radius int `json:"radius"`
	// Terms are the words of the topic. A question naming all of them is
	// about the topic even if its evidence is far away.
	Terms []string `json:"terms"`
	// Name is what to call the topic in the report.
	Name string `json:"name"`
}

// ExcludedTopics is the list of knowledge points to stay away from.
type ExcludedTopics []ExcludedTopic

// LoadExcludedTopics reads the excluded knowledge points from a JSON file.
func LoadExcludedTopics(path string) (ExcludedTopics, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var topics ExcludedTopics
	if err := json.Unmarshal(raw, &topics); err != nil {
		return nil, fmt.Errorf("parse excluded topics %s: %w", path, err)
	}
	return topics, nil
}

// checkMultiQuestion marks multi-evidence questions that string several
// small questions together.
//
// "If T1 holds an X lock on row r, what happens when T2 asks for any lock on
// that row? What level are the intention locks InnoDB uses, and what do they
// indicate?" is two questions glued together with a question mark. The answer
// is not one thing, and scoring it against one evidence set is muddy.
func checkMultiQuestion(report *Report, q dataset.Question) {
	if q.Type != "multi_evidence" {
		return
	}
	count := strings.Count(q.Question, "？") + strings.Count(q.Question, "?")
	if count < 2 {
		return
	}
	report.Findings = append(report.Findings, Finding{
		Severity: SeverityWarning, Rule: RuleMultiQuestion, ID: q.ID,
		Detail: fmt.Sprintf("the question has %d question marks; a multi-evidence question is ONE question that needs two places to answer, not several small questions joined together",
			count)})
}

// mdFilePattern matches a corpus file name in a question text.
var mdFilePattern = regexp.MustCompile(`[\w\-\.]+\.md`)

// checkUnanswerableMetaLeak marks unanswerable questions that name the
// corpus itself.
//
// "In the Content, Raw and line-number section of notes/gopherai-rag.md,
// paragraphs usually have Content == Raw; do files with a BOM still make the
// two equal?" tells the reader which file and which section to look at. A
// real user has no idea how the corpus is cut up, so such a question is not a
// realistic one, and it also hands the answerer the place to look.
func checkUnanswerableMetaLeak(report *Report, q dataset.Question, docs map[string]*Document) {
	if q.Answerable {
		return
	}
	text := q.Question
	var reasons []string
	if mdFilePattern.MatchString(text) {
		reasons = append(reasons, "it names a corpus file (like notes/xxx.md)")
	}
	if name := mentionedCorpusName(text, docs); name != "" {
		reasons = append(reasons, fmt.Sprintf("it names the corpus document %q", name))
	}
	if len(reasons) == 0 {
		return
	}
	report.Findings = append(report.Findings, Finding{
		Severity: SeverityWarning, Rule: RuleUnanswerableMetaLeak, ID: q.ID,
		Detail: fmt.Sprintf("%s; an unanswerable question must read like a real user asking, with no file name, no path and no section title in it",
			strings.Join(reasons, " and "))})
}

// mentionedCorpusName returns the name of a corpus document the text names.
//
// Only the document names long enough to be distinctive are looked for: a
// two-letter stem would match half the questions.
func mentionedCorpusName(text string, docs map[string]*Document) string {
	names := make([]string, 0, len(docs))
	for file := range docs {
		base := file
		if index := strings.LastIndex(base, "/"); index >= 0 {
			base = base[index+1:]
		}
		base = strings.TrimSuffix(base, ".md")
		if len([]rune(base)) < 5 {
			continue
		}
		names = append(names, base)
	}
	sort.Strings(names)
	for _, name := range names {
		if strings.Contains(text, name) {
			return name
		}
	}
	return ""
}

// checkExcludedTopicNear marks questions that sit next to an excluded
// knowledge point.
//
// Two ways to get there: the evidence lands within the excluded line's
// neighbourhood, or the question names every word of the topic. The second
// one catches the questions that ask about the same thing from a different
// angle and whose evidence points somewhere else.
func checkExcludedTopicNear(report *Report, q dataset.Question, docs map[string]*Document, topics ExcludedTopics) {
	for _, topic := range topics {
		radius := topic.Radius
		if radius <= 0 {
			radius = 10
		}
		for _, set := range q.Evidence {
			for _, span := range set {
				if span.File != topic.File {
					continue
				}
				if span.Start <= topic.Line+radius && topic.Line-radius <= span.End {
					report.Findings = append(report.Findings, Finding{
						Severity: SeverityWarning, Rule: RuleExcludedTopicNear, ID: q.ID,
						File: span.File, Line: span.Start,
						Detail: fmt.Sprintf("the evidence sits within %d lines of %s (%s:%d), which the plan excludes from the first question set",
							radius, topicLabel(topic), topic.File, topic.Line)})
				}
			}
		}
		if topicTermsAllPresent(q.Question, topic.Terms) {
			report.Findings = append(report.Findings, Finding{
				Severity: SeverityWarning, Rule: RuleExcludedTopicNear, ID: q.ID,
				Detail: fmt.Sprintf("the question names every word of %s (%s), which the plan excludes from the first question set",
					topicLabel(topic), strings.Join(topic.Terms, " and "))})
		}
	}
}

// topicTermsAllPresent reports whether the text names every term of the topic.
func topicTermsAllPresent(text string, terms []string) bool {
	if len(terms) == 0 {
		return false
	}
	for _, term := range terms {
		if term == "" {
			continue
		}
		if !strings.Contains(text, term) {
			return false
		}
	}
	return true
}

func topicLabel(topic ExcludedTopic) string {
	if topic.Name != "" {
		return topic.Name
	}
	return "the excluded topic"
}
