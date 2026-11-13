package conversation

// The layered summary, driven end to end by the evaluation.
//
// The tests here do not call the generator and look at what it returned.
// They go through the evaluation's own entry: the role table is read from the
// product's own sources, the request goes over the HTTP boundary the
// evaluation puts in front of the provider, and a local stand-in answers it.
// What the run spent, what the run wrote and what the product counted are
// read back afterwards.
//
// The path that matters is the one a summary that does not fit takes: the
// model is asked again, with the same turns and a request to write it
// shorter, and the answer that is kept is the second one. Three things have
// to hold for that to be a real mechanism rather than a description of one:
//
//   - the request that asks for a shorter summary is still a request the
//     evaluation knows. A request it cannot classify is refused before it
//     reaches the provider, and a refusal reads as a broken product rather
//     than as an unclassified prompt;
//   - every call that was made is counted, including the ones that failed -
//     a batch that wrote nothing still cost what it cost;
//   - a batch that never fitted writes nothing at all and does not move the
//     session's watermark forward.
//
// The upstream is a stand-in rather than a provider: it answers on demand,
// it keeps every request it was given, and it can be told to fail or to
// answer with nothing.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"GopherAI/model"
)

// standInAnswer is what the stand-in upstream answers a call with.
type standInAnswer struct {
	// content is the answer the model returns.
	content string
	// status is the HTTP status the provider answers with. Zero is 200.
	status int
}

// standInUpstream is the provider the evaluation's counting proxy would
// reach.
//
// It is the far end of the proxy lane the evaluation drives, so a request
// that arrives here really travelled through the evaluation's own boundary:
// the role was read from the request that was sent, and the answer came back
// over HTTP.
type standInUpstream struct {
	mu sync.Mutex
	// requests is every request the upstream was given, in order.
	requests [][]map[string]any
	// answers is what each call is answered with, in order.
	answers []standInAnswer
	server  *httptest.Server
}

// newStandInUpstream starts the stand-in and returns it.
func newStandInUpstream(t *testing.T, answers []standInAnswer) *standInUpstream {
	t.Helper()
	upstream := &standInUpstream{answers: answers}
	upstream.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		r.Body.Close()
		if err != nil {
			http.Error(w, "the request could not be read", http.StatusBadRequest)
			return
		}
		var request struct {
			Messages []map[string]any `json:"messages"`
		}
		if err = json.Unmarshal(body, &request); err != nil {
			http.Error(w, "the request could not be read", http.StatusBadRequest)
			return
		}
		upstream.mu.Lock()
		upstream.requests = append(upstream.requests, request.Messages)
		index := len(upstream.requests)
		upstream.mu.Unlock()

		answer := standInAnswer{}
		if index <= len(upstream.answers) {
			answer = upstream.answers[index-1]
		}
		if answer.status != 0 && answer.status != http.StatusOK {
			http.Error(w, "the provider failed", answer.status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "stand-in",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   "stand-in",
			"choices": []any{map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": answer.content},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 32, "completion_tokens": 16, "total_tokens": 48},
		})
	}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

// base is the address the counting proxy would be at.
func (u *standInUpstream) base() string { return u.server.URL }

// count is how many requests the upstream was given.
func (u *standInUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.requests)
}

// request is one request the upstream was given.
func (u *standInUpstream) request(index int) (system string, user string, ok bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if index < 0 || index >= len(u.requests) {
		return "", "", false
	}
	messages := u.requests[index]
	for _, message := range messages {
		role, _ := message["role"].(string)
		content, _ := message["content"].(string)
		switch role {
		case "system":
			system = content
		case "user":
			user = content
		}
	}
	return system, user, system != "" || user != ""
}

// systemPrompts is the system prompt of every request the upstream was given.
func (u *standInUpstream) systemPrompts() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]string, 0, len(u.requests))
	for _, messages := range u.requests {
		for _, message := range messages {
			if role, _ := message["role"].(string); role == "system" {
				content, _ := message["content"].(string)
				out = append(out, content)
				break
			}
		}
	}
	return out
}

