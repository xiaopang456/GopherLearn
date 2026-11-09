package agent

import (
	"GopherAI/common/observability"
	"GopherAI/model"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
)

// The tests of the compression budget. A summary that outgrew its budget used
// to be thrown away, and a thrown away summary is a summary that never
// updates again: the session kept its last good summary and the raw messages
// after it, and the early history fell out of the window for good.
//
// The budget is a budget, not a validity rule, so what is tested here is that
// an overlong summary is trimmed to the budget, that the trim keeps the items
// a later message still backs, and that the trim is reported instead of
// hidden.

// scriptedSummaryBody returns one fixed body, or one fixed failure.
type scriptedSummaryBody struct {
	body  string
	err   error
	calls int
}

func (g *scriptedSummaryBody) GenerateSummary(_ context.Context, _ string, _ []model.Message) (SummaryGeneration, error) {
	g.calls++
	if g.err != nil {
		return SummaryGeneration{}, g.err
	}
	return SummaryGeneration{Content: g.body}, nil
}

// bodyGenerator is the product's own generator behind a model that always
// answers with the same body. The trim happens inside the generator, so a
// test of the trim has to go through it.
func bodyGenerator(t *testing.T, body string) *ModelSummaryGenerator {
	t.Helper()
	chat := &scriptedChatModel{respond: func(_ context.Context, _ int) (*schema.Message, error) {
		return schema.AssistantMessage(body, nil), nil
	}}
	generator, err := NewModelSummaryGenerator(chat, 8000, 4000, model.DefaultSummaryBudget())
	if err != nil {
		t.Fatal(err)
	}
	return generator
}

// userSummaryMessages are messages the learner wrote. Only a learner message
// may be cited by a summary item, so the tests that cite many ids need many
// of them.
func userSummaryMessages(count int) []model.Message {
	messages := make([]model.Message, 0, count)
	for i := 1; i <= count; i++ {
		messages = append(messages, model.Message{ID: uint(i), IsUser: true, Content: fmt.Sprintf("message-%d", i)})
	}
	return messages
}

// openLoopsBody is a summary with the given number of open loops, each citing
// the learner message of the same number.
func openLoopsBody(count int) string {
	loops := make([]string, 0, count)
	for i := 1; i <= count; i++ {
		loops = append(loops, fmt.Sprintf(`{"text":"loop-%02d","evidence_message_ids":[%d]}`, i, i))
	}
	return fmt.Sprintf(`{"schema_version":2,"current_goal":"keep going","confirmed_decisions":[],"completed_items":[],"open_loops":[%s],"user_corrections":[],"key_errors":[],"references":[]}`, strings.Join(loops, ","))
}

// TestAnOverlongSummaryIsTrimmedInsteadOfRefused is the bug this change is
// about: a summary with more open loops than the budget allows used to be
// refused as a whole, and the session never compacted again.
func TestAnOverlongSummaryIsTrimmedInsteadOfRefused(t *testing.T) {
	document, truncations, err := decodeSummaryDocument(openLoopsBody(22), "", userSummaryMessages(40), model.DefaultSummaryBudget())
	if err != nil {
		t.Fatalf("an overlong summary must be trimmed, not refused: %v", err)
	}
	if len(document.OpenLoops) != model.SessionSummaryItemLimit {
		t.Fatalf("open loops = %d, want %d", len(document.OpenLoops), model.SessionSummaryItemLimit)
	}
	if len(truncations) != 1 || truncations[0].Group != model.SummaryGroupOpenLoops ||
		truncations[0].Kind != "items" || truncations[0].Kept != 20 || truncations[0].Dropped != 2 {
		t.Fatalf("truncations = %+v", truncations)
	}
	if err := document.Validate(model.DefaultSummaryBudget()); err != nil {
		t.Fatalf("the trimmed summary is not within the budget: %v", err)
	}
}

// TestTheTrimKeepsTheItemsTheLatestMessageBacks pins the rule: an item is as
// old as the message it rests on, and what survives is what a later message
// still backs. The twenty loops that cite the twenty latest messages stay.
func TestTheTrimKeepsTheItemsTheLatestMessageBacks(t *testing.T) {
	document, _, err := decodeSummaryDocument(openLoopsBody(22), "", userSummaryMessages(40), model.DefaultSummaryBudget())
	if err != nil {
		t.Fatal(err)
	}
	if len(document.OpenLoops) != 20 {
		t.Fatalf("open loops = %d, want 20", len(document.OpenLoops))
	}
	for index, item := range document.OpenLoops {
		want := fmt.Sprintf("loop-%02d", index+3)
		if item.Text != want {
			t.Fatalf("open loop %d = %q, want %q", index, item.Text, want)
		}
		if len(item.EvidenceMessageIDs) != 1 || item.EvidenceMessageIDs[0] != uint(index+3) {
			t.Fatalf("open loop %d cites %v, want [%d]", index, item.EvidenceMessageIDs, index+3)
		}
	}
}

