package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/responder"
	"github.com/xiaopang456/GopherLearn/internal/summary"
)

// These tests sit at the conversation service because that is where a refresh is decided
// and where a turn's history is read. The claims worth protecting are about behaviour:
// what actually made a model call, what actually reached the model, and which strategy a
// migrated rule was allowed to change.

func newSummaryAssistant() *fixedResponder {
	return &fixedResponder{answer: "回答", respondRequest: make(chan responder.Request, 4)}
}

// answered reports whether the turn reached the answer model, and drains the record so
// the next turn starts clean.
func answered(assistant *fixedResponder) bool {
	select {
	case <-assistant.respondRequest:
		return true
	default:
		return false
	}
}

// seedLargePairs writes complete exchanges with `runes` of text each, so a session can be
// over the token figure without being over the message count.
func seedLargePairs(t *testing.T, store *repository.InMemoryRepository, userID, sessionID string, count, runes int) {
	t.Helper()
	ctx := context.Background()
	body := strings.Repeat("记", runes)
	for index := 1; index <= count; index++ {
		for _, entry := range []struct{ role, content string }{
			{roleUser, fmt.Sprintf("%s问%d", body, index)},
			{roleAssistant, fmt.Sprintf("%s答%d", body, index)},
		} {
			if _, err := store.AppendMessage(ctx, userID, sessionID,
				&domain.Message{Role: entry.role, Content: entry.content}); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// TestTokenThresholdIsTheAgentsOnly is the scope check for the migrated trigger. The same
// session - five pairs, ten messages, far past the token figure and below the twelve the
// project has always used - must summarize on a learning-agent turn and must not on a
// chat turn. The agent gets the second way a refresh becomes due; chat keeps the one it
// had, because "adopt the agent's context budget" does not mean "make every strategy pay
// for a new kind of model call".
func TestTokenThresholdIsTheAgentsOnly(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		mode     domain.SessionMode
		strategy domain.AnswerStrategy
		want     int
	}{
		{name: "chat", mode: domain.SessionModeChat, want: 0},
		{name: "knowledge", mode: domain.SessionModeChat, want: 0},
		{name: "agent", mode: domain.SessionModeLearningAgent, want: 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			summarizer := &scriptedSummarizer{}
			assistant := newSummaryAssistant()
			service, store, userID, sessionID := newSummaryFixture(t, assistant, summarizer, testCase.mode)
			seedLargePairs(t, store, userID, sessionID, 5, 2000)
			if _, err := service.Send(context.Background(), userID, sessionID, "新问题",
				domain.KnowledgeOptions{}); err != nil {
				t.Fatal(err)
			}
			if summarizer.count() != testCase.want {
				t.Fatalf("%d summarizing calls on a %s turn, want %d",
					summarizer.count(), testCase.name, testCase.want)
			}
			if testCase.want == 0 {
				return
			}
			request := summarizer.requestAt(0)
			if len(request.Turns) == 0 || len(request.Turns)%2 != 0 {
				t.Fatalf("the refresh compressed %d messages, want a non-empty whole prefix",
					len(request.Turns))
			}
			if request.Turns[0].Content == "" {
				t.Fatal("the material reaching the summarizer was empty")
			}
		})
	}
}

func TestShortSessionPassesNeitherThreshold(t *testing.T) {
	summarizer := &scriptedSummarizer{}
	assistant := newSummaryAssistant()
	service, store, userID, sessionID := newSummaryFixture(t, assistant, summarizer, domain.SessionModeChat)
	seedPairs(t, store, userID, sessionID, 5)
	if _, err := service.Send(context.Background(), userID, sessionID, "新问题", domain.KnowledgeOptions{}); err != nil {
		t.Fatal(err)
	}
	if summarizer.count() != 0 {
		t.Fatalf("%d calls for a short conversation, want neither threshold passed", summarizer.count())
	}
}

// TestAgentTurnReadsCandidatesAndChatReadsTheWindow is the boundary between the two window
// rules. The agent gets the candidate set and the budget trims it once, where the whole
// prompt is visible; a chat turn keeps the message and rune limits it always had. Giving
// the agent both rules would put two owners on one transcript, and moving chat onto the
// token rule would be a change to chat made in passing.
func TestAgentTurnReadsCandidatesAndChatReadsTheWindow(t *testing.T) {
	long := strings.Repeat("经", 2000)
	history := []responder.Turn{
		{Role: roleUser, Content: long}, {Role: roleAssistant, Content: long},
		{Role: roleUser, Content: long}, {Role: roleAssistant, Content: long},
		{Role: roleUser, Content: long}, {Role: roleAssistant, Content: long},
	}
	runeTotal := func(turns []responder.Turn) int {
		total := 0
		for _, turn := range turns {
			total += len([]rune(turn.Content))
		}
		return total
	}

	// Chat: the stored window rule is unchanged, on the same content.
	chatStored := make([]domain.Message, 0, len(history))
	for _, turn := range history {
		chatStored = append(chatStored, domain.Message{Role: turn.Role, Content: turn.Content})
	}
	kept := recentTurns(chatStored, ContextBudget{}.Normalize())
	if got := runeTotal(kept); got > DefaultContextMaxRunes {
		t.Fatalf("the chat window carried %d runes over a limit of %d", got, DefaultContextMaxRunes)
	}
	if len(kept) != 2 {
		t.Fatalf("the chat window kept %d messages, want exactly the newest whole pair", len(kept))
	}

	// Agent: the service hands over candidates; the budget in contextwindow is what
	// decides what reaches the model, and the agent-level ceilings are pinned in
	// internal/agent/context_window_test.go.
	agentAssistant := newSummaryAssistant()
	agentService, agentStore, agentUser, agentSession := newSummaryFixture(t, agentAssistant,
		&scriptedSummarizer{}, domain.SessionModeLearningAgent)
	for _, turn := range history {
		if _, err := agentStore.AppendMessage(context.Background(), agentUser, agentSession,
			&domain.Message{Role: turn.Role, Content: turn.Content}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := agentService.Send(context.Background(), agentUser, agentSession, "新问题",
		domain.KnowledgeOptions{}); err != nil {
		t.Fatal(err)
	}
	received := lastRequest(t, agentAssistant)
	if got, want := runeTotal(received.History), runeTotal(history); got != want {
		t.Fatalf("the agent received %d runes from a session holding %d: the service pre-trimmed",
			got, want)
	}
	if len(received.History) > agentService.context.Window.HistoryFetchLimit {
		t.Fatalf("the agent received %d candidates over the read limit of %d",
			len(received.History), agentService.context.Window.HistoryFetchLimit)
	}
}

// TestSummaryFailureCoolsTheSessionDownAndDegrades is the source project's product
// semantics, restored: a summary is an optimization over history that is still on disk,
// so a due refresh that fails leaves the turn on the last good summary plus the raw
// messages after its watermark, and the failed attempt opens the session's cooldown.
// The cooldown is still the point - what changed is that a degraded summary no longer
// ends the turn.
func TestSummaryFailureCoolsTheSessionDownAndDegrades(t *testing.T) {
	summarizer := &scriptedSummarizer{err: errors.New("the summarizing model refused")}
	assistant := newSummaryAssistant()
	service, store, userID, sessionID := newSummaryFixture(t, assistant, summarizer,
		domain.SessionModeLearningAgent)
	seedPairs(t, store, userID, sessionID, 6)

	// First turn: a refresh is due and fails, the cooldown opens, and the turn is
	// still answered from the last good summary.
	if _, err := service.Send(context.Background(), userID, sessionID, "第一问",
		domain.KnowledgeOptions{}); err != nil {
		t.Fatalf("a due refresh that failed ended the turn: %v", err)
	}
	if !service.summaryGuard.coolingDown(summaryGuardKey(userID, sessionID)) {
		t.Fatal("a failed refresh opened no cooldown")
	}
	if summarizer.count() != 1 {
		t.Fatalf("%d calls, want the one that failed", summarizer.count())
	}

	// Second turn inside the cooldown: no call is made and the turn is answered
	// anyway - the cooldown skips the optimization, not the conversation.
	if _, err := service.Send(context.Background(), userID, sessionID, "第二问",
		domain.KnowledgeOptions{}); err != nil {
		t.Fatalf("the cooldown stopped the turn instead of the refresh: %v", err)
	}
	if summarizer.count() != 1 {
		t.Fatalf("%d calls, want no second call while the session cools down", summarizer.count())
	}
	if !answered(assistant) {
		t.Fatal("the degraded turn was not answered")
	}
}

// TestChatKeepsRetryingAfterAFailure is the other half of the cooldown's scope: a chat
// session that failed to summarize retries on the next turn, exactly as it did before the
// cooldown existed. A failure that silenced every strategy would be a new outage nobody
// asked for.
func TestChatKeepsRetryingAfterAFailure(t *testing.T) {
	summarizer := &scriptedSummarizer{err: errors.New("the summarizing model refused")}
	assistant := newSummaryAssistant()
	service, store, userID, sessionID := newSummaryFixture(t, assistant, summarizer, domain.SessionModeChat)
	seedPairs(t, store, userID, sessionID, 6)

	if _, err := service.Send(context.Background(), userID, sessionID, "第一问",
		domain.KnowledgeOptions{}); err == nil {
		t.Fatal("a failed refresh let the chat turn answer")
	}
	if summarizer.count() != 1 {
		t.Fatalf("%d calls, want the one that failed", summarizer.count())
	}
	if service.summaryGuard.coolingDown(summaryGuardKey(userID, sessionID)) {
		t.Fatal("a chat failure opened a cooldown this strategy never had")
	}
	if _, err := service.Send(context.Background(), userID, sessionID, "第二问",
		domain.KnowledgeOptions{}); err == nil {
		t.Fatal("the second turn answered over a refresh that was due and unwritten")
	}
	if summarizer.count() != 2 {
		t.Fatalf("%d calls, want the chat turn to have attempted the refresh again", summarizer.count())
	}
}

func TestCooldownExpiresAndIsScopedToOneSession(t *testing.T) {
	summarizer := &scriptedSummarizer{err: errors.New("refused")}
	assistant := newSummaryAssistant()
	service, store, userID, sessionID := newSummaryFixture(t, assistant, summarizer,
		domain.SessionModeLearningAgent)
	seedPairs(t, store, userID, sessionID, 6)
	if _, err := service.Send(context.Background(), userID, sessionID, "第一问",
		domain.KnowledgeOptions{}); err != nil {
		t.Fatalf("a failed refresh ended the turn: %v", err)
	}
	if !service.summaryGuard.coolingDown(summaryGuardKey(userID, sessionID)) {
		t.Fatal("a failed refresh opened no cooldown")
	}

	// A different session of the same user is unaffected: the cooldown is about a
	// conversation that already failed, not about the user.
	other, err := store.CreateSession(context.Background(), &domain.Session{
		UserID: userID, Title: "另一会话", Mode: domain.SessionModeLearningAgent,
	})
	if err != nil {
		t.Fatal(err)
	}
	seedPairs(t, store, userID, other.ID, 6)
	if _, err := service.Send(context.Background(), userID, other.ID, "另一会话的问题",
		domain.KnowledgeOptions{}); errors.Is(err, ErrSummaryCoolingDown) {
		t.Fatal("one session's failure cooled a different session down")
	}

	// Once the window passes the entry is forgotten rather than kept, so the session gets
	// to discover that the model recovered.
	service.summaryGuard.cooldowns[summaryGuardKey(userID, sessionID)] = time.Now().Add(-time.Second)
	if service.summaryGuard.coolingDown(summaryGuardKey(userID, sessionID)) {
		t.Fatal("an expired cooldown still held the session")
	}
	if _, err := service.Send(context.Background(), userID, sessionID, "第三问",
		domain.KnowledgeOptions{}); errors.Is(err, ErrSummaryCoolingDown) {
		t.Fatalf("an expired cooldown still refused the refresh: %v", err)
	}
}

// A learning-agent turn nearly out of time must not spend what is left on the summary:
// the refresh is an optimization over history that is still stored, the answer is not.
func TestLateTurnSkipsTheSummaryStageWithoutCallingTheModel(t *testing.T) {
	summarizer := &scriptedSummarizer{}
	assistant := newSummaryAssistant()
	service, store, userID, sessionID := newSummaryFixture(t, assistant, summarizer,
		domain.SessionModeLearningAgent)
	seedPairs(t, store, userID, sessionID, 6)

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	time.Sleep(2 * time.Millisecond)
	_, err := service.Send(ctx, userID, sessionID, "新问题", domain.KnowledgeOptions{})
	if !errors.Is(err, ErrSummaryStageSkipped) && !errors.Is(err, context.DeadlineExceeded) &&
		!errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want a skipped stage or the deadline itself", err)
	}
	if summarizer.count() != 0 {
		t.Fatalf("%d calls from a turn that had no time for one", summarizer.count())
	}
	// A skip that never attempted anything must not extend the cooldown: the next turn
	// may well have room, and reading lateness as failure would suppress it for nothing.
	if service.summaryGuard.coolingDown(summaryGuardKey(userID, sessionID)) {
		t.Fatal("a skipped stage cooled the session down as if a model had refused")
	}
}

// TestLateChatTurnKeepsItsOwnTiming checks the same lateness against a chat turn: no stage
// budget is imposed that this strategy never had, and the refresh is asked on the caller's
// context rather than a shorter one built here.
func TestLateChatTurnKeepsItsOwnTiming(t *testing.T) {
	var (
		seenDeadline time.Time
		hadDeadline  bool
	)
	summarizer := &deadlineObservingSummarizer{observe: func(ctx context.Context) {
		seenDeadline, hadDeadline = ctx.Deadline()
	}}
	assistant := newSummaryAssistant()
	service, store, userID, sessionID := newSummaryFixture(t, assistant, summarizer, domain.SessionModeChat)
	service.agentTurnDeadline = 30 * time.Second
	seedPairs(t, store, userID, sessionID, 6)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := service.Send(ctx, userID, sessionID, "新问题", domain.KnowledgeOptions{}); err != nil {
		t.Fatal(err)
	}
	if summarizer.count() != 1 {
		t.Fatalf("%d calls, want the chat refresh to have run", summarizer.count())
	}
	if hadDeadline && time.Until(seenDeadline) < 55*time.Second {
		t.Fatalf("a chat refresh was squeezed to %v of the caller's minute", time.Until(seenDeadline))
	}
}

// The summary and the agent answer spend one budget. A refresh bounded only by its own
// timeout can consume most of a turn and leave the reply to expire, which is the failure
// the reference project measured and named.
func TestAgentTurnSharesOneDeadlineBetweenSummaryAndAnswer(t *testing.T) {
	var (
		seenDeadline time.Time
		hadDeadline  bool
	)
	summarizer := &deadlineObservingSummarizer{observe: func(ctx context.Context) {
		seenDeadline, hadDeadline = ctx.Deadline()
	}}
	assistant := newSummaryAssistant()
	service, store, userID, sessionID := newSummaryFixture(t, assistant, summarizer,
		domain.SessionModeLearningAgent)
	service.agentTurnDeadline = 30 * time.Second
	seedPairs(t, store, userID, sessionID, 6)

	if _, err := service.Send(context.Background(), userID, sessionID, "新问题",
		domain.KnowledgeOptions{}); err != nil {
		t.Fatal(err)
	}
	if !hadDeadline {
		t.Fatal("an agent turn refreshed its summary with no turn deadline in force")
	}
	// The stage is bound shorter than the turn it lives inside: 8s of a 30s budget, and
	// capped again by the share the policy allows. That is what sharing a clock looks
	// like from the inside, and what a refresh bounded only by its own timeout would
	// not have.
	remaining := time.Until(seenDeadline)
	if remaining > service.summaryPolicy.StageBudget {
		t.Fatalf("the refresh saw %v, more than the %v stage budget drawn from the turn's 30s",
			remaining, service.summaryPolicy.StageBudget)
	}
	if remaining <= 0 {
		t.Fatalf("the refresh saw an already-expired deadline: %v", remaining)
	}
}

// deadlineObservingSummarizer records the context it was handed, which is the only way to
// see whether the stage and the turn share a clock.
type deadlineObservingSummarizer struct {
	observe func(context.Context)
	err     error
	calls   int
}

func (s *deadlineObservingSummarizer) count() int { return s.calls }

func (s *deadlineObservingSummarizer) Summarize(ctx context.Context, request summary.Request) (summary.Result, error) {
	s.calls++
	if s.observe != nil {
		s.observe(ctx)
	}
	if request.Controls.StageDeadline {
		if _, ok := ctx.Deadline(); !ok {
			return summary.Result{}, errors.New("a migrated turn arrived with no stage deadline")
		}
	} else if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < 30*time.Second {
		return summary.Result{}, fmt.Errorf("a legacy turn was squeezed to %v", time.Until(deadline))
	}
	if s.err != nil {
		return summary.Result{}, s.err
	}
	return summary.Result{Content: fmt.Sprintf("摘要第%d版", s.calls)}, nil
}

// untimedClock marks a call that arrived with no deadline at all, which a fake cannot
// distinguish from a deadline of the same length by any honest means.
const untimedClock = time.Duration(-1)

// stageBoundedSummarizer stands in for a summarizing model that keeps going past the
// share of the turn it was given: it answers only when the context it was handed ends.
// That makes a stage timeout something the test causes on purpose rather than a race
// against the wall clock, and it also records the clock each call actually saw.
//
// worstCase bounds the wait for the case where no deadline is imposed at all. A caller
// that never times out is itself one of the bugs being tested for, so the fake reports an
// error instead of hanging the suite.
type stageBoundedSummarizer struct {
	mu        sync.Mutex
	calls     int
	saw       []time.Duration
	worstCase time.Duration
}

func (f *stageBoundedSummarizer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *stageBoundedSummarizer) clockAt(index int) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	if index >= len(f.saw) {
		return untimedClock
	}
	return f.saw[index]
}

func (f *stageBoundedSummarizer) Summarize(ctx context.Context, _ summary.Request) (summary.Result, error) {
	f.mu.Lock()
	remaining := untimedClock
	if deadline, ok := ctx.Deadline(); ok {
		remaining = time.Until(deadline)
	}
	f.calls++
	f.saw = append(f.saw, remaining)
	worstCase := f.worstCase
	f.mu.Unlock()

	select {
	case <-ctx.Done():
		return summary.Result{}, ctx.Err()
	case <-time.After(worstCase):
		return summary.Result{}, errors.New("the summarizing call was handed no deadline to obey")
	}
}

// walkawaySummarizer hangs up the way a learner does: it cancels the turn from inside the
// summarizing call - which is the only moment a test can cancel the caller while the
// refresh is genuinely in flight - and then reports the cancellation.
type walkawaySummarizer struct {
	mu       sync.Mutex
	calls    int
	walkAway func()
}

func (f *walkawaySummarizer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *walkawaySummarizer) Summarize(ctx context.Context, _ summary.Request) (summary.Result, error) {
	f.mu.Lock()
	first := f.calls == 0
	f.calls++
	walkAway := f.walkAway
	f.mu.Unlock()
	if first && walkAway != nil {
		walkAway()
		return summary.Result{}, context.Canceled
	}
	return summary.Result{Content: "摘要"}, nil
}

// commitClockObserver records the deadline in force when a summary is written. The stage
// budget is drawn to bound a model call, so a commit that inherited it would be a local
// write scheduled by a clock meant for a provider.
type commitClockObserver struct {
	repository.Repository
	mu        sync.Mutex
	commitSaw []time.Duration
}

func (o *commitClockObserver) CommitSessionSummary(ctx context.Context, value *domain.SessionSummary,
	expectVersion int64,
) (*domain.SessionSummary, error) {
	o.mu.Lock()
	remaining := untimedClock
	if deadline, ok := ctx.Deadline(); ok {
		remaining = time.Until(deadline)
	}
	o.commitSaw = append(o.commitSaw, remaining)
	o.mu.Unlock()
	return o.Repository.CommitSessionSummary(ctx, value, expectVersion)
}

func (o *commitClockObserver) clocks() []time.Duration {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]time.Duration(nil), o.commitSaw...)
}