// localLayeredStore is the store a run writes its turns into when it does not
// own a disposable database.
//
// It is the evaluation's own store, on a local database: the turns, the
// session and the rolling summary are the same rows the run would write
// anywhere else, and the ids the segments cite are the ids the rows got.
type localLayeredStore struct {
	db *gorm.DB
}

// newLocalLayeredStore opens a local database for one run.
func newLocalLayeredStore(t *testing.T) *localLayeredStore {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(&model.User{}, &model.Session{}, &model.Message{},
		&model.SessionSummary{}, &model.SessionSummarySegment{}, &model.SessionWhiteboard{}); err != nil {
		t.Fatal(err)
	}
	return &localLayeredStore{db: db}
}

func (s *localLayeredStore) Init(ctx context.Context) error { return nil }

func (s *localLayeredStore) Seed(ctx context.Context, user model.User) error {
	return s.db.WithContext(ctx).Create(&user).Error
}

func (s *localLayeredStore) CreateSession(ctx context.Context, session model.Session) error {
	return s.db.WithContext(ctx).Create(&session).Error
}

func (s *localLayeredStore) CreateMessage(ctx context.Context, message model.Message) (*model.Message, error) {
	if err := s.db.WithContext(ctx).Create(&message).Error; err != nil {
		return nil, err
	}
	return &message, nil
}

func (s *localLayeredStore) GetSummary(ctx context.Context, userID int64, session string) (*model.SessionSummary, error) {
	var summary model.SessionSummary
	if err := s.db.WithContext(ctx).Where("user_id = ? AND session_id = ?", userID, session).
		First(&summary).Error; err != nil {
		return nil, err
	}
	return &summary, nil
}

func (s *localLayeredStore) SaveSummary(ctx context.Context, summary *model.SessionSummary, version uint64) error {
	return s.db.WithContext(ctx).Save(summary).Error
}

func (s *localLayeredStore) ListAfter(ctx context.Context, session string, id uint, limit int) ([]model.Message, error) {
	var messages []model.Message
	if err := s.db.WithContext(ctx).Where("session_id = ? AND id > ?", session, id).
		Order("id asc").Limit(limit).Find(&messages).Error; err != nil {
		return nil, err
	}
	return messages, nil
}

// repositoryRoot is the checkout the evaluation reads the product's prompts
// from.
//
// The role table is read out of the product's own source files, so a test
// that drove the run against a copy of the sources would classify the
// requests against prompts the product never sends. The table is therefore
// read from the same checkout the code under test lives in.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("the repository root could not be resolved: %v", err)
	}
	if _, err = os.Stat(filepath.Join(root, "v2", "service", "agent", "segment_generator.go")); err != nil {
		t.Fatalf("the product sources are not under %s: %v", root, err)
	}
	return root
}

// summaryTurns are the turns one batch is summarized from.
//
// The batch has to be long enough for the product to write a segment at all:
// ten unsummarized turns are the trigger.
func summaryTurns() []Turn {
	turns := make([]Turn, 0, 5)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for index := 0; index < 5; index++ {
		turns = append(turns, Turn{
			User:        fmt.Sprintf("第 %d 轮：把阅读计划改成每次读 41 页，另外记下练习场地的名字。", index+1),
			Assistant:   fmt.Sprintf("第 %d 轮的回答：按新的页数走，场地先按原来的来。", index+1),
			UserAt:      at.Add(time.Duration(index*2) * time.Minute),
			AssistantAt: at.Add(time.Duration(index*2+1) * time.Minute),
		})
	}
	return turns
}

// overBudgetSegment is a summary that does not fit the window it is read
// under.
//
// The window is 450 tokens when the assistant's products are recorded, and a
// point is rendered on one line, so twenty points of this length are well
// past it.
//
// Every point cites the learner's own first message: a point that says the
// learner said something has to cite a message the learner wrote, and one
// message can carry several facts.
func overBudgetSegment() string {
	points := make([]string, 0, 20)
	for index := 0; index < 20; index++ {
		points = append(points, fmt.Sprintf(
			`{"text":"第 %d 条：学习者把阅读计划改成每次读 41 页，练习场地改到白鹭馆，这一轮把来源、时间与被取代的旧值都记下来。","evidence_message_ids":[1]}`,
			index+1))
	}
	return `{"schema_version":1,"segment":{"topic":"阅读计划","points":[` + strings.Join(points, ",") + `]}}`
}