// TestTheTrimIsReported says what was cut, so a run can show that a session
// lost something rather than only that it kept something.
func TestTheTrimIsReported(t *testing.T) {
	var logged []string
	recorder := observability.NewMemoryRecorder(func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	})
	previousRecorder := observability.Memory
	observability.Memory = recorder
	defer func() { observability.Memory = previousRecorder }()
	var written bytes.Buffer
	previousLog := log.Writer()
	log.SetOutput(&written)
	defer log.SetOutput(previousLog)

	repository := &fakeSummaryRepository{}
	generator := bodyGenerator(t, openLoopsBody(22))
	provider, err := NewRollingSummaryProvider(repository, fakeSummaryMessages{messages: userSummaryMessages(40)}, generator, DefaultSummaryConfig())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := provider.SessionSummary(context.Background(), RunContext{UserID: 1, SessionID: "s"})
	if err != nil {
		t.Fatalf("the compaction failed: %v", err)
	}
	if len(snapshot.Truncations) != 1 || snapshot.Truncations[0].Dropped != 2 {
		t.Fatalf("the snapshot does not report the trim: %+v", snapshot.Truncations)
	}
	// The trim is not written into the summary itself: the stored document is
	// the trimmed one, and the report travels beside it.
	if repository.current == nil || !strings.Contains(repository.current.Content, "loop-22") ||
		strings.Contains(repository.current.Content, "loop-01") {
		t.Fatalf("the stored summary is not the trimmed one: %+v", repository.current)
	}
	counters := recorder.Snapshot().Counters
	if counters["memory_summary_truncated_total{group=open_loops,kind=items}"] != 1 {
		t.Fatalf("the trim was not counted: %v", counters)
	}
	metric := false
	for _, line := range logged {
		if strings.Contains(line, "name=summary_truncated") && strings.Contains(line, "group=open_loops") {
			metric = true
		}
	}
	if !metric {
		t.Fatalf("the trim was not measured: %v", logged)
	}
	if !strings.Contains(written.String(), "compaction truncated") ||
		!strings.Contains(written.String(), "group=open_loops") {
		t.Fatalf("the trim was not logged: %s", written.String())
	}
}

// TestASummaryInsideTheBudgetIsByteForByteTheSame is the guard that keeps the
// two things apart: a summary that was trimmed and a summary that was not.
// Without it, a change of the cleaning order would look like a change of the
// summary.
func TestASummaryInsideTheBudgetIsByteForByteTheSame(t *testing.T) {
	body := `{"schema_version":2,"current_goal":"keep going","confirmed_decisions":[{"text":"use length prefixes","evidence_message_ids":[1]}],"completed_items":["read the guide"],"open_loops":[{"text":"write the test","evidence_message_ids":[2]}],"user_corrections":[],"key_errors":[],"references":["guide.md"]}`
	document, truncations, err := decodeSummaryDocument(body, "", userSummaryMessages(4), model.DefaultSummaryBudget())
	if err != nil {
		t.Fatal(err)
	}
	if len(truncations) != 0 {
		t.Fatalf("nothing had to be trimmed: %+v", truncations)
	}
	canonical, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":2,"current_goal":"keep going","confirmed_decisions":[{"text":"use length prefixes","evidence_message_ids":[1]}],"completed_items":["read the guide"],"open_loops":[{"text":"write the test","evidence_message_ids":[2]}],"user_corrections":[],"key_errors":[],"references":["guide.md"]}`
	if string(canonical) != want {
		t.Fatalf("the summary changed:\n got %s\nwant %s", canonical, want)
	}
}