// TestSummaryStageTimeoutIsRecordedAsASummaryFailure is the attribution rule: a refresh
// that ran past its share of the turn while the learner was still waiting is evidence
// about summarizing, so it opens the cooldown, and the very next turn must say so without
// spending a second call. Before this rule the stage deadline looked like a caller who
// had left, so a slow model cost one more call per turn forever.
func TestSummaryStageTimeoutIsRecordedAsASummaryFailure(t *testing.T) {
	policy := summary.Policy{
		StageBudget:     20 * time.Millisecond,
		Timeout:         time.Minute,
		FailureCooldown: time.Hour,
	}
	summarizer := &stageBoundedSummarizer{worstCase: 500 * time.Millisecond}
	assistant := newSummaryAssistant()
	service, store, userID, sessionID := newSummaryFixtureWithPolicy(t, assistant, summarizer,
		domain.SessionModeLearningAgent, policy)
	service.agentTurnDeadline = 30 * time.Second
	seedPairs(t, store, userID, sessionID, 6)

	// The learner is patient and the turn has minutes left; only the stage is short.
	caller, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := service.Send(caller, userID, sessionID, "第一问", domain.KnowledgeOptions{})
	if err != nil {
		t.Fatalf("a stage timeout degraded the turn instead of ending it: %v", err)
	}
	if caller.Err() != nil {
		t.Errorf("the caller was treated as gone, but nothing about this turn was a cancellation")
	}
	if got := summarizer.clockAt(0); got <= 0 || got > policy.StageBudget {
		t.Fatalf("the summarizing model saw %v, outside the %v stage budget drawn from the turn",
			got, policy.StageBudget)
	}
	if !service.summaryGuard.coolingDown(summaryGuardKey(userID, sessionID)) {
		t.Fatal("a stage that timed out over a waiting caller opened no cooldown")
	}
	if !answered(assistant) {
		t.Fatal("the turn whose summary timed out was not answered from the last good summary")
	}

	// The next turn meets the cooldown and makes no call at all: the total stays at one,
	// which is the whole point of recording this failure.
	if _, err := service.Send(context.Background(), userID, sessionID, "第二问",
		domain.KnowledgeOptions{}); err != nil {
		t.Fatalf("err = %v, want the cooldown to skip the refresh, not the turn", err)
	}
	if got := summarizer.count(); got != 1 {
		t.Fatalf("%d summarizing calls, want the one that timed out and no second attempt", got)
	}
}

