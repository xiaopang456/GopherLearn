// Command qprep prepares the question generation: it cuts the corpus into the
// passages the questions will be written from, and forecasts how much model
// work the generation needs.
//
//	qprep plan -corpus corpus.json [-out passages.jsonl] [-per-call 4] [-prompt-tokens 1600] [-question-tokens 320]
//
// It never calls a model: it only reads the corpus and prints a forecast.
//
// The passages are the product's own chunks (rag.ParseMarkdown plus the
// evaluation's chunker configuration), so a passage's line range is exactly
// the range L1 measures coverage against. A question is therefore written from
// one chunk and may cite any body line inside it.
//
// A chunk that holds nothing but an image or a blank line is dropped: no
// question can be answered from it in text.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"

	"GopherAI/common/rag"

	"github.com/xiaopang456/GopherLearn/eval/internal/dataset"
	"github.com/xiaopang456/GopherLearn/eval/internal/pathguard"
	"github.com/xiaopang456/GopherLearn/eval/internal/validate"
)

const usage = `usage: qprep plan -corpus <file> [flags]

flags:
  -corpus <file>         corpus manifest (required)
  -quota <file>          quota file; the passages follow the per-topic question
                         quota instead of being spread evenly (optional)
  -out <file>            write the passages as JSONL (optional)
  -per-call <n>          passages per generation call (default 4)
  -ask-per-call <n>      questions asked for per call (default 3)
  -yield-rate <f>        share of asked-for questions the model returns (default 0.7)
  -prompt-tokens <n>     tokens of instructions and format spec per call (default 1600)
  -question-tokens <n>   tokens per generated question, JSON included (default 320)
  -max-tokens <n>        longest passage to use (default 600)
  -min-tokens <n>        shortest passage to use (default 25)
  -questions <n>         target number of questions (default 250)
  -mix                   allow one generation call to span two documents
`

// Line is one numbered source line of a passage.
type Line struct {
	N    int    `json:"n"`
	Text string `json:"text"`
}

