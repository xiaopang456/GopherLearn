package agent

import (
	sessiondao "GopherAI/dao/session"
	"GopherAI/model"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
)

type fakeSummaryRepository struct {
	mu      sync.Mutex
	current *model.SessionSummary
}

func (f *fakeSummaryRepository) GetSummary(_ context.Context, userID int64, sessionID string) (*model.SessionSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.current == nil || f.current.UserID != userID || f.current.SessionID != sessionID {
		return nil, sessiondao.ErrSummaryNotFound
	}
	copy := *f.current
	return &copy, nil
}

func (f *fakeSummaryRepository) SaveSummary(_ context.Context, next *model.SessionSummary, expected uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	currentVersion := uint64(0)
	currentCovered := uint(0)
	if f.current != nil {
		currentVersion = f.current.Version
		currentCovered = f.current.CoveredMessageID
	}
	if currentVersion != expected || next.CoveredMessageID <= currentCovered {
		return sessiondao.ErrSummaryConflict
	}
	copy := *next
	copy.Version = expected + 1
	f.current = &copy
	next.Version = copy.Version
	return nil
}

type fakeSummaryMessages struct{ messages []model.Message }

func (f fakeSummaryMessages) ListAfter(_ context.Context, _ string, after uint, limit int) ([]model.Message, error) {
	result := make([]model.Message, 0, len(f.messages))
	for _, message := range f.messages {
		if message.ID > after {
			result = append(result, message)
		}
		if len(result) == limit {
			break
		}
	}
	return result, nil
}

type recordingSummaryGenerator struct {
	mu       sync.Mutex
	calls    int
	previous string
	messages []model.Message
	err      error
	barrier  chan struct{}
}

type summaryMemoryRecorder struct {
	mu        sync.Mutex
	userID    int64
	sessionID string
	version   uint64
	calls     int
}

func (r *summaryMemoryRecorder) PublishSummaryCompacted(_ context.Context, userID int64, sessionID string, version uint64, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.userID, r.sessionID, r.version = userID, sessionID, version
	r.calls++
	return nil
}