// TestCallerCancellationLeavesNoCooldown is the other side of the same attribution: when
// the learner hangs up, nothing has been learned about the summarizing model, so the
// session must not be silenced and the next valid turn must still be able to try.
func TestCallerCancellationLeavesNoCooldown(t *testing.T) {
	policy := summary.Policy{
		StageBudget:     20 * time.Millisecond,
		Timeout:         time.Minute,
		FailureCooldown: time.Hour,
	}
	caller, cancel := context.WithCancel(context.Background())
	defer cancel()
	summarizer := &walkawaySummarizer{walkAway: cancel}
	assistant := newSummaryAssistant()
	service, store, userID, sessionID := newSummaryFixtureWithPolicy(t, assistant, summarizer,
		domain.SessionModeLearningAgent, policy)
	service.agentTurnDeadline = 30 * time.Second
	seedPairs(t, store, userID, sessionID, 6)

	_, err := service.Send(caller, userID, sessionID, "第一问", domain.KnowledgeOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the caller's cancellation returned as itself", err)
	}
	if errors.Is(err, ErrSummaryStageTimedOut) || errors.Is(err, ErrSummaryCoolingDown) {
		t.Fatalf("err = %v, a caller who left must not be reported as a summarizing failure", err)
	}
	if service.summaryGuard.coolingDown(summaryGuardKey(userID, sessionID)) {
		t.Fatal("a cancellation opened a cooldown: the user leaving was read as the model failing")
	}

	// A later turn from a caller that is still there gets its attempt, and succeeds.
	if _, err := service.Send(context.Background(), userID, sessionID, "第二问",
		domain.KnowledgeOptions{}); err != nil {
		t.Fatalf("the next valid turn was refused: %v", err)
	}
	if got := summarizer.count(); got != 2 {
		t.Fatalf("%d summarizing calls, want the cancellation plus one fresh attempt", got)
	}
	if !answered(assistant) {
		t.Fatal("the turn that followed a cancellation never reached the answer model")
	}
}

