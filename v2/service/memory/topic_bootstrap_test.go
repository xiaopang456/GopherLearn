package memory

import (
	memorydao "GopherAI/dao/memory"
	"GopherAI/model"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

var bootstrapClock = time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)

// permissiveTopicRepository returns every row filed under the requested key,
// without applying the owner, status, confidence or expiry predicates the real
// SQL applies.
//
// That is the point. The DAO has its own tests; what this fixture checks is the
// second half of the defence in depth - that a lifecycle or ownership mistake
// in one layer does not put another account's private notes in front of a model
// with a citable label attached.
type permissiveTopicRepository struct {
	rows []model.LongTermMemory
	err  error

	topicCalls  int
	lastKeys    []string
	globalCalls int
	keyListings int
}

func (r *permissiveTopicRepository) ListAllActive(context.Context, int64, model.MemoryScope, float64, int) ([]model.LongTermMemory, error) {
	r.globalCalls++
	return nil, nil
}

func (r *permissiveTopicRepository) ListTopicKeys(context.Context, int64, int) ([]memorydao.TopicKeyRow, error) {
	r.keyListings++
	return nil, nil
}

func (r *permissiveTopicRepository) ListByTopicKey(_ context.Context, _ int64, _ model.MemoryScope,
	topicKey string, _ float64, limit int) ([]model.LongTermMemory, error) {
	r.topicCalls++
	r.lastKeys = append(r.lastKeys, topicKey)
	if r.err != nil {
		return nil, r.err
	}
	matched := make([]model.LongTermMemory, 0, len(r.rows))
	for _, row := range r.rows {
		if row.TopicKey == topicKey {
			matched = append(matched, row)
		}
	}
	if len(matched) > limit {
		matched = matched[:limit]
	}
	return matched, nil
}

func (r *permissiveTopicRepository) ListTopicWindow(_ context.Context, _ int64, _ model.MemoryScope,
	topicKeys []string, _ float64, _ time.Time, limit int) (memorydao.TopicMemoryWindow, error) {
	r.topicCalls++
	r.lastKeys = append(r.lastKeys, topicKeys...)
	if r.err != nil {
		return memorydao.TopicMemoryWindow{}, r.err
	}
	allowed := make(map[string]struct{}, len(topicKeys))
	for _, key := range topicKeys {
		allowed[key] = struct{}{}
	}
	matched := make([]model.LongTermMemory, 0, len(r.rows))
	for _, row := range r.rows {
		if _, ok := allowed[row.TopicKey]; ok {
			matched = append(matched, row)
		}
	}
	window := memorydao.TopicMemoryWindow{EligibleCount: int64(len(matched)), Memories: matched}
	if len(window.Memories) > limit {
		window.Memories = window.Memories[:limit]
	}
	return window, nil
}

func topicMemoryRow(id string, userID int64, topicKey string) model.LongTermMemory {
	return model.LongTermMemory{
		ID: id, UserID: userID, Scope: model.MemoryScopeTopic, Subject: id,
		Content: id + " content", ContentHash: model.MemoryContentHash(id + " content"),
		StructuredJSON: `{}`, TopicKey: topicKey, SourceRefs: model.StringList{"source:" + id},
		Durability: model.MemoryDurabilityStable, Confidence: 0.9, Importance: 7,
		Status: model.LongTermMemoryActive, UpdatedAt: bootstrapClock,
	}
}

func newBootstrapRetriever(t *testing.T, repository RecallRepository, learning ActiveLearningReader) *Retriever {
	t.Helper()
	retriever, err := NewRetriever(repository, learning, nil, DefaultRetrieverConfig())
	if err != nil {
		t.Fatal(err)
	}
	retriever.now = func() time.Time { return bootstrapClock }
	return retriever
}

func TestTopicMemorySnapshotReportsThePreLimitCount(t *testing.T) {
	rows := make([]model.LongTermMemory, 0, 8)
	for i := 0; i < 8; i++ {
		rows = append(rows, topicMemoryRow(fmt.Sprintf("m-%d", i), 11, "rpc-粘包"))
	}
	retriever := newBootstrapRetriever(t, &permissiveTopicRepository{rows: rows}, nil)
	snapshot, err := retriever.TopicMemorySnapshotByKeys(context.Background(), 11, []string{"RPC 粘包"})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Rows) != 8 || snapshot.EligibleCount != 8 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func TestTopicMemorySnapshotFailsClosedOnAnIneligibleWindowRow(t *testing.T) {
	foreign := topicMemoryRow("foreign", 99, "rpc-粘包")
	retriever := newBootstrapRetriever(t, &permissiveTopicRepository{rows: []model.LongTermMemory{foreign}}, nil)
	if _, err := retriever.TopicMemorySnapshotByKeys(context.Background(), 11, []string{"rpc-粘包"}); err == nil {
		t.Fatal("snapshot accepted a foreign row")
	}
}