func (g *recordingSummaryGenerator) GenerateSummary(ctx context.Context, previous string, messages []model.Message) (string, error) {
	g.mu.Lock()
	g.calls++
	g.previous = previous
	g.messages = append([]model.Message(nil), messages...)
	barrier := g.barrier
	err := g.err
	g.mu.Unlock()
	if barrier != nil {
		select {
		case <-barrier:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if err != nil {
		return "", err
	}
	return `{"current_goal":"finish Agent","confirmed_decisions":["use optimistic locking"],"completed_items":[],"open_loops":["tests"],"user_corrections":[],"key_errors":[],"references":[]}`, nil
}

func summaryMessages(count int) []model.Message {
	messages := make([]model.Message, 0, count)
	for i := 1; i <= count; i++ {
		messages = append(messages, model.Message{ID: uint(i), IsUser: i%2 == 1, Content: fmt.Sprintf("message-%d", i)})
	}
	return messages
}

func TestRollingSummaryIsIncrementalAndDoesNotRepeatCoveredMessages(t *testing.T) {
	repository := &fakeSummaryRepository{current: &model.SessionSummary{UserID: 7, SessionID: "s", Content: `{"current_goal":"old"}`, CoveredMessageID: 2, Version: 3}}
	generator := &recordingSummaryGenerator{}
	provider, err := NewRollingSummaryProvider(repository, fakeSummaryMessages{messages: summaryMessages(16)}, generator, DefaultSummaryConfig())
	if err != nil {
		t.Fatal(err)
	}
	memoryEvents := &summaryMemoryRecorder{}
	provider.SetMemoryPublisher(memoryEvents)
	snapshot, err := provider.SessionSummary(context.Background(), RunContext{UserID: 7, SessionID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.CoveredMessageID != 8 || snapshot.Version != 4 {
		t.Fatalf("snapshot = %+v, want covered=8 version=4", snapshot)
	}
	if generator.previous != `{"current_goal":"old"}` || len(generator.messages) != 6 || generator.messages[0].ID != 3 || generator.messages[5].ID != 8 {
		t.Fatalf("increment was not old-summary + ids 3..8: previous=%s messages=%#v", generator.previous, generator.messages)
	}
	if _, err := provider.SessionSummary(context.Background(), RunContext{UserID: 7, SessionID: "s"}); err != nil {
		t.Fatal(err)
	}
	if generator.calls != 1 {
		t.Fatalf("covered messages were summarized again; calls=%d", generator.calls)
	}
	if memoryEvents.calls != 1 || memoryEvents.userID != 7 || memoryEvents.sessionID != "s" || memoryEvents.version != 4 {
		t.Fatalf("summary memory event = %+v", memoryEvents)
	}
}

func TestRollingSummaryFailureReturnsLastValidSnapshot(t *testing.T) {
	repository := &fakeSummaryRepository{current: &model.SessionSummary{UserID: 1, SessionID: "s", Content: "old-valid", CoveredMessageID: 1, Version: 1}}
	provider, err := NewRollingSummaryProvider(repository, fakeSummaryMessages{messages: summaryMessages(15)}, &recordingSummaryGenerator{err: context.DeadlineExceeded}, DefaultSummaryConfig())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := provider.SessionSummary(context.Background(), RunContext{UserID: 1, SessionID: "s"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v", err)
	}
	if snapshot.Content != "old-valid" || snapshot.CoveredMessageID != 1 || repository.current.Version != 1 {
		t.Fatalf("last valid summary changed on failure: %+v", snapshot)
	}
}

func TestRollingSummaryFailureCooldownSkipsRepeatedGeneration(t *testing.T) {
	repository := &fakeSummaryRepository{current: &model.SessionSummary{UserID: 1, SessionID: "s", Content: "old-valid", CoveredMessageID: 1, Version: 1}}
	generator := &recordingSummaryGenerator{err: context.DeadlineExceeded}
	config := DefaultSummaryConfig()
	config.FailureCooldown = time.Minute
	provider, err := NewRollingSummaryProvider(repository, fakeSummaryMessages{messages: summaryMessages(15)}, generator, config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.SessionSummary(context.Background(), RunContext{UserID: 1, SessionID: "s"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first error = %v", err)
	}
	if _, err := provider.SessionSummary(context.Background(), RunContext{UserID: 1, SessionID: "s"}); !errors.Is(err, ErrSummaryCoolingDown) {
		t.Fatalf("second error = %v, want cooldown", err)
	}
	if generator.calls != 1 {
		t.Fatalf("generator calls = %d, want 1", generator.calls)
	}
}

func TestSummaryInputOmitsAssistantBodyButKeepsSourceHandles(t *testing.T) {
	payload := renderSummaryInput("", []model.Message{
		{ID: 1, IsUser: true, Content: "GAP-531 remains unresolved"},
		{ID: 2, IsUser: false, Content: "I located and fixed GAP-531", Sources: model.MessageSources{{DocID: "doc", DocVersion: 2, FileName: "RPC.md"}}},
	}, 2000)
	if !strings.Contains(payload, "GAP-531 remains unresolved") {
		t.Fatalf("user authority missing: %s", payload)
	}
	if strings.Contains(payload, "I located and fixed GAP-531") {
		t.Fatalf("assistant inference leaked into durable summary input: %s", payload)
	}
	if !strings.Contains(payload, "assistant_sources_only") || !strings.Contains(payload, `"file_name":"RPC.md"`) {
		t.Fatalf("source handle missing: %s", payload)
	}
}

func TestRollingSummaryTimeoutReturnsLastValidSnapshot(t *testing.T) {
	config := DefaultSummaryConfig()
	config.Timeout, config.StageBudget = 5*time.Millisecond, 5*time.Millisecond
	repository := &fakeSummaryRepository{current: &model.SessionSummary{UserID: 1, SessionID: "s", Content: "old-valid", CoveredMessageID: 1, Version: 1}}
	provider, err := NewRollingSummaryProvider(repository, fakeSummaryMessages{messages: summaryMessages(15)}, &recordingSummaryGenerator{barrier: make(chan struct{})}, config)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := provider.SessionSummary(context.Background(), RunContext{UserID: 1, SessionID: "s"})
	if !errors.Is(err, context.DeadlineExceeded) || snapshot.Content != "old-valid" || snapshot.CoveredMessageID != 1 {
		t.Fatalf("timeout did not preserve snapshot: snapshot=%+v err=%v", snapshot, err)
	}
}

func TestRollingSummaryConcurrentTriggersConvergeOnOneVersion(t *testing.T) {
	repository := &fakeSummaryRepository{}
	barrier := make(chan struct{})
	generator := &recordingSummaryGenerator{barrier: barrier}
	provider, err := NewRollingSummaryProvider(repository, fakeSummaryMessages{messages: summaryMessages(13)}, generator, DefaultSummaryConfig())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	results := make([]SummarySnapshot, 2)
	errs := make([]error, 2)
	for i := range results {
		go func(index int) {
			defer wg.Done()
			results[index], errs[index] = provider.SessionSummary(context.Background(), RunContext{UserID: 1, SessionID: "s"})
		}(i)
	}
	deadline := time.Now().Add(time.Second)
	for {
		generator.mu.Lock()
		calls := generator.calls
		generator.mu.Unlock()
		if calls == 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(barrier)
	wg.Wait()
	for i := range errs {
		if errs[i] != nil || results[i].Version != 1 || results[i].CoveredMessageID != 5 {
			t.Fatalf("result[%d]=%+v err=%v", i, results[i], errs[i])
		}
	}
	if repository.current.Version != 1 || repository.current.CoveredMessageID != 5 {
		t.Fatalf("repository did not converge: %+v", repository.current)
	}
}

func TestModelSummaryGeneratorRejectsUnknownAndEmptyOutput(t *testing.T) {
	responses := []string{
		`{"current_goal":"x","confirmed_decisions":[],"completed_items":[],"open_loops":[],"user_corrections":[],"key_errors":[],"references":[],"hidden_reasoning":"no"}`,
		`{"current_goal":"","confirmed_decisions":[],"completed_items":[],"open_loops":[],"user_corrections":[],"key_errors":[],"references":[]}`,
	}
	chat := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		return schema.AssistantMessage(responses[call-1], nil), nil
	}}
	generator, err := NewModelSummaryGenerator(chat, 2000, 500)
	if err != nil {
		t.Fatal(err)
	}
	for i := range responses {
		_, err := generator.GenerateSummary(context.Background(), "", []model.Message{{ID: 1, IsUser: true, Content: "hello"}})
		if err == nil {
			t.Fatalf("response %d should be rejected", i)
		}
	}
}

func TestSummaryV2BindsNewDurableItemsToCurrentUserMessages(t *testing.T) {
	previous := `{"current_goal":"old","confirmed_decisions":["保留旧决定"]}`
	messages := []model.Message{
		{ID: 3, IsUser: true, Content: "新决定采用长度前缀"},
		{ID: 4, IsUser: false, Content: "助手建议补测试"},
	}
	valid := `{"schema_version":2,"current_goal":"new","confirmed_decisions":[{"text":"保留旧决定","evidence_message_ids":[]},{"text":"采用长度前缀","evidence_message_ids":[3]}],"completed_items":[],"open_loops":[],"user_corrections":[],"key_errors":[],"references":[]}`
	document, err := decodeSummaryDocument(valid, previous, messages)
	if err != nil {
		t.Fatal(err)
	}
	if len(document.ConfirmedDecisions) != 2 || document.ConfirmedDecisions[1].EvidenceMessageIDs[0] != 3 {
		t.Fatalf("document=%#v", document)
	}
	for _, invalid := range []string{
		`{"schema_version":2,"current_goal":"x","confirmed_decisions":[{"text":"助手建议","evidence_message_ids":[4]}]}`,
		`{"schema_version":2,"current_goal":"x","confirmed_decisions":[{"text":"臆造","evidence_message_ids":[99]}]}`,
		`{"schema_version":2,"current_goal":"x","confirmed_decisions":[{"text":"新无证据项","evidence_message_ids":[]}]}`,
	} {
		if _, err := decodeSummaryDocument(invalid, previous, messages); err == nil {
			t.Fatalf("summary should fail: %s", invalid)
		}
	}
}

func TestSummaryV2PreservesPreviouslyValidatedEvidenceExactly(t *testing.T) {
	previous := `{"schema_version":2,"current_goal":"old","confirmed_decisions":[{"text":"采用 Protobuf","evidence_message_ids":[1]}]}`
	output := `{"schema_version":2,"current_goal":"continue","confirmed_decisions":[{"text":"采用 Protobuf","evidence_message_ids":[1]}]}`
	document, err := decodeSummaryDocument(output, previous, []model.Message{{ID: 2, IsUser: false, Content: "tail"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(document.ConfirmedDecisions) != 1 || document.ConfirmedDecisions[0].EvidenceMessageIDs[0] != 1 {
		t.Fatalf("document=%#v", document)
	}
}

// BUG-AGENT-102. Compaction runs inside the Runner's 45s deadline, so its cost
// is taken from the agent's own budget rather than added to it. These three
// tests pin the stage that bounds it.

func TestSummaryStageBudgetCapsTheModelCallWellInsideTheRequestDeadline(t *testing.T) {
	config := DefaultSummaryConfig()
	config.Timeout = 20 * time.Second
	config.StageBudget = 80 * time.Millisecond

	repository := &fakeSummaryRepository{current: &model.SessionSummary{
		UserID: 1, SessionID: "s", Content: "old-valid", CoveredMessageID: 1, Version: 1}}
	// A generator that never returns: without the stage budget this call would
	// run until the 20s Timeout, spending most of a 45s request.
	generator := &recordingSummaryGenerator{barrier: make(chan struct{})}
	provider, err := NewRollingSummaryProvider(repository,
		fakeSummaryMessages{messages: summaryMessages(15)}, generator, config)
	if err != nil {
		t.Fatal(err)
	}

	requestCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	started := time.Now()
	snapshot, err := provider.SessionSummary(requestCtx, RunContext{UserID: 1, SessionID: "s"})
	elapsed := time.Since(started)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want the stage deadline", err)
	}
	if elapsed > time.Second {
		t.Errorf("compaction held the request for %s; the stage budget was %s and did not bound it",
			elapsed, config.StageBudget)
	}
	// The request must still have almost all of its own deadline left.
	if remaining := time.Until(deadlineOf(t, requestCtx)); remaining < 44*time.Second {
		t.Errorf("only %s of the 45s request deadline survived compaction", remaining)
	}
	// The last good summary is intact, and its watermark still points at
	// message 1 — everything after it is raw history the builder will load.
	if snapshot.Content != "old-valid" || snapshot.CoveredMessageID != 1 || snapshot.Version != 1 {
		t.Errorf("stage timeout did not preserve the last good summary: %+v", snapshot)
	}
	if repository.current.Version != 1 || repository.current.Content != "old-valid" {
		t.Errorf("stage timeout changed the stored summary: %+v", repository.current)
	}
	if generator.calls != 1 {
		t.Errorf("generator calls = %d, want 1", generator.calls)
	}
}

func TestSummaryStageIsSkippedWhenTheRequestHasTooLittleLeft(t *testing.T) {
	repository := &fakeSummaryRepository{current: &model.SessionSummary{
		UserID: 1, SessionID: "s", Content: "old-valid", CoveredMessageID: 1, Version: 1}}
	generator := &recordingSummaryGenerator{}
	config := DefaultSummaryConfig()
	provider, err := NewRollingSummaryProvider(repository,
		fakeSummaryMessages{messages: summaryMessages(15)}, generator, config)
	if err != nil {
		t.Fatal(err)
	}

	// A request that is already nearly out of time. Starting a compaction here
	// would spend what is left and produce nothing.
	requestCtx, cancel := context.WithTimeout(context.Background(), 900*time.Millisecond)
	defer cancel()
	snapshot, err := provider.SessionSummary(requestCtx, RunContext{UserID: 1, SessionID: "s"})

	if !errors.Is(err, ErrSummaryStageSkipped) {
		t.Fatalf("error = %v, want ErrSummaryStageSkipped", err)
	}
	if generator.calls != 0 {
		t.Errorf("generator calls = %d, want 0; a skipped stage must not reach the model", generator.calls)
	}
	if snapshot.Content != "old-valid" || snapshot.CoveredMessageID != 1 {
		t.Errorf("skipping compaction lost the last good summary: %+v", snapshot)
	}

	// A skip is not a failure: the next request, with room to work, must be
	// allowed to compact rather than sitting out a 30 second cooldown it never
	// earned.
	roomyCtx, cancelRoomy := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancelRoomy()
	if _, err := provider.SessionSummary(roomyCtx, RunContext{UserID: 1, SessionID: "s"}); err != nil {
		t.Fatalf("the request after a skip failed: %v", err)
	}
	if generator.calls != 1 {
		t.Errorf("generator calls = %d, want 1; the skip opened a cooldown it should not have",
			generator.calls)
	}
}

func TestSummaryStageBudgetIsTheSmallestOfItsThreeBounds(t *testing.T) {
	config := DefaultSummaryConfig()
	config.Timeout = 20 * time.Second
	config.StageBudget = 8 * time.Second
	config.MaxDeadlineShare = 3
	provider, err := NewRollingSummaryProvider(&fakeSummaryRepository{},
		fakeSummaryMessages{}, &recordingSummaryGenerator{}, config)
	if err != nil {
		t.Fatal(err)
	}

	for _, testCase := range []struct {
		name      string
		remaining time.Duration
		want      time.Duration
		ok        bool
	}{
		{"no deadline at all yields the stage budget", 0, 8 * time.Second, true},
		{"a roomy request is bounded by the stage budget", 45 * time.Second, 8 * time.Second, true},
		{"a tight request is bounded by its own share", 12 * time.Second, 4 * time.Second, true},
		{"a nearly exhausted request is skipped", time.Second, 0, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			if testCase.remaining > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, testCase.remaining)
				defer cancel()
			}
			budget, ok := provider.stageBudget(ctx)
			if ok != testCase.ok {
				t.Fatalf("ok = %v, want %v (budget %s)", ok, testCase.ok, budget)
			}
			if !ok {
				return
			}
			// The share is computed from the clock, so allow a little slack.
			if delta := testCase.want - budget; delta < 0 || delta > 50*time.Millisecond {
				t.Errorf("budget = %s, want about %s", budget, testCase.want)
			}
		})
	}

	// The model timeout is still a hard ceiling: a stage budget can never widen
	// it past what the model itself is allowed.
	config.Timeout = 3 * time.Second
	tight, err := NewRollingSummaryProvider(&fakeSummaryRepository{},
		fakeSummaryMessages{}, &recordingSummaryGenerator{}, config)
	if err == nil {
		if budget, ok := tight.stageBudget(context.Background()); !ok || budget != 3*time.Second {
			t.Errorf("budget = %s (ok=%v), want the 3s model timeout as the ceiling", budget, ok)
		}
	} else if !strings.Contains(err.Error(), "stage budget") {
		t.Fatalf("unexpected config error: %v", err)
	}
}

func deadlineOf(t *testing.T, ctx context.Context) time.Time {
	t.Helper()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("context carries no deadline")
	}
	return deadline
}