// TestSummaryCommitKeepsTheCallersClock bounds the stage to the model call and nothing
// else. A write to the project's own store is neither slow nor external, and inheriting a
// deadline drawn for a provider would fail the commit - and so the whole turn - for a
// reason that has nothing to do with the database.
func TestSummaryCommitKeepsTheCallersClock(t *testing.T) {
	policy := summary.Policy{
		StageBudget:     500 * time.Millisecond,
		Timeout:         time.Minute,
		FailureCooldown: time.Hour,
	}
	inner := repository.NewInMemoryRepository()
	store := &commitClockObserver{Repository: inner}
	user, err := inner.CreateUser(context.Background(), &domain.User{Email: "owner@example.com", DisplayName: "Owner"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := inner.CreateSession(context.Background(), &domain.Session{
		UserID: user.ID, Title: "会话", Mode: domain.SessionModeLearningAgent,
	})
	if err != nil {
		t.Fatal(err)
	}
	seedPairs(t, inner, user.ID, session.ID, 6)

	summarizer := &deadlineObservingSummarizer{}
	service := NewMessageService(store, newSummaryAssistant(),
		WithCollectionAccess(openAccess{}),
		WithRollingSummary(summarizer, policy),
		WithAgentTurnDeadline(30*time.Second),
	)
	if _, err := service.Send(context.Background(), user.ID, session.ID, "新问题",
		domain.KnowledgeOptions{}); err != nil {
		t.Fatal(err)
	}

	clocks := store.clocks()
	if len(clocks) != 1 {
		t.Fatalf("%d commits, want the one this turn made", len(clocks))
	}
	// The commit runs inside the turn's 30s budget, only milliseconds after the model
	// answered. Anything near the 500ms stage budget means the provider's clock was
	// handed to a local write.
	if got := clocks[0]; got == untimedClock || got < 20*time.Second {
		t.Fatalf("the summary commit ran on a clock with %v left, want the turn's own 30s", clocks[0])
	}
}

// TestChatAndKnowledgeTakeNeitherTheStageBudgetNorTheCooldown keeps the migrated timing
// rules on the path they were adopted for. Both legacy strategies are given the same short
// stage budget and long cooldown a migrated turn uses, and must show none of it: the
// summarizing call runs on the caller's own clock, a failure costs no silence, and the next
// turn retries as it always did.
func TestChatAndKnowledgeTakeNeitherTheStageBudgetNorTheCooldown(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		mode      domain.SessionMode
		knowledge domain.KnowledgeOptions
	}{
		{name: "chat", mode: domain.SessionModeChat, knowledge: domain.KnowledgeOptions{}},
		{name: "knowledge", mode: domain.SessionModeChat,
			knowledge: domain.KnowledgeOptions{Enabled: true, CollectionIDs: []string{"col-1"}}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			policy := summary.Policy{
				StageBudget:     20 * time.Millisecond,
				Timeout:         time.Minute,
				FailureCooldown: time.Hour,
			}
			var saw time.Duration
			summarizer := &deadlineObservingSummarizer{
				err:     errors.New("the summarizing model refused"),
				observe: func(ctx context.Context) { saw = deadlineRemaining(ctx) },
			}
			assistant := newSummaryAssistant()
			service, store, userID, sessionID := newSummaryFixtureWithPolicy(t, assistant, summarizer,
				testCase.mode, policy)
			service.agentTurnDeadline = 30 * time.Second
			seedPairs(t, store, userID, sessionID, 6)

			caller, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			_, err := service.Send(caller, userID, sessionID, "第一问", testCase.knowledge)
			if errors.Is(err, ErrSummaryCoolingDown) || errors.Is(err, ErrSummaryStageTimedOut) {
				t.Fatalf("err = %v, want the failure this strategy has always returned", err)
			}
			if !errors.Is(err, ErrInternal) {
				t.Fatalf("err = %v, want a refused refresh to fail the turn", err)
			}
			if saw < 55*time.Second {
				t.Fatalf("the summarizing call saw %v of a minute-long caller, want the caller's own clock", saw)
			}
			if service.summaryGuard.coolingDown(summaryGuardKey(userID, sessionID)) {
				t.Fatal("a legacy failure opened a cooldown this strategy never had")
			}
			if _, err := service.Send(caller, userID, sessionID, "第二问", testCase.knowledge); !errors.Is(err, ErrInternal) {
				t.Fatalf("err = %v, want the second turn to attempt the refresh again", err)
			}
			if got := summarizer.count(); got != 2 {
				t.Fatalf("%d summarizing calls, want one per turn with no silence in between", got)
			}
		})
	}
}

// deadlineRemaining reads a context's clock the way the fakes below do: untimedClock when
// no deadline is in force.
func deadlineRemaining(ctx context.Context) time.Duration {
	if deadline, ok := ctx.Deadline(); ok {
		return time.Until(deadline)
	}
	return untimedClock
}
