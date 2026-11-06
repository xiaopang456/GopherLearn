package agent

import (
	"context"
	"errors"
	"testing"

	"GopherAI/model"

	"github.com/cloudwego/eino/components/embedding"
)

// The index that finds the segments.
//
// The index is derived from MySQL, so everything about it is repairable: the
// tests here are about the two moments that matter - the write, and the
// repair after the index was lost.

// fakeSegmentEmbedder returns one fixed vector per text.
type fakeSegmentEmbedder struct {
	texts []string
	err   error
}

func (f *fakeSegmentEmbedder) EmbedStrings(_ context.Context, texts []string, _ ...embedding.Option) ([][]float64, error) {
	f.texts = append(f.texts, texts...)
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float64, len(texts))
	for index := range out {
		out[index] = make([]float64, 4)
		out[index][0] = 1
	}
	return out, nil
}

// fakeSegmentIndex is the derived view.
type fakeSegmentIndex struct {
	stored  []model.SessionSummarySegment
	vectors [][]float32
	users   map[int64]int
	reset   []int64
}

func (f *fakeSegmentIndex) EnsureIndex(context.Context) error { return nil }

func (f *fakeSegmentIndex) Store(_ context.Context, segment model.SessionSummarySegment, vector []float32) error {
	f.stored = append(f.stored, segment)
	f.vectors = append(f.vectors, vector)
	if f.users == nil {
		f.users = map[int64]int{}
	}
	f.users[segment.UserID]++
	return nil
}

func (f *fakeSegmentIndex) Delete(context.Context, int64, string, []int) error { return nil }

func (f *fakeSegmentIndex) CountUser(_ context.Context, userID int64) (int, error) {
	return f.users[userID], nil
}

func (f *fakeSegmentIndex) CountAll(context.Context) (int, error) {
	total := 0
	for _, count := range f.users {
		total += count
	}
	return total, nil
}

func (f *fakeSegmentIndex) ResetUser(_ context.Context, userID int64) error {
	f.reset = append(f.reset, userID)
	if f.users == nil {
		f.users = map[int64]int{}
	}
	f.users[userID] = 0
	return nil
}

// fakeSegmentIndexSource is the MySQL side.
type fakeSegmentIndexSource struct {
	segments []model.SessionSummarySegment
	statuses map[string]string
}

func (f *fakeSegmentIndexSource) UpdateSummarySegmentIndexStatus(_ context.Context, userID int64,
	sessionID string, seq int, status string) error {
	if f.statuses == nil {
		f.statuses = map[string]string{}
	}
	f.statuses[sessionID+":"+itoa(seq)] = status
	return nil
}

func (f *fakeSegmentIndexSource) ListSummarySegmentsForUser(_ context.Context, userID int64, _ int) ([]model.SessionSummarySegment, error) {
	out := make([]model.SessionSummarySegment, 0, len(f.segments))
	for _, segment := range f.segments {
		if segment.UserID == userID {
			out = append(out, segment)
		}
	}
	return out, nil
}

func (f *fakeSegmentIndexSource) CountSummarySegmentsForUser(_ context.Context, userID int64) (int, error) {
	count := 0
	for _, segment := range f.segments {
		if segment.UserID == userID {
			count++
		}
	}
	return count, nil
}

func (f *fakeSegmentIndexSource) CountSummarySegments(context.Context) (int, error) {
	return len(f.segments), nil
}

func (f *fakeSegmentIndexSource) ListSummarySegmentOwners(context.Context, int) ([]int64, error) {
	seen := map[int64]bool{}
	owners := make([]int64, 0, len(f.segments))
	for _, segment := range f.segments {
		if !seen[segment.UserID] {
			seen[segment.UserID] = true
			owners = append(owners, segment.UserID)
		}
	}
	return owners, nil
}

// itoa is the sequence number as a string.
func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := ""
	for value > 0 {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
	}
	return digits
}

// indexedSegment is one segment with a body.
func indexedSegment(seq int) model.SessionSummarySegment {
	content, _ := model.MarshalSessionSummarySegmentContent(model.SessionSummarySegmentContent{
		SchemaVersion: model.SessionSummarySegmentSchemaV1, Topic: "拆包",
		Points: []model.SessionSummarySegmentPoint{{Text: "决定使用长度字段", EvidenceMessageIDs: []uint{12}}},
	})
	return model.SessionSummarySegment{
		UserID: 7, SessionID: "session", Seq: seq, StartMessageID: 1, EndMessageID: 30,
		Topic: "拆包", Content: content,
	}
}

