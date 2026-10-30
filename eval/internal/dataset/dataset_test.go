package dataset

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testdata = "../../testdata"

func loadTestCorpus(t *testing.T) *Corpus {
	t.Helper()
	corpus, err := LoadCorpus(filepath.Join(testdata, "corpus.json"))
	if err != nil {
		t.Fatal(err)
	}
	return corpus
}

func TestCountLines(t *testing.T) {
	cases := map[string]int{
		"":              0,
		"a":             1,
		"a\n":           1,
		"a\nb":          2,
		"a\nb\n":        2,
		"a\r\nb\r\n":    2,
		"\n":            1,
		"\n\n":          2,
		"a\n\nb":        3,
		"no newline at": 1,
	}
	for in, want := range cases {
		if got := CountLines([]byte(in)); got != want {
			t.Errorf("CountLines(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestLoadCorpusReadsTheSyntheticFixture(t *testing.T) {
	corpus := loadTestCorpus(t)
	if corpus.Name != "synthetic-harbour-v1" || len(corpus.Documents) != 3 {
		t.Fatalf("corpus = %q with %d documents", corpus.Name, len(corpus.Documents))
	}
	// Line counts of the fixture files; the evidence in dataset.jsonl is written
	// against these numbers.
	want := map[string]int{"tide-tables.md": 21, "lantern-maintenance.md": 24, "notes/orchard-grafting.md": 20}
	for file, lines := range want {
		doc, ok := corpus.Lookup(file)
		if !ok {
			t.Fatalf("%s is not in the corpus", file)
		}
		if doc.Lines != lines {
			t.Errorf("%s has %d lines, fixture says %d", file, doc.Lines, lines)
		}
		sum, err := os.ReadFile(doc.Path)
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(sum)
		if doc.SHA256 != hex.EncodeToString(hash[:]) || doc.Size != int64(len(sum)) {
			t.Errorf("%s: hash or size does not describe the file", file)
		}
	}
	if _, ok := corpus.Lookup("missing.md"); ok {
		t.Fatal("Lookup invented a document")
	}
}

// writeCorpus builds a corpus in a temp directory from name -> content and
// returns the manifest path. manifest may use %s-free literal JSON.
func writeCorpus(t *testing.T, files map[string]string, manifest string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "corpus.json")
	if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadCorpusRejectsBadManifests(t *testing.T) {
	files := map[string]string{"a.md": "one\ntwo\n", "empty.md": ""}
	sumA := sha256.Sum256([]byte("one\ntwo\n"))
	cases := map[string]struct {
		manifest string
		want     string
	}{
		"not json":          {`not json`, "parse corpus manifest"},
		"unknown field":     {`{"name":"x","documents":[{"file":"a.md"}],"extra":1}`, "unknown field"},
		"no name":           {`{"documents":[{"file":"a.md"}]}`, "name is required"},
		"no documents":      {`{"name":"x","documents":[]}`, "documents is empty"},
		"absolute name":     {`{"name":"x","documents":[{"file":"/etc/passwd"}]}`, "relative path"},
		"parent traversal":  {`{"name":"x","documents":[{"file":"../a.md"}]}`, "relative path"},
		"backslash":         {`{"name":"x","documents":[{"file":"..\\a.md"}]}`, "relative path"},
		"drive letter":      {`{"name":"x","documents":[{"file":"C:a.md"}]}`, "relative path"},
		"duplicate":         {`{"name":"x","documents":[{"file":"a.md"},{"file":"a.md"}]}`, "listed twice"},
		"missing file":      {`{"name":"x","documents":[{"file":"b.md"}]}`, "b.md"},
		"empty file":        {`{"name":"x","documents":[{"file":"empty.md"}]}`, "is empty"},
		"hash mismatch":     {`{"name":"x","documents":[{"file":"a.md","sha256":"` + strings.Repeat("0", 64) + `"}]}`, "changed"},
		"root with parent":  {`{"name":"x","root":"../up","documents":[{"file":"a.md"}]}`, "root"},
		"dot as a document": {`{"name":"x","documents":[{"file":"."}]}`, "relative path"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LoadCorpus(writeCorpus(t, files, tc.manifest))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}

	t.Run("a matching hash is accepted, in either case", func(t *testing.T) {
		upper := strings.ToUpper(hex.EncodeToString(sumA[:]))
		corpus, err := LoadCorpus(writeCorpus(t, files, `{"name":"x","documents":[{"file":"a.md","sha256":"`+upper+`"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		if corpus.Documents[0].SHA256 != strings.ToLower(upper) {
			t.Fatalf("hash = %s", corpus.Documents[0].SHA256)
		}
	})
}

func TestCorpusHashTracksContentNotFormatting(t *testing.T) {
	files := map[string]string{"a.md": "one\n", "b.md": "two\n"}
	one := writeCorpus(t, files, `{"name":"first","documents":[{"file":"a.md"},{"file":"b.md"}]}`)
	reordered := writeCorpus(t, files, `{
  "name": "renamed",
  "documents": [ {"file": "b.md"}, {"file": "a.md"} ]
}`)
	changed := writeCorpus(t, map[string]string{"a.md": "one!\n", "b.md": "two\n"}, `{"name":"first","documents":[{"file":"a.md"},{"file":"b.md"}]}`)
	fewer := writeCorpus(t, files, `{"name":"first","documents":[{"file":"a.md"}]}`)

	hash := func(path string) string {
		c, err := LoadCorpus(path)
		if err != nil {
			t.Fatal(err)
		}
		return c.Hash()
	}
	if hash(one) != hash(reordered) {
		t.Error("reordering or renaming the manifest changed the corpus hash")
	}
	if hash(one) == hash(changed) {
		t.Error("changing a document did not change the corpus hash")
	}
	if hash(one) == hash(fewer) {
		t.Error("dropping a document did not change the corpus hash")
	}
}

func TestLoadDatasetReadsTheSyntheticFixture(t *testing.T) {
	data, err := LoadDataset(filepath.Join(testdata, "dataset.jsonl"), loadTestCorpus(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Questions) != 6 {
		t.Fatalf("%d questions", len(data.Questions))
	}
	if len(data.Filter(SplitDev)) != 5 || len(data.Filter(SplitSealed)) != 1 {
		t.Fatalf("dev %d, sealed %d", len(data.Filter(SplitDev)), len(data.Filter(SplitSealed)))
	}
	if len(data.Version) != 64 {
		t.Fatalf("version = %q", data.Version)
	}
	byID := map[string]Question{}
	for _, q := range data.Questions {
		byID[q.ID] = q
	}
	if q := byID["q-fuel"]; len(q.Evidence) != 2 {
		t.Errorf("q-fuel should keep both alternative sets, has %d", len(q.Evidence))
	}
	if q := byID["q-wick-glass"]; len(q.Evidence) != 1 || len(q.Evidence[0]) != 2 {
		t.Errorf("q-wick-glass should be one set of two spans, got %+v", q.Evidence)
	}
	if q := byID["q-tungsten"]; q.Answerable || len(q.Evidence) != 0 {
		t.Errorf("q-tungsten should be unanswerable without evidence: %+v", q)
	}
}

func TestEvidenceSetLinesCountOverlapOnce(t *testing.T) {
	set := EvidenceSet{
		{File: "a.md", Start: 3, End: 5},
		{File: "a.md", Start: 5, End: 6},
		{File: "b.md", Start: 5, End: 5},
	}
	lines := set.Lines()
	if len(lines) != 5 {
		t.Fatalf("%d lines, want 5 (a:3,4,5,6 and b:5): %v", len(lines), lines)
	}
	for _, ref := range []LineRef{{"a.md", 3}, {"a.md", 6}, {"b.md", 5}} {
		if _, ok := lines[ref]; !ok {
			t.Errorf("missing %+v", ref)
		}
	}
	if _, ok := lines[LineRef{"b.md", 3}]; ok {
		t.Error("lines of different files must not mix")
	}
}

func TestParseDatasetRejectsStructuralProblems(t *testing.T) {
	corpus := loadTestCorpus(t)
	ok := `{"id":"ok","question":"q","answerable":true,"evidence_sets":[[{"file":"tide-tables.md","start":1,"end":2}]],"split":"dev"}`
	cases := map[string]struct{ line, want string }{
		"not json":              {`{nope`, "not a valid question object"},
		"unknown field":         {`{"id":"a","question":"q","answerable":false,"split":"dev","evidence":[]}`, "unknown field"},
		"unknown span field":    {`{"id":"a","question":"q","answerable":true,"split":"dev","evidence_sets":[[{"file":"tide-tables.md","start":1,"end":2,"note":"x"}]]}`, "unknown field"},
		"two values on a line":  {ok + ` ` + ok, "more than one JSON value"},
		"no id":                 {`{"question":"q","answerable":false,"split":"dev"}`, "id is required"},
		"blank id":              {`{"id":"  ","question":"q","answerable":false,"split":"dev"}`, "id is required"},
		"no question":           {`{"id":"a","answerable":false,"split":"dev"}`, "question is required"},
		"blank question":        {`{"id":"a","question":" ","answerable":false,"split":"dev"}`, "question is required"},
		"no answerable":         {`{"id":"a","question":"q","split":"dev"}`, "answerable is required"},
		"no split":              {`{"id":"a","question":"q","answerable":false}`, "split is required"},
		"bad split":             {`{"id":"a","question":"q","answerable":false,"split":"test"}`, `split "test"`},
		"answerable no sets":    {`{"id":"a","question":"q","answerable":true,"split":"dev"}`, "at least one evidence set"},
		"answerable null sets":  {`{"id":"a","question":"q","answerable":true,"evidence_sets":null,"split":"dev"}`, "at least one evidence set"},
		"unanswerable has sets": {`{"id":"a","question":"q","answerable":false,"evidence_sets":[[{"file":"tide-tables.md","start":1,"end":1}]],"split":"dev"}`, "must have no evidence_sets"},
		"empty set":             {`{"id":"a","question":"q","answerable":true,"evidence_sets":[[]],"split":"dev"}`, "evidence_sets[0] is empty"},
		"file not in corpus":    {`{"id":"a","question":"q","answerable":true,"evidence_sets":[[{"file":"ghost.md","start":1,"end":1}]],"split":"dev"}`, `"ghost.md" is not in the corpus`},
		"start zero":            {`{"id":"a","question":"q","answerable":true,"evidence_sets":[[{"file":"tide-tables.md","start":0,"end":3}]],"split":"dev"}`, "at least 1"},
		"end before start":      {`{"id":"a","question":"q","answerable":true,"evidence_sets":[[{"file":"tide-tables.md","start":5,"end":4}]],"split":"dev"}`, "before start"},
		"end past the file":     {`{"id":"a","question":"q","answerable":true,"evidence_sets":[[{"file":"tide-tables.md","start":20,"end":22}]],"split":"dev"}`, "past the last line"},
		"second set is bad":     {`{"id":"a","question":"q","answerable":true,"evidence_sets":[[{"file":"tide-tables.md","start":1,"end":2}],[{"file":"tide-tables.md","start":1,"end":99}]],"split":"dev"}`, "evidence_sets[1][0]"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseDataset([]byte(tc.line+"\n"), corpus)
			var validation *ValidationError
			if !errors.As(err, &validation) {
				t.Fatalf("err = %v, want a ValidationError", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
			if validation.Issues[0].Line != 1 {
				t.Errorf("problem reported on line %d, want 1", validation.Issues[0].Line)
			}
		})
	}
}

func TestParseDatasetReportsEveryProblemWithItsLine(t *testing.T) {
	corpus := loadTestCorpus(t)
	file := strings.Join([]string{
		`{"id":"a","question":"q","answerable":false,"split":"dev"}`,
		``,
		`{"id":"a","question":"again","answerable":false,"split":"dev"}`,
		`{"id":"b","question":"q","answerable":true,"split":"dev"}`,
	}, "\n")
	_, err := ParseDataset([]byte(file), corpus)
	var validation *ValidationError
	if !errors.As(err, &validation) || len(validation.Issues) != 2 {
		t.Fatalf("err = %v", err)
	}
	// The blank line counts, so the duplicate is on line 3 and the bad one on 4.
	if validation.Issues[0].Line != 3 || validation.Issues[0].ID != "a" || !strings.Contains(validation.Issues[0].Message, "line 1") {
		t.Errorf("duplicate id issue = %+v", validation.Issues[0])
	}
	if validation.Issues[1].Line != 4 || validation.Issues[1].ID != "b" {
		t.Errorf("missing evidence issue = %+v", validation.Issues[1])
	}
}

func TestParseDatasetEmptyAndTrailingWhitespace(t *testing.T) {
	corpus := loadTestCorpus(t)
	for _, empty := range []string{"", "\n\n  \n"} {
		if _, err := ParseDataset([]byte(empty), corpus); err == nil || !strings.Contains(err.Error(), "no questions") {
			t.Errorf("%q: err = %v, want \"no questions\"", empty, err)
		}
	}
	line := `{"id":"a","question":"q","answerable":false,"split":"sealed"}`
	for _, text := range []string{line, line + "\n", line + "\r\n", "  " + line + "  \n\n"} {
		data, err := ParseDataset([]byte(text), corpus)
		if err != nil || len(data.Questions) != 1 {
			t.Errorf("%q: %v", text, err)
		}
	}
}

func TestParseDatasetCapsTheReportedIssues(t *testing.T) {
	corpus := loadTestCorpus(t)
	var b strings.Builder
	for i := 0; i < 80; i++ {
		b.WriteString(`{"id":"","question":"q","answerable":false,"split":"dev"}` + "\n")
	}
	_, err := ParseDataset([]byte(b.String()), corpus)
	if err == nil || !strings.Contains(err.Error(), "80 problem(s)") || !strings.Contains(err.Error(), "and 30 more") {
		t.Fatalf("err = %v", err)
	}
}

func TestDatasetVersionTracksTheBytes(t *testing.T) {
	corpus := loadTestCorpus(t)
	a, err := ParseDataset([]byte(`{"id":"a","question":"q","answerable":false,"split":"dev"}`), corpus)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseDataset([]byte(`{"id":"a","question":"q2","answerable":false,"split":"dev"}`), corpus)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := ParseDataset([]byte(`{"id":"a","question":"q","answerable":false,"split":"dev"}`), corpus)
	if a.Version == b.Version || a.Version != again.Version {
		t.Fatalf("versions: %s %s %s", a.Version, b.Version, again.Version)
	}
}