// fittingSegment is a summary that fits the window.
func fittingSegment() string {
	return `{"schema_version":1,"segment":{"topic":"阅读计划","points":[` +
		`{"text":"学习者把阅读计划改成每次读 41 页","evidence_message_ids":[1]}]}}`
}

// The summary that did not fit is written again, and the second answer is the
// one that is kept.
//
// The run is the evaluation's own: the role table comes from the product's
// sources, the request goes over the evaluation's HTTP boundary, and the
// stand-in answers it. What the test reads back is what the run spent and
// what the run wrote.
func TestTheSummaryThatDidNotFitIsAskedForAgainAndTheSecondAnswerIsKept(t *testing.T) {
	t.Setenv(summaryAssistantContentEnv, "on")
	upstream := newStandInUpstream(t, []standInAnswer{
		{content: overBudgetSegment()},
		{content: fittingSegment()},
	})
	store := newLocalLayeredStore(t)

	run, err := RunLayeredSummaryInStore(context.Background(), repositoryRoot(t),
		"rewrite-succeeds", "on", summaryTurns(), "", nil,
		upstream.base(), "stand-in", "on", store)
	if err != nil {
		t.Fatalf("run the layered summary: %v", err)
	}

	// The provider was asked twice: once for the summary, once for a shorter
	// one. The first answer did not fit and was not kept.
	if upstream.count() != 2 {
		t.Fatalf("the provider was asked %d times, want 2", upstream.count())
	}
	first, _, _ := upstream.request(0)
	second, secondUser, _ := upstream.request(1)
	if first == "" || second == "" {
		t.Fatal("a request carried no system prompt")
	}
	if first != second {
		t.Fatalf("the rewrite changed the system prompt:\nfirst:  %q\nsecond: %q", first, second)
	}
	if !strings.Contains(secondUser, "Write it again, shorter") {
		t.Fatalf("the second request did not ask for a shorter summary: %q", secondUser)
	}

	// The segment that was kept is the shorter one.
	if len(run.Segments) != 1 {
		t.Fatalf("segments = %d, want 1: %+v", len(run.Segments), run.Segments)
	}
	if run.Segments[0].Points != 1 {
		t.Fatalf("the segment that was kept is not the shorter one: %+v", run.Segments[0])
	}
	if !strings.Contains(run.Segments[0].Content, "41 页") {
		t.Fatalf("the segment that was kept does not carry the second answer: %q", run.Segments[0].Content)
	}

	// Every call was classified, and the rewrite is the summary's role and
	// not a role of its own.
	//
	// The evaluation refuses a request it cannot classify, so a call that
	// reached the provider at all was classified; the point of the check is
	// that the classification is the right one and not merely a present one.
	classified := 0
	for _, call := range run.Calls {
		if !strings.HasPrefix(call.Method, "http:") {
			continue
		}
		classified++
		if call.Role != "summary" {
			t.Fatalf("call %d was classified as %q, want the summary's role", classified, call.Role)
		}
	}
	if classified != 2 {
		t.Fatalf("the boundary saw %d calls, want 2", classified)
	}

	// The run's own records of the calls: one summary and one rewrite.
	if len(run.SegmentCalls) != 2 {
		t.Fatalf("the run recorded %d calls, want 2: %+v", len(run.SegmentCalls), run.SegmentCalls)
	}
	if run.SegmentCalls[0].Purpose != "summary" || run.SegmentCalls[1].Purpose != "rewrite" {
		t.Fatalf("the calls are not one summary and one rewrite: %+v", run.SegmentCalls)
	}
	if run.SegmentCalls[0].Outcome != "rejected" || run.SegmentCalls[1].Outcome != "ok" {
		t.Fatalf("the outcomes are not what happened: %+v", run.SegmentCalls)
	}
}