// TestDuplicatesAreRemovedBeforeTheBudgetIsApplied is the order of the work:
// a document that only looks too big because it repeated itself is not
// trimmed at all.
func TestDuplicatesAreRemovedBeforeTheBudgetIsApplied(t *testing.T) {
	loops := make([]string, 0, 22)
	for i := 1; i <= 19; i++ {
		loops = append(loops, fmt.Sprintf(`{"text":"loop-%02d","evidence_message_ids":[%d]}`, i, i))
	}
	// The same three items again, with the same text and the same ids.
	loops = append(loops, loops[0], loops[1], loops[2])
	body := fmt.Sprintf(`{"schema_version":2,"current_goal":"keep going","confirmed_decisions":[],"completed_items":[],"open_loops":[%s],"user_corrections":[],"key_errors":[],"references":[]}`, strings.Join(loops, ","))
	document, truncations, err := decodeSummaryDocument(body, "", userSummaryMessages(40), model.DefaultSummaryBudget())
	if err != nil {
		t.Fatal(err)
	}
	if len(document.OpenLoops) != 19 {
		t.Fatalf("open loops = %d, want 19", len(document.OpenLoops))
	}
	if len(truncations) != 0 {
		t.Fatalf("nothing had to be trimmed after the duplicates went: %+v", truncations)
	}
}

// TestAPlainGroupIsTrimmedToo covers the groups that hold no citations: they
// had their own refusal, and they are trimmed the same way.
func TestAPlainGroupIsTrimmedToo(t *testing.T) {
	items := make([]string, 0, 25)
	for i := 1; i <= 25; i++ {
		items = append(items, fmt.Sprintf("%q", fmt.Sprintf("done-%02d", i)))
	}
	body := fmt.Sprintf(`{"schema_version":2,"current_goal":"keep going","confirmed_decisions":[],"completed_items":[%s],"open_loops":[],"user_corrections":[],"key_errors":[],"references":[]}`,
		strings.Join(items, ","))
	document, truncations, err := decodeSummaryDocument(body, "", userSummaryMessages(4), model.DefaultSummaryBudget())
	if err != nil {
		t.Fatalf("an overlong plain group must be trimmed, not refused: %v", err)
	}
	if len(document.CompletedItems) != model.SessionSummaryItemLimit {
		t.Fatalf("completed items = %d, want %d", len(document.CompletedItems), model.SessionSummaryItemLimit)
	}
	if len(truncations) != 1 || truncations[0].Group != model.SummaryGroupCompletedItems || truncations[0].Dropped != 5 {
		t.Fatalf("truncations = %+v", truncations)
	}
}

// TestTooManyCitedMessagesAreTrimmed covers the third refusal: the document
// as a whole may not cite more messages than the budget allows.
func TestTooManyCitedMessagesAreTrimmed(t *testing.T) {
	loops := make([]string, 0, 20)
	next := 1
	for i := 1; i <= 20; i++ {
		ids := make([]string, 0, 4)
		for j := 0; j < 4; j++ {
			ids = append(ids, fmt.Sprintf("%d", next))
			next++
		}
		loops = append(loops, fmt.Sprintf(`{"text":"loop-%02d","evidence_message_ids":[%s]}`, i, strings.Join(ids, ",")))
	}
	body := fmt.Sprintf(`{"schema_version":2,"current_goal":"keep going","confirmed_decisions":[],"completed_items":[],"open_loops":[%s],"user_corrections":[],"key_errors":[],"references":[]}`,
		strings.Join(loops, ","))
	document, truncations, err := decodeSummaryDocument(body, "", userSummaryMessages(120), model.DefaultSummaryBudget())
	if err != nil {
		t.Fatalf("too many cited messages must be trimmed, not refused: %v", err)
	}
	if cited := len(document.EvidenceMessageIDs()); cited > model.SessionSummarySourceLimit {
		t.Fatalf("the summary still cites %d messages, want at most %d", cited, model.SessionSummarySourceLimit)
	}
	if len(truncations) == 0 {
		t.Fatal("the trim was not reported")
	}
	if err := document.Validate(model.DefaultSummaryBudget()); err != nil {
		t.Fatalf("the trimmed summary is not within the budget: %v", err)
	}
}

// TestTheBudgetIsConfigurable checks that the limit of a group is the
// deployment's decision: an operator who widens the open loops keeps more of
// them, without a change of the code.
func TestTheBudgetIsConfigurable(t *testing.T) {
	budget := SummaryBudgetOf(20, 40, 64)
	document, truncations, err := decodeSummaryDocument(openLoopsBody(22), "", userSummaryMessages(40), budget)
	if err != nil {
		t.Fatal(err)
	}
	if len(document.OpenLoops) != 22 {
		t.Fatalf("open loops = %d, want 22: the wider limit was not honoured", len(document.OpenLoops))
	}
	if len(truncations) != 0 {
		t.Fatalf("nothing had to be trimmed: %+v", truncations)
	}
}