// Passage is one candidate question source: a chunk of a corpus document.
type Passage struct {
	Doc       string   `json:"doc"`
	Chunk     int      `json:"chunk"`
	Type      string   `json:"type"`
	StartLine int      `json:"start"`
	EndLine   int      `json:"end"`
	TitlePath []string `json:"title_path,omitempty"`
	Tokens    int      `json:"tokens"`
	Lines     []Line   `json:"lines"`
	Text      string   `json:"text"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "plan" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	flags := flag.NewFlagSet("qprep plan", flag.ContinueOnError)
	flags.SetOutput(stderr)
	corpusPath := flags.String("corpus", "", "corpus manifest")
	quotaPath := flags.String("quota", "", "quota file")
	outPath := flags.String("out", "", "passages output")
	perCall := flags.Int("per-call", 4, "passages per call")
	askPerCall := flags.Int("ask-per-call", 3, "questions asked for per call")
	yieldRate := flags.Float64("yield-rate", 0.7, "share of asked questions returned")
	promptTokens := flags.Int("prompt-tokens", 1600, "instruction tokens per call")
	questionTokens := flags.Int("question-tokens", 320, "tokens per question")
	minTokens := flags.Int("min-tokens", 25, "shortest passage")
	maxTokens := flags.Int("max-tokens", 600, "longest passage")
	target := flags.Int("questions", 250, "target number of questions")
	mix := flags.Bool("mix", false, "allow one generation call to span two documents")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if *corpusPath == "" {
		fmt.Fprintln(stderr, "qprep plan: -corpus is required")
		return 2
	}
	if err := pathguard.Check("corpus", *corpusPath); err != nil {
		fmt.Fprintf(stderr, "qprep plan: %v\n", err)
		return 2
	}
	corpus, err := dataset.LoadCorpus(*corpusPath)
	if err != nil {
		fmt.Fprintf(stderr, "qprep plan: %v\n", err)
		return 2
	}
	chunker, err := rag.NewChunker(rag.DefaultChunkerConfig())
	if err != nil {
		fmt.Fprintf(stderr, "qprep plan: %v\n", err)
		return 2
	}

	var passages []Passage
	perDoc := map[string]int{}
	skipped := map[string]int{}
	for _, doc := range corpus.Documents {
		raw, err := os.ReadFile(doc.Path)
		if err != nil {
			fmt.Fprintf(stderr, "qprep plan: %v\n", err)
			return 2
		}
		blocks := rag.ParseMarkdown(string(raw))
		chunks, err := chunker.Chunk(rag.ChunkInput{
			DocID: doc.File, DocVersion: 1, SourceFile: doc.File, Blocks: blocks,
		})
		if err != nil {
			fmt.Fprintf(stderr, "qprep plan: chunk %s: %v\n", doc.File, err)
			return 2
		}
		source := splitLines(string(raw))
		kinds := validate.ClassifyLines(source)
		for i, chunk := range chunks {
			if chunk.TokenEstimate < *minTokens || chunk.TokenEstimate > *maxTokens {
				skipped[doc.File]++
				continue
			}
			numbered, ok := numberLines(source, chunk.StartLine, chunk.EndLine)
			if !ok {
				skipped[doc.File]++
				continue
			}
			if !hasBodyText(numbered, kinds) {
				skipped[doc.File]++
				continue
			}
			passages = append(passages, Passage{
				Doc: doc.File, Chunk: i, Type: string(chunk.Type),
				StartLine: chunk.StartLine, EndLine: chunk.EndLine,
				TitlePath: chunk.TitlePath, Tokens: chunk.TokenEstimate,
				Lines: numbered, Text: numberedText(numbered),
			})
			perDoc[doc.File]++
		}
	}
	sort.Slice(passages, func(i, j int) bool {
		if passages[i].Doc != passages[j].Doc {
			return passages[i].Doc < passages[j].Doc
		}
		return passages[i].StartLine < passages[j].StartLine
	})

	if *outPath != "" {
		if err := pathguard.Check("out", *outPath); err != nil {
			fmt.Fprintf(stderr, "qprep plan: %v\n", err)
			return 2
		}
		file, err := os.Create(*outPath)
		if err != nil {
			fmt.Fprintf(stderr, "qprep plan: %v\n", err)
			return 2
		}
		encoder := json.NewEncoder(file)
		encoder.SetEscapeHTML(false)
		for _, p := range passages {
			if err := encoder.Encode(p); err != nil {
				fmt.Fprintf(stderr, "qprep plan: %v\n", err)
				return 2
			}
		}
		if err := file.Close(); err != nil {
			fmt.Fprintf(stderr, "qprep plan: %v\n", err)
			return 2
		}
	}

	batches := 0
	contextTokens := 0
	for _, doc := range corpus.Documents {
		count := perDoc[doc.File]
		if count == 0 {
			continue
		}
		batches += (count + *perCall - 1) / *perCall
	}
	for _, p := range passages {
		contextTokens += rag.EstimateTokens(p.Text)
	}
	calls := batches
	inputTokens := contextTokens + calls**promptTokens
	questionsPerCall := float64(*askPerCall) * *yieldRate
	outputTokens := int(math.Ceil(float64(calls)*questionsPerCall)) * *questionTokens
	callsForTarget := int(math.Ceil(float64(*target) / questionsPerCall))
	passagesForTarget := callsForTarget * *perCall
	if passagesForTarget > len(passages) {
		passagesForTarget = len(passages)
	}
	// One passage yields questionsPerPassage questions on average.
	questionsPerPassage := questionsPerCall / float64(*perCall)

	byDoc := map[string][]int{}
	for i, p := range passages {
		byDoc[p.Doc] = append(byDoc[p.Doc], i)
	}
	docs := make([]string, 0, len(byDoc))
	for d := range byDoc {
		docs = append(docs, d)
	}
	sort.Strings(docs)

	// The passages offered are taken per document, so a big document does not
	// take all the calls and a small one is not left out.
	//
	// With a quota file the share of every document follows its question
	// quota: the eight notes carry most of the questions and the 21 official
	// documents share the rest. Spreading the passages evenly over the
	// documents instead would hand the 21 small documents three quarters of
	// the budget while the plan asks them for two fifths of the questions.
	var quota validate.Quota
	if *quotaPath != "" {
		loaded, err := validate.LoadQuota(*quotaPath)
		if err != nil {
			fmt.Fprintf(stderr, "qprep plan: %v\n", err)
			return 2
		}
		quota = loaded
	}
	take := map[string]int{}
	switch {
	case !quota.Empty() && len(quota.PerTopic) > 0:
		for _, d := range docs {
			want := quota.PerTopic[d]
			need := int(math.Ceil(float64(want) / questionsPerPassage))
			if need > len(byDoc[d]) {
				need = len(byDoc[d])
			}
			if want == 0 && need == 0 && len(byDoc[d]) > 0 {
				// A document the plan gives no questions to still keeps one
				// passage: a call that spans two documents can use it as the
				// companion of another document's passages.
				need = 1
			}
			take[d] = need
		}
	default:
		each := passagesForTarget / len(docs)
		extra := passagesForTarget % len(docs)
		for i, d := range docs {
			n := each
			if i < extra {
				n++
			}
			if n > len(byDoc[d]) {
				n = len(byDoc[d])
			}
			take[d] = n
		}
	}

	selected := 0
	contextForTarget := 0
	for _, d := range docs {
		selected += take[d]
		for _, idx := range byDoc[d][:take[d]] {
			contextForTarget += rag.EstimateTokens(passages[idx].Text)
		}
	}

	// The calls are packed per document. With -mix a call may hold the
	// leftovers of one document plus passages of the next, so a document with
	// three passages does not waste a whole call.
	selectedCalls, mixedCalls := packCalls(take, docs, *perCall, *mix)

	fmt.Fprintf(stdout, "documents: %d\n", len(corpus.Documents))
	fmt.Fprintf(stdout, "candidate passages (product chunks): %d\n", len(passages))
	fmt.Fprintf(stdout, "  skipped (too small, too large, or no body text): %d\n", sumMap(skipped))
	fmt.Fprintln(stdout, "passages per document:")
	for _, doc := range corpus.Documents {
		fmt.Fprintf(stdout, "  %-46s %5d\n", doc.File, perDoc[doc.File])
	}
	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "generation calls if every passage is offered: %d  (passages per call %d, calls never mix documents)\n", calls, *perCall)
	fmt.Fprintf(stdout, "  context tokens:      %10d\n", contextTokens)
	fmt.Fprintf(stdout, "  instruction tokens:  %10d  (%d per call)\n", calls**promptTokens, *promptTokens)
	fmt.Fprintf(stdout, "  input tokens total:  %10d\n", inputTokens)
	fmt.Fprintf(stdout, "  output tokens:       %10d  (%d per question, %.1f questions per call)\n",
		outputTokens, *questionTokens, questionsPerCall)
	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "questions per call: ask %d, expect %.1f after the %.0f%% yield\n",
		*askPerCall, questionsPerCall, *yieldRate*100)
	fmt.Fprintf(stdout, "calls needed for %d questions: %d\n", *target, callsForTarget)
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "scenario: enough calls for the target question count")
	fmt.Fprintf(stdout, "  calls:                   %10d  (%d passages per call, %d questions per call after the %.0f%% yield)\n",
		selectedCalls, *perCall, int(questionsPerCall), *yieldRate*100)
	if *mix {
		fmt.Fprintf(stdout, "  calls spanning 2 docs:   %10d\n", mixedCalls)
	}
	fmt.Fprintf(stdout, "  passages selected:       %10d  (of %d available, %.0f%%)\n",
		selected, len(passages), 100*float64(selected)/float64(len(passages)))
	fmt.Fprintf(stdout, "  context tokens:          %10d\n", contextForTarget)
	fmt.Fprintf(stdout, "  instruction tokens:      %10d  (%d per call)\n", selectedCalls**promptTokens, *promptTokens)
	fmt.Fprintf(stdout, "  input tokens total:      %10d\n", contextForTarget+selectedCalls**promptTokens)
	fmt.Fprintf(stdout, "  output tokens:           %10d  (%d per question the model writes)\n",
		int(math.Ceil(float64(selectedCalls)*questionsPerCall))**questionTokens, *questionTokens)
	fmt.Fprintf(stdout, "  tokens total:            %10d\n",
		contextForTarget+selectedCalls**promptTokens+int(math.Ceil(float64(selectedCalls)*questionsPerCall))**questionTokens)

	if !quota.Empty() && len(quota.PerTopic) > 0 {
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "per document: quota, passages taken, questions that can come out of them")
		short := 0
		for _, d := range docs {
			want := quota.PerTopic[d]
			can := int(math.Floor(float64(take[d]) * questionsPerPassage))
			mark := ""
			if can < want {
				mark = fmt.Sprintf("  <-- short by %d", want-can)
				short++
			}
			fmt.Fprintf(stdout, "  %-46s quota %3d  passages %4d  questions ~%3d%s\n",
				d, want, take[d], can, mark)
		}
		if short > 0 {
			fmt.Fprintf(stdout, "  %d document(s) cannot reach their quota with these passages\n", short)
		}
	}
	return 0
}

// packCalls counts the generation calls the selected passages need.
//
// Without mixing, every document is packed on its own: a document with three
// passages and four passages per call needs a whole call.
//
// With mixing, the leftovers of a document may be topped up with passages of
// the next document, and one call never spans more than two documents. The
// passages of one document stay together, so a question can still be written
// from a single document.
func packCalls(take map[string]int, docs []string, perCall int, mix bool) (calls int, mixed int) {
	if perCall <= 0 {
		return 0, 0
	}
	used := 0
	var open []string
	flush := func() {
		if used > 0 {
			if len(open) > 1 {
				mixed++
			}
			calls++
		}
		used = 0
		open = nil
	}
	for _, d := range docs {
		left := take[d]
		for left > 0 {
			if used > 0 && mix && len(open) >= 2 {
				// This call already spans two documents; close it before a
				// third one joins.
				flush()
			}
			room := perCall - used
			if room > left {
				room = left
			}
			used += room
			left -= room
			if len(open) == 0 || open[len(open)-1] != d {
				open = append(open, d)
			}
			if used >= perCall {
				flush()
			}
		}
		if !mix {
			// Without mixing a document never shares a call with another one.
			flush()
		}
	}
	flush()
	return calls, mixed
}

// splitLines splits on "\n" and drops the empty tail a trailing newline leaves.
func splitLines(text string) []string {
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// numberLines returns the source lines of a chunk, numbered from 1.
func numberLines(source []string, start, end int) ([]Line, bool) {
	if start < 1 || end > len(source) || start > end {
		return nil, false
	}
	out := make([]Line, 0, end-start+1)
	for n := start; n <= end; n++ {
		out = append(out, Line{N: n, Text: strings.TrimRight(source[n-1], "\r")})
	}
	return out, true
}

func numberedText(lines []Line) string {
	var b strings.Builder
	for _, l := range lines {
		fmt.Fprintf(&b, "%d\t%s\n", l.N, l.Text)
	}
	return b.String()
}

// hasBodyText reports whether the passage holds at least one body-text line.
//
// The kinds come from the whole document, so a line inside a fenced code block
// counts as code even when it looks like a heading.
func hasBodyText(lines []Line, kinds []validate.LineKind) bool {
	for _, l := range lines {
		if l.N-1 < 0 || l.N-1 >= len(kinds) {
			continue
		}
		if kinds[l.N-1] == validate.LineContent {
			return true
		}
	}
	return false
}

func sumMap(m map[string]int) int {
	total := 0
	for _, v := range m {
		total += v
	}
	return total
}