// The write publishes the segment's own words, not its stored JSON.
//
// An embedding of the JSON would be an embedding of the field names and the
// ids, and a search that matched the JSON would match the schema rather than
// what the learner said.
func TestTheIndexerEmbedsTheSegmentsWords(t *testing.T) {
	embedder := &fakeSegmentEmbedder{}
	index := &fakeSegmentIndex{}
	source := &fakeSegmentIndexSource{}
	indexer, err := NewSummarySegmentIndexer(index, source, embedder, 4)
	if err != nil {
		t.Fatal(err)
	}
	segment := indexedSegment(1)
	if err := indexer.Index(context.Background(), &segment); err != nil {
		t.Fatalf("index: %v", err)
	}
	if len(embedder.texts) != 1 {
		t.Fatalf("the embedder was called %d times", len(embedder.texts))
	}
	text := embedder.texts[0]
	if text == "" {
		t.Fatal("the segment was embedded as nothing")
	}
	for _, want := range []string{"拆包", "决定使用长度字段"} {
		if !contains(text, want) {
			t.Fatalf("the embedded text does not carry %q: %s", want, text)
		}
	}
	if contains(text, "schema_version") {
		t.Fatalf("the embedded text carries the stored JSON: %s", text)
	}
	if len(index.stored) != 1 || len(index.vectors[0]) != 4 {
		t.Fatalf("the index holds %d rows", len(index.stored))
	}
	if source.statuses["session:1"] != model.SessionSummarySegmentIndexIndexed {
		t.Fatalf("the segment's status is %q", source.statuses["session:1"])
	}
}

// A write that fails is recorded on the segment and does not throw the
// segment away.
//
// The segment is in MySQL and is readable there. Only the way of finding it is
// missing, and the repair puts it back.
func TestAFailedIndexWriteIsRecordedOnTheSegment(t *testing.T) {
	embedder := &fakeSegmentEmbedder{err: errors.New("provider refused")}
	index := &fakeSegmentIndex{}
	source := &fakeSegmentIndexSource{}
	indexer, err := NewSummarySegmentIndexer(index, source, embedder, 4)
	if err != nil {
		t.Fatal(err)
	}
	segment := indexedSegment(1)
	if err := indexer.Index(context.Background(), &segment); err == nil {
		t.Fatal("a refused embedding was reported as a success")
	}
	if source.statuses["session:1"] != model.SessionSummarySegmentIndexFailed {
		t.Fatalf("the segment's status is %q", source.statuses["session:1"])
	}
	if len(index.stored) != 0 {
		t.Fatal("the row was written even though the embedding failed")
	}
}

// The repair puts a lost index back.
//
// The check is the count: MySQL says how many segments exist and the index
// says how many rows it holds. A user whose counts agree is left alone,
// because rebuilding on every pass would re-embed every segment for nothing.
func TestTheRepairRebuildsAUserWhoseRowsAreMissing(t *testing.T) {
	embedder := &fakeSegmentEmbedder{}
	index := &fakeSegmentIndex{}
	source := &fakeSegmentIndexSource{segments: []model.SessionSummarySegment{
		indexedSegment(1), indexedSegment(2),
	}}
	indexer, err := NewSummarySegmentIndexer(index, source, embedder, 4)
	if err != nil {
		t.Fatal(err)
	}
	report, err := indexer.Repair(context.Background(), 100)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if report.Rebuilt != 1 {
		t.Fatalf("the repair rebuilt %d users, want 1", report.Rebuilt)
	}
	if report.Stored != 2 {
		t.Fatalf("the repair stored %d segments, want 2", report.Stored)
	}
	if len(index.reset) != 1 || index.reset[0] != 7 {
		t.Fatalf("the repair reset %v", index.reset)
	}
}

// The repair does nothing when the two agree.
func TestTheRepairLeavesAnIndexThatAgreesAlone(t *testing.T) {
	embedder := &fakeSegmentEmbedder{}
	index := &fakeSegmentIndex{users: map[int64]int{7: 1}}
	source := &fakeSegmentIndexSource{segments: []model.SessionSummarySegment{indexedSegment(1)}}
	indexer, err := NewSummarySegmentIndexer(index, source, embedder, 4)
	if err != nil {
		t.Fatal(err)
	}
	report, err := indexer.Repair(context.Background(), 100)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if report.Rebuilt != 0 {
		t.Fatalf("the repair rebuilt %d users, want none", report.Rebuilt)
	}
	if len(index.reset) != 0 {
		t.Fatalf("the repair reset %v", index.reset)
	}
}

// contains reports whether the text carries the word.
func contains(text, word string) bool {
	return len(text) >= len(word) && (func() bool {
		for index := 0; index+len(word) <= len(text); index++ {
			if text[index:index+len(word)] == word {
				return true
			}
		}
		return false
	})()
}