// TestATrimmedItemNoLongerAuthorisesARead is the cost of the trim, written
// down: what was cut does not authorise a read of the message it cited. The
// summary is the only thing that may open a message below its watermark, and
// a trimmed item is not in the summary any more.
func TestATrimmedItemNoLongerAuthorisesARead(t *testing.T) {
	repository := &fakeSummaryRepository{}
	generator := bodyGenerator(t, openLoopsBody(22))
	provider, err := NewRollingSummaryProvider(repository, fakeSummaryMessages{messages: userSummaryMessages(40)}, generator, DefaultSummaryConfig())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := provider.SessionSummary(context.Background(), RunContext{UserID: 1, SessionID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	grants := historyGrantsFromSummary(snapshot, snapshot.Content, "s")
	granted := make(map[uint]bool, len(grants))
	for _, grant := range grants {
		granted[grant.MessageID] = true
	}
	if granted[1] || granted[2] {
		t.Fatalf("a trimmed item still authorises its message: %v", grants)
	}
	if !granted[3] || !granted[22] {
		t.Fatalf("a kept item does not authorise its message: %v", grants)
	}
}

// TestTheSummaryPromptIsUnchanged pins the prompt. The budget is enforced by
// the product, not by an instruction to the model: a summary that says the
// limit would be a summary whose length depends on how well the model
// followed the instruction, and the comparisons that measure this change
// would no longer be about the product.
func TestTheSummaryPromptIsUnchanged(t *testing.T) {
	sum := sha256.Sum256([]byte(summarySystemPrompt))
	digest := hex.EncodeToString(sum[:])
	const frozen = "189c5ff2c15bd7584d1435062aa4856a18ad4df549a54f7306cce35d885465f7"
	if digest != frozen {
		t.Fatalf("the summary prompt changed:\n got %s\nwant %s", digest, frozen)
	}
}

// TestTheFailuresAreToldApart says which step of the compaction failed, so
// that a run can tell a failure from the cooldown it caused.
func TestTheFailuresAreToldApart(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want SummaryFailureReason
	}{
		{"the model did not answer", fmt.Errorf("generate rolling summary: %w", errors.New("upstream is unavailable")), SummaryFailureModelCall},
		{"the model ran out of time", fmt.Errorf("generate rolling summary: %w", context.DeadlineExceeded), SummaryFailureTimeout},
		{"the body could not be read", fmt.Errorf("decode rolling summary: unexpected end of JSON"), SummaryFailureParse},
		{"the body said something invalid", fmt.Errorf("rolling summary: session summary has too many evidence items"), SummaryFailureStructure},
		{"the body was too long", fmt.Errorf("rolling summary exceeds output token limit"), SummaryFailureLength},
		{"nothing was left to keep", ErrEmptySessionSummary, SummaryFailureEmpty},
		{"the cooldown held it back", ErrSummaryCoolingDown, SummaryFailureCooldown},
		{"the request had no room", ErrSummaryStageSkipped, SummaryFailureSkipped},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := SummaryFailureReasonOf(testCase.err); got != testCase.want {
				t.Fatalf("reason = %s, want %s", got, testCase.want)
			}
		})
	}
}

// TestTheCooldownStillHoldsTheCompactionBack is the other half of the report:
// a compaction the cooldown held back is not a new failure, and it must not
// spend another model call.
func TestTheCooldownStillHoldsTheCompactionBack(t *testing.T) {
	repository := &fakeSummaryRepository{current: &model.SessionSummary{UserID: 1, SessionID: "s", Content: readablePreviousSummary, CoveredMessageID: 1, Version: 1}}
	generator := &scriptedSummaryBody{err: fmt.Errorf("generate rolling summary: %w", context.DeadlineExceeded)}
	config := DefaultSummaryConfig()
	config.FailureCooldown = 30 * time.Second
	provider, err := NewRollingSummaryProvider(repository, fakeSummaryMessages{messages: userSummaryMessages(20)}, generator, config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.SessionSummary(context.Background(), RunContext{UserID: 1, SessionID: "s"}); err == nil {
		t.Fatal("the compaction should have failed")
	}
	if _, err := provider.SessionSummary(context.Background(), RunContext{UserID: 1, SessionID: "s"}); !errors.Is(err, ErrSummaryCoolingDown) {
		t.Fatalf("the second attempt = %v, want the cooldown", err)
	}
	if generator.calls != 1 {
		t.Fatalf("generator calls = %d, want 1", generator.calls)
	}
}