// Every call the run made was classified, and a call that failed is still
// there.
//
// The evaluation refuses a request it cannot classify, so a role of "unknown"
// is not a label: it is a request that was turned away. The run's own
// telemetry check fails on one, and so does this test.
func TestEveryCallTheRunMadeWasClassifiedAndTheFailedOneIsStillThere(t *testing.T) {
	t.Setenv(summaryAssistantContentEnv, "on")
	upstream := newStandInUpstream(t, []standInAnswer{
		{content: overBudgetSegment()},
		{status: http.StatusInternalServerError},
	})
	store := newLocalLayeredStore(t)

	run, err := RunLayeredSummaryInStore(context.Background(), repositoryRoot(t),
		"the-call-failed", "on", summaryTurns(), "", nil,
		upstream.base(), "stand-in", "on", store)
	if err != nil {
		t.Fatalf("run the layered summary: %v", err)
	}

	// Both calls were seen at the provider boundary, and both were the
	// summary's: the rewrite is the same role, not a new one.
	calls := 0
	for _, call := range run.Calls {
		if !strings.HasPrefix(call.Method, "http:") {
			continue
		}
		calls++
	}
	if calls != 2 {
		t.Fatalf("the boundary saw %d calls, want 2", calls)
	}

	// The generator's own records: the first answer was refused, the second
	// call failed.
	if len(run.SegmentCalls) != 2 {
		t.Fatalf("the run recorded %d calls, want 2: %+v", len(run.SegmentCalls), run.SegmentCalls)
	}
	if run.SegmentCalls[0].Outcome != "rejected" {
		t.Fatalf("the first call is not the refused one: %+v", run.SegmentCalls)
	}
	if run.SegmentCalls[1].Outcome != "failed" {
		t.Fatalf("the failed call is not kept as a failure: %+v", run.SegmentCalls)
	}
	if run.SegmentCalls[1].Reason == "" {
		t.Fatalf("the failed call has no reason: %+v", run.SegmentCalls)
	}
}

// A summary that never fits is a failure, and the batch that failed wrote
// nothing and moved nothing forward.
//
// The run reads the coverage from the batches that produced a segment, so a
// batch that failed leaves a hole rather than a watermark that runs past the
// turns no segment covers.
func TestTheSummaryThatNeverFitsIsAFailureAndNothingIsWritten(t *testing.T) {
	t.Setenv(summaryAssistantContentEnv, "on")
	upstream := newStandInUpstream(t, []standInAnswer{
		{content: overBudgetSegment()},
		{content: overBudgetSegment()},
		{content: overBudgetSegment()},
	})
	store := newLocalLayeredStore(t)

	run, err := RunLayeredSummaryInStore(context.Background(), repositoryRoot(t),
		"never-fits", "on", summaryTurns(), "", nil,
		upstream.base(), "stand-in", "on", store)
	if err != nil {
		t.Fatalf("run the layered summary: %v", err)
	}

	// The batch was asked three times and not a fourth: the first answer and
	// the two rewrites.
	if upstream.count() != 3 {
		t.Fatalf("the provider was asked %d times, want 3", upstream.count())
	}

	// Nothing was written, and the coverage did not move.
	if len(run.Segments) != 0 {
		t.Fatalf("a batch that failed wrote %d segments: %+v", len(run.Segments), run.Segments)
	}
	if run.Coverage.CoveredMessageID != 0 {
		t.Fatalf("the coverage moved to %d although no segment was written", run.Coverage.CoveredMessageID)
	}
	if len(run.Holes) != 1 {
		t.Fatalf("the uncovered stretch is not recorded as a hole: %+v", run.Holes)
	}
	if run.Failures.Segments != 1 {
		t.Fatalf("the failed batch was not counted: %+v", run.Failures)
	}

	// The calls are still there: three of them, one summary and two rewrites,
	// and none of them a success.
	if len(run.SegmentCalls) != 3 {
		t.Fatalf("the run recorded %d calls, want 3: %+v", len(run.SegmentCalls), run.SegmentCalls)
	}
	for index, call := range run.SegmentCalls {
		if call.Outcome == "ok" {
			t.Fatalf("call %d is a success although nothing fit: %+v", index, run.SegmentCalls)
		}
	}

	// And the product's own counters say the same thing, once each.
	assertCounter(t, run, "summary_segment_model_calls_total", 3)
	assertCounter(t, run, "summary_segment_call_failures_total", 3)
	assertCounter(t, run, "summary_segment_compressed_total", 2)
	assertCounter(t, run, "summary_segment_batch_split_total", 0)
	assertLabelledCounter(t, run, "summary_segment_failed_total{mode=on,reason=over_budget}", 1)
}