// The happy path, and the two things it must not do: no lexical topic listing,
// no global read. Those are what RelevantMemories would have run, and running
// them here would reopen the sources the trust boundary excludes.
func TestTopicMemoriesByKeysReadsOnlyTheKeysItWasGiven(t *testing.T) {
	repository := &permissiveTopicRepository{rows: []model.LongTermMemory{
		topicMemoryRow("m-1", 11, "rpc-粘包"),
		topicMemoryRow("m-2", 11, "redis-ttl"),
	}}
	retriever := newBootstrapRetriever(t, repository, nil)

	rows, err := retriever.TopicMemoriesByKeys(context.Background(), 11, []string{"RPC 粘包"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "m-1" {
		t.Fatalf("rows = %#v", rows)
	}
	if rows[0].Owner != 11 || rows[0].TopicKey != "rpc-粘包" ||
		rows[0].Status != string(model.LongTermMemoryActive) || rows[0].Scope != string(model.MemoryScopeTopic) {
		t.Fatalf("the record lost its server identity: %#v", rows[0])
	}
	// The version has to be the one the candidate layer would stamp, or the
	// tool cannot recognise the same record when a search returns it.
	if rows[0].Version != model.SemanticMemoryVersion(topicMemoryRow("m-1", 11, "rpc-粘包")) {
		t.Fatalf("version = %d", rows[0].Version)
	}
	if rows[0].Line == "" {
		t.Fatal("the record carries no rendered line")
	}
	// The key was normalised before it reached the DAO.
	if len(repository.lastKeys) != 1 || repository.lastKeys[0] != "rpc-粘包" {
		t.Fatalf("the DAO was asked for %v", repository.lastKeys)
	}
	if repository.globalCalls != 0 || repository.keyListings != 0 {
		t.Fatalf("the bootstrap read ran another channel: global=%d keys=%d",
			repository.globalCalls, repository.keyListings)
	}
}

// A learning reader is attached in production, and this path must not consult
// it: the keys are already decided, and re-reading is a second chance for the
// two answers to disagree.
func TestTopicMemoriesByKeysNeverReadsTheLearningSession(t *testing.T) {
	learning := &countingActiveLearning{}
	repository := &permissiveTopicRepository{rows: []model.LongTermMemory{topicMemoryRow("m-1", 11, "rpc-粘包")}}
	retriever := newBootstrapRetriever(t, repository, learning)

	if _, err := retriever.TopicMemoriesByKeys(context.Background(), 11, []string{"rpc-粘包"}); err != nil {
		t.Fatal(err)
	}
	if learning.calls != 0 {
		t.Fatalf("the bootstrap read consulted the learning session %d times", learning.calls)
	}
}

type countingActiveLearning struct{ calls int }

func (c *countingActiveLearning) GetActive(context.Context, int64, string) (*model.LearningSession, error) {
	c.calls++
	return nil, errors.New("must not be called")
}

// Every lifecycle and ownership rejection, on rows the fake happily returns.
func TestTopicMemoriesByKeysRejectsIneligibleRows(t *testing.T) {
	foreign := topicMemoryRow("m-foreign", 12, "rpc-粘包")
	archived := topicMemoryRow("m-archived", 11, "rpc-粘包")
	archived.Status = model.LongTermMemoryArchived
	deleted := topicMemoryRow("m-deleted", 11, "rpc-粘包")
	deleted.Status = model.LongTermMemoryDeleted
	superseded := topicMemoryRow("m-superseded", 11, "rpc-粘包")
	superseded.Status = model.LongTermMemorySuperseded
	expired := topicMemoryRow("m-expired", 11, "rpc-粘包")
	past := bootstrapClock.Add(-time.Hour)
	// Expiry only bites on a time-bound record; a stable one has no deadline to
	// pass. Setting both is what the write path does, and setting only the date
	// would test a row production never produces.
	expired.Durability = model.MemoryDurabilityTimeBound
	expired.ValidUntil = &past
	lowConfidence := topicMemoryRow("m-low", 11, "rpc-粘包")
	lowConfidence.Confidence = 0.1
	wrongScope := topicMemoryRow("m-global", 11, "rpc-粘包")
	wrongScope.Scope = model.MemoryScopeGlobal

	repository := &permissiveTopicRepository{rows: []model.LongTermMemory{
		foreign, archived, deleted, superseded, expired, lowConfidence, wrongScope,
	}}
	retriever := newBootstrapRetriever(t, repository, nil)

	rows, err := retriever.TopicMemoriesByKeys(context.Background(), 11, []string{"rpc-粘包"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("an ineligible record survived: %#v", rows)
	}

	// And a key that is merely close is not the key. No prefix, no alias.
	near := &permissiveTopicRepository{rows: []model.LongTermMemory{topicMemoryRow("m-1", 11, "rpc-粘包-进阶")}}
	nearRetriever := newBootstrapRetriever(t, near, nil)
	nearRows, err := nearRetriever.TopicMemoriesByKeys(context.Background(), 11, []string{"rpc-粘包"})
	if err != nil {
		t.Fatal(err)
	}
	if len(nearRows) != 0 {
		t.Fatalf("a near-miss topic key was accepted: %#v", nearRows)
	}
}

// Ordering and the count ceiling are the ones RelevantMemories already applies,
// so the two paths cannot disagree about which five of a large topic a learner
// sees. The ceiling is the existing MaxResults; this task does not widen it.
func TestTopicMemoriesByKeysOrdersStablyAndStopsAtTheExistingCeiling(t *testing.T) {
	rows := make([]model.LongTermMemory, 0, 8)
	for index := 0; index < 8; index++ {
		row := topicMemoryRow(string(rune('a'+index)), 11, "rpc-粘包")
		row.Importance = 5
		rows = append(rows, row)
	}
	// One confirmed record must sort ahead of everything else whatever order
	// the database returned them in.
	rows[6].UserConfirmed = true
	repository := &permissiveTopicRepository{rows: rows}
	retriever := newBootstrapRetriever(t, repository, nil)

	first, err := retriever.TopicMemoriesByKeys(context.Background(), 11, []string{"rpc-粘包"})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != DefaultRetrieverConfig().MaxResults {
		t.Fatalf("returned %d records, want the existing ceiling %d",
			len(first), DefaultRetrieverConfig().MaxResults)
	}
	if first[0].ID != "g" {
		t.Fatalf("the confirmed record did not sort first: %#v", first)
	}
	second, err := retriever.TopicMemoriesByKeys(context.Background(), 11, []string{"rpc-粘包"})
	if err != nil {
		t.Fatal(err)
	}
	for index := range first {
		if first[index].ID != second[index].ID {
			t.Fatalf("the order is not stable: %v vs %v", first, second)
		}
	}
}

// Duplicate and empty keys are the caller's problem to survive, not the
// database's to answer twice.
func TestTopicMemoriesByKeysNormalisesAndDeduplicatesKeys(t *testing.T) {
	repository := &permissiveTopicRepository{rows: []model.LongTermMemory{topicMemoryRow("m-1", 11, "rpc-粘包")}}
	retriever := newBootstrapRetriever(t, repository, nil)

	rows, err := retriever.TopicMemoriesByKeys(context.Background(), 11,
		[]string{"RPC 粘包", "rpc-粘包", "   ", ""})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %#v", rows)
	}
	if repository.topicCalls != 1 {
		t.Fatalf("the same key was read %d times", repository.topicCalls)
	}

	// No usable key at all means no read at all.
	empty := &permissiveTopicRepository{}
	emptyRetriever := newBootstrapRetriever(t, empty, nil)
	if rows, err = emptyRetriever.TopicMemoriesByKeys(context.Background(), 11, []string{"", "!!!"}); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 || empty.topicCalls != 0 {
		t.Fatalf("an unusable key still reached the database: calls=%d", empty.topicCalls)
	}
}

// A partial read is worse than none: to the model a short block looks exactly
// like a complete one, so the error propagates and the caller drops the block.
func TestTopicMemoriesByKeysFailsRatherThanReturningHalfABlock(t *testing.T) {
	repository := &permissiveTopicRepository{err: errors.New("memory store down")}
	retriever := newBootstrapRetriever(t, repository, nil)
	if _, err := retriever.TopicMemoriesByKeys(context.Background(), 11, []string{"rpc-粘包"}); err == nil {
		t.Fatal("a database failure was reported as an empty topic")
	}
	// And an untrusted identity never reaches the database at all.
	if _, err := retriever.TopicMemoriesByKeys(context.Background(), 0, []string{"rpc-粘包"}); err == nil {
		t.Fatal("the read ran without a trusted owner")
	}
}