// The counters and the calls agree, and nothing is counted twice.
//
// A summary that was written again is a rewrite, not a split, and a refused
// answer is a call that was spent - not a call that was never made.
func TestTheCountersAndTheCallsAgreeAndNothingIsCountedTwice(t *testing.T) {
	t.Setenv(summaryAssistantContentEnv, "on")
	upstream := newStandInUpstream(t, []standInAnswer{
		{content: overBudgetSegment()},
		{content: fittingSegment()},
	})
	store := newLocalLayeredStore(t)

	run, err := RunLayeredSummaryInStore(context.Background(), repositoryRoot(t),
		"counters-agree", "on", summaryTurns(), "", nil,
		upstream.base(), "stand-in", "on", store)
	if err != nil {
		t.Fatalf("run the layered summary: %v", err)
	}

	// Two calls were made, two are counted, and neither is a split.
	assertCounter(t, run, "summary_segment_model_calls_total", 2)
	assertCounter(t, run, "summary_segment_batch_split_total", 0)
	assertCounter(t, run, "summary_segment_batch_split_calls_total", 0)
	// One summary was written again, so one call was a rewrite.
	assertCounter(t, run, "summary_segment_compressed_total", 1)
	// The refused answer was a call that was spent and did not survive.
	assertCounter(t, run, "summary_segment_call_failures_total", 1)
	// The batch itself succeeded.
	assertCounter(t, run, "summary_segment_failed_total", 0)
}

// An answer that came back empty is a call that was spent and did not
// survive, and it is not the same thing as a summary that did not fit.
func TestTheEmptyAnswerIsACallThatFailed(t *testing.T) {
	t.Setenv(summaryAssistantContentEnv, "on")
	upstream := newStandInUpstream(t, []standInAnswer{
		{content: ""},
	})
	store := newLocalLayeredStore(t)

	run, err := RunLayeredSummaryInStore(context.Background(), repositoryRoot(t),
		"the-answer-was-empty", "on", summaryTurns(), "", nil,
		upstream.base(), "stand-in", "on", store)
	if err != nil {
		t.Fatalf("run the layered summary: %v", err)
	}
	if upstream.count() != 1 {
		t.Fatalf("the provider was asked %d times, want 1", upstream.count())
	}
	if len(run.SegmentCalls) != 1 {
		t.Fatalf("the run recorded %d calls, want 1: %+v", len(run.SegmentCalls), run.SegmentCalls)
	}
	if run.SegmentCalls[0].Outcome != "failed" {
		t.Fatalf("the empty answer is not a failed call: %+v", run.SegmentCalls)
	}
	if run.SegmentCalls[0].Reason == "" {
		t.Fatalf("the failed call has no reason of its own: %+v", run.SegmentCalls)
	}
	assertCounter(t, run, "summary_segment_model_calls_total", 1)
	assertCounter(t, run, "summary_segment_call_failures_total", 1)
	if len(run.Segments) != 0 {
		t.Fatalf("a batch that failed wrote %d segments: %+v", len(run.Segments), run.Segments)
	}
}

// assertCounter is what one of the product's own counters says the run did.
//
// The counters are read as the difference between the state before the run
// and the state after it, so a counter that did not move is not in the
// difference at all: a run that did not split a batch has no split count,
// and that is zero.
func assertCounter(t *testing.T, run LayeredRun, name string, want int64) {
	t.Helper()
	got, ok := run.Totals[name]
	if !ok {
		got = 0
	}
	if got != want {
		t.Fatalf("the counter %s = %d, want %d", name, got, want)
	}
}

// assertLabelledCounter is what one of the product's counters with labels
// says the run did.
func assertLabelledCounter(t *testing.T, run LayeredRun, name string, want uint64) {
	t.Helper()
	got, ok := run.Counters[name]
	if !ok {
		got = 0
	}
	if got != want {
		t.Fatalf("the counter %s = %d, want %d", name, got, want)
	}
}
