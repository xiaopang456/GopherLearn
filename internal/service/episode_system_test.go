package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/retrieval"
)

// The episodic system's offline acceptance. Everything here runs on the
// in-memory repository, the process-local view and the deterministic embedder:
// no provider, no Redis, no database. The model double is a real
// ToolChatModel-shaped fake, so the tests drive the same generate -> verify ->
// persist -> index -> recall path production runs, not a shortcut around it.

// fakeEpisodeModel is the model boundary for the suite. Calls that carry the
// projection tool are answered with a publish_episode tool call built from the
// payload it received; calls without tools are the support verifier, and the
// verdict is configurable per statement.
type fakeEpisodeModel struct {
	mu    sync.Mutex
	calls int
	// failProjection makes the next n projection calls fail.
	failProjection int
	// emptyProjection makes the model publish a synopsis with no assertions.
	emptyProjection bool
}

func (m *fakeEpisodeModel) record() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	return m.calls
}

func (m *fakeEpisodeModel) Complete(_ context.Context, request model.Request) (model.Completion, error) {
	m.record()
	return model.Completion{Content: `{"verdict":"entailed"}`, FinishReason: model.FinishStop}, nil
}

func (m *fakeEpisodeModel) Stream(_ context.Context, request model.Request, _ model.DeltaHandler) (model.Completion, error) {
	return m.Complete(context.Background(), request)
}

func (m *fakeEpisodeModel) CompleteWithTools(_ context.Context, request model.Request) (model.Completion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.failProjection > 0 {
		m.failProjection--
		return model.Completion{}, model.ErrUnavailable
	}
	// The payload is the second message: system prompt, then the encoded view.
	var payload episodeProjectionPayload
	if err := json.Unmarshal([]byte(request.Messages[1].Content), &payload); err != nil {
		return model.Completion{}, fmt.Errorf("fake model could not read its payload: %w", err)
	}
	var cited []string
	for _, message := range payload.Messages {
		if message.Role == "user" {
			cited = append(cited, message.ID)
		}
	}
	document := domain.EpisodeDocument{
		Synopsis:      "用户确定了 JSON 编解码器的选型。",
		Topics:        []domain.EpisodeTopic{{Key: domain.NormalizeEpisodeTopicKey("JSON 编解码器"), Label: "JSON 编解码器"}},
		SourceHandles: []string{},
	}
	// The fake echoes only the handles the server actually supplied: a handle
	// it invented would be refused by the same source boundary production runs.
	for _, message := range payload.Messages {
		document.SourceHandles = append(document.SourceHandles, message.SourceHandles...)
	}
	if !m.emptyProjection && len(cited) > 0 {
		document.Decisions = []domain.EpisodeEvidence{{
			Text:               "用户决定采用 JSON 编解码器",
			EvidenceMessageIDs: cited,
		}}
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return model.Completion{}, err
	}
	return model.Completion{
		ToolCalls:    []model.ToolCall{{ID: "call-1", Name: episodeToolName, Arguments: string(encoded)}},
		FinishReason: model.FinishToolCalls,
	}, nil
}

func (m *fakeEpisodeModel) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// newEpisodeTestSystem wires a full EpisodeSystem on offline doubles.
func newEpisodeTestSystem(t *testing.T) (*EpisodeSystem, *repository.InMemoryRepository, *fakeEpisodeModel) {
	t.Helper()
	store := repository.NewInMemoryRepository()
	chat := &fakeEpisodeModel{}
	system, err := NewEpisodeSystem(store, chat, retrieval.NewDeterministicEmbedder(0),
		NewMemoryEpisodeVectorIndex(), EpisodeSystemConfig{
			Worker: EpisodeWorkerConfig{
				WorkerCount: 1, QueueSize: 8, ScanInterval: 5 * time.Millisecond,
				ReconcileInterval: 20 * time.Millisecond, TaskTimeout: 2 * time.Second,
				Lease: 5 * time.Second, RecoveryInterval: 20 * time.Millisecond,
				RetryDelay: 5 * time.Millisecond, MaxAttempts: 3,
			},
			QuietWindow:     0,
			MaxMessages:     512,
			MaxInputTokens:  12000,
			MaxOutputTokens: 1800,
		})
	if err != nil {
		t.Fatalf("assemble episode system: %v", err)
	}
	return system, store, chat
}

type episodeScenario struct {
	userID      string
	otherUserID string
	sessionA    string
	sessionB    string
	decided     string
	answered    string
	decidedB    string
	answeredB   string
}

// seedEpisodeScenario builds two users with one conversation each: session A
// decides on a codec, session B only asks. The scenario is what the offline
// acceptance reads back through the whole system.
func seedEpisodeScenario(t *testing.T, store repository.Repository) episodeScenario {
	t.Helper()
	ctx := context.Background()
	user, err := store.CreateUser(ctx, &domain.User{Email: "owner@example.com", DisplayName: "Owner"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.CreateUser(ctx, &domain.User{Email: "other@example.com", DisplayName: "Other"})
	if err != nil {
		t.Fatal(err)
	}
	sessionA, err := store.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "编解码器", Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}
	sessionB, err := store.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "新会话", Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}
	decided, err := store.AppendMessage(ctx, user.ID, sessionA.ID, &domain.Message{Role: "user", Content: "我决定用 JSON 编解码器。"})
	if err != nil {
		t.Fatal(err)
	}
	answered, err := store.AppendMessage(ctx, user.ID, sessionA.ID, &domain.Message{Role: "assistant", Content: "好的，按 JSON 编解码器实现。"})
	if err != nil {
		t.Fatal(err)
	}
	decidedB, err := store.AppendMessage(ctx, user.ID, sessionB.ID, &domain.Message{Role: "user", Content: "我想了解更多。"})
	if err != nil {
		t.Fatal(err)
	}
	answeredB, err := store.AppendMessage(ctx, user.ID, sessionB.ID, &domain.Message{Role: "user", Content: "我还想问一个遗留问题。"})
	if err != nil {
		t.Fatal(err)
	}
	return episodeScenario{
		userID: user.ID, otherUserID: other.ID,
		sessionA: sessionA.ID, sessionB: sessionB.ID,
		decided: decided.ID, answered: answered.ID,
		decidedB: decidedB.ID, answeredB: answeredB.ID,
	}
}

// waitFor polls until the condition holds or the deadline passes, so the tests
// drive the real background loops without sleeping on their tickers.
func waitFor(t *testing.T, what string, deadline time.Duration, condition func() bool) {
	t.Helper()
	limit := time.Now().Add(deadline)
	for time.Now().Before(limit) {
		if condition() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("%s did not happen within %v", what, deadline)
}

// waitForIndexedCard waits until the session's card exists AND its derived view
// is in place: the projection and the index write are two steps, and recall
// only answers from the second one.
func waitForIndexedCard(t *testing.T, store repository.Repository, userID, sessionID string) {
	t.Helper()
	waitFor(t, "the indexed episode card", 2*time.Second, func() bool {
		loaded, err := store.GetEpisodeBySession(t.Context(), userID, sessionID)
		return err == nil && loaded.Status == domain.EpisodeStatusActive && loaded.IndexStatus == domain.EpisodeIndexIndexed
	})
}

// TestEpisodeGenerationIsProductionReachable is the headline path: a published
// conversation is projected, persisted, indexed and recalled from another
// session of the same user, with no manual step in between.
func TestEpisodeGenerationIsProductionReachable(t *testing.T) {
	system, store, chat := newEpisodeTestSystem(t)
	scenario := seedEpisodeScenario(t, store)
	worker := system.Worker()
	if err := worker.Start(t.Context()); err != nil {
		t.Fatalf("start worker: %v", err)
	}
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = worker.Stop(stop)
	})

	if err := system.Publisher().PublishConversationCompleted(t.Context(), scenario.userID, scenario.sessionA, scenario.answered); err != nil {
		t.Fatalf("publish: %v", err)
	}
	var card *domain.Episode
	waitForIndexedCard(t, store, scenario.userID, scenario.sessionA)
	card, _ = store.GetEpisodeBySession(t.Context(), scenario.userID, scenario.sessionA)
	if card.Status != domain.EpisodeStatusActive || card.IndexStatus != domain.EpisodeIndexIndexed {
		t.Fatalf("card state = %s/%s, want active/indexed", card.Status, card.IndexStatus)
	}
	if card.Version != 1 || card.SourceLastMessageID != scenario.answered {
		t.Fatalf("card watermark = %s version %d", card.SourceLastMessageID, card.Version)
	}

	// The same user's other session recalls it. The question names the topic,
	// which is what the exact channel matches on.
	cards, err := system.RecallForTurn(t.Context(), scenario.userID, scenario.sessionB, scenario.decidedB, "之前关于 JSON 编解码器 的决定")
	if err != nil {
		t.Fatalf("recall from session B: %v", err)
	}
	if len(cards) != 1 || len(cards[0].Decisions) != 1 {
		t.Fatalf("recall from session B = %+v, want the decision card", cards)
	}
	if got := cards[0].Decisions[0].MessageIDs; len(got) != 1 || got[0] != scenario.decided {
		t.Fatalf("decision cites %v, want the stored question %s", got, scenario.decided)
	}
	if err := system.AuthorizeDisplayed(t.Context(), scenario.userID, scenario.sessionB, scenario.decidedB, []EpisodeDisplayedAuthorization{{EpisodeID: cards[0].ID, EpisodeVersion: cards[0].Version, MessageIDs: cards[0].Decisions[0].MessageIDs}}); err != nil {
		t.Fatalf("authorize displayed card: %v", err)
	}

	// The read-back path returns the learner's own words for the cited id, and
	// only for it.
	messages, refusals, err := system.ReadHistoryForTurn(t.Context(), scenario.userID, scenario.sessionB, scenario.decidedB, []string{scenario.decided}, 0)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(messages) != 1 || messages[0].MessageID != scenario.decided ||
		!strings.Contains(messages[0].Content, "JSON 编解码器") {
		t.Fatalf("read back = %+v refusals %v", messages, refusals)
	}
	if _, _, err := system.ReadHistoryForTurn(t.Context(), scenario.userID, scenario.sessionB, scenario.decidedB, []string{scenario.answeredB}, 0); err != nil {
		t.Fatalf("read back uncited errored: %v", err)
	} else {
		// The assistant message of session B was never cited by a card, so the
		// refusal vocabulary must name it rather than serve it.
		if _, refusals, _ := system.ReadHistoryForTurn(t.Context(), scenario.userID, scenario.sessionB, scenario.decidedB, []string{scenario.answeredB}, 0); len(refusals) == 0 {
			t.Fatal("an uncited message id was readable")
		}
	}
	_ = chat
}

// TestEpisodeRecallRefusesUnrelatedQuestionsAndForeignUsers pins the two
// boundaries around recall: an unrelated question is not forced to use
// history, and another user's card is invisible.
func TestEpisodeRecallRefusesUnrelatedQuestionsAndForeignUsers(t *testing.T) {
	system, store, _ := newEpisodeTestSystem(t)
	scenario := seedEpisodeScenario(t, store)
	worker := system.Worker()
	if err := worker.Start(t.Context()); err != nil {
		t.Fatalf("start worker: %v", err)
	}
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = worker.Stop(stop)
	})
	if err := system.Publisher().PublishConversationCompleted(t.Context(), scenario.userID, scenario.sessionA, scenario.answered); err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitForIndexedCard(t, store, scenario.userID, scenario.sessionA)

	// An unrelated question: the plan refuses the read outright.
	cards, err := system.RecallForTurn(t.Context(), scenario.userID, scenario.sessionB, scenario.decidedB, "今天晚饭吃什么比较好")
	if err != nil {
		t.Fatalf("unrelated recall: %v", err)
	}
	if len(cards) != 0 {
		t.Fatalf("unrelated question recalled %+v", cards)
	}
	// Another user: the recall fails closed. The history probe is the first read,
	// and a store that refuses a foreign user is not evidence that the session has
	// no history - the read errors instead of serving an honest-looking empty
	// result, so nothing about another user's data can be inferred either way.
	if _, err := system.RecallForTurn(t.Context(), scenario.otherUserID, scenario.sessionB, scenario.decidedB, "之前关于 JSON 编解码器 的决定"); err == nil {
		t.Fatal("a foreign user's recall was served rather than refused")
	}
}

// TestEpisodePublishIsIdempotentAndAdvances pins the debounce and the
// watermark: a repeated publish generates nothing new, and a genuinely new
// message re-projects the same row to a higher version.
func TestEpisodePublishIsIdempotentAndAdvances(t *testing.T) {
	system, store, chat := newEpisodeTestSystem(t)
	scenario := seedEpisodeScenario(t, store)
	worker := system.Worker()
	if err := worker.Start(t.Context()); err != nil {
		t.Fatalf("start worker: %v", err)
	}
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = worker.Stop(stop)
	})
	publisher := system.Publisher()
	if err := publisher.PublishConversationCompleted(t.Context(), scenario.userID, scenario.sessionA, scenario.answered); err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitForIndexedCard(t, store, scenario.userID, scenario.sessionA)
	callsAfterFirst := chat.callCount()

	// Re-delivered publish at the same watermark: debounce, no model work.
	if err := publisher.PublishConversationCompleted(t.Context(), scenario.userID, scenario.sessionA, scenario.answered); err != nil {
		t.Fatalf("republish: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if chat.callCount() != callsAfterFirst {
		t.Fatalf("a repeated publish spent %d extra model call(s)", chat.callCount()-callsAfterFirst)
	}

	// A new message advances the watermark: the same row is re-projected to
	// version 2 and the view is rewritten in place.
	followup, err := store.AppendMessage(t.Context(), scenario.userID, scenario.sessionA, &domain.Message{Role: "assistant", Content: "第二轮完成。"})
	if err != nil {
		t.Fatalf("append follow-up: %v", err)
	}
	if err := publisher.PublishConversationCompleted(t.Context(), scenario.userID, scenario.sessionA, followup.ID); err != nil {
		t.Fatalf("publish advanced: %v", err)
	}
	waitFor(t, "version 2", 2*time.Second, func() bool {
		loaded, err := store.GetEpisodeBySession(t.Context(), scenario.userID, scenario.sessionA)
		return err == nil && loaded.Version >= 2
	})
	if chat.callCount() <= callsAfterFirst {
		t.Fatal("the advanced publish did not reach the model again")
	}
}

// TestEpisodeFailuresAreBounded pins the retry ceiling: a model that always
// fails spends exactly MaxAttempts projection attempts and no more, so a broken
// provider cannot loop forever.
func TestEpisodeFailuresAreBounded(t *testing.T) {
	system, store, chat := newEpisodeTestSystem(t)
	scenario := seedEpisodeScenario(t, store)
	chat.failProjection = 100
	worker := system.Worker()
	if err := worker.Start(t.Context()); err != nil {
		t.Fatalf("start worker: %v", err)
	}
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = worker.Stop(stop)
	})
	if err := system.Publisher().PublishConversationCompleted(t.Context(), scenario.userID, scenario.sessionA, scenario.answered); err != nil {
		t.Fatalf("publish: %v", err)
	}
	// Projection calls = attempts, verifier never runs; wait for the ceiling.
	waitFor(t, "the attempt ceiling", 2*time.Second, func() bool {
		return chat.callCount() >= 3
	})
	time.Sleep(80 * time.Millisecond)
	if chat.callCount() > 3 {
		t.Fatalf("a failing projection ran %d times, want exactly MaxAttempts", chat.callCount())
	}
	if _, err := store.GetEpisodeBySession(t.Context(), scenario.userID, scenario.sessionA); !errors.Is(err, repository.ErrEpisodeNotFound) {
		t.Fatalf("a failed projection wrote %v", err)
	}
}

// TestEpisodeDeleteInvalidatesRecallAndCitation pins the deletion boundary:
// after the source session goes, the card is neither recalled nor citable.
func TestEpisodeDeleteInvalidatesRecallAndCitation(t *testing.T) {
	system, store, _ := newEpisodeTestSystem(t)
	scenario := seedEpisodeScenario(t, store)
	worker := system.Worker()
	if err := worker.Start(t.Context()); err != nil {
		t.Fatalf("start worker: %v", err)
	}
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = worker.Stop(stop)
	})
	if err := system.Publisher().PublishConversationCompleted(t.Context(), scenario.userID, scenario.sessionA, scenario.answered); err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitForIndexedCard(t, store, scenario.userID, scenario.sessionA)
	cards, err := system.RecallForTurn(t.Context(), scenario.userID, scenario.sessionB, scenario.decidedB, "之前关于 JSON 编解码器 的决定")
	if err != nil || len(cards) != 1 {
		t.Fatalf("precondition failed: cards=%v err=%v", cards, err)
	}

	// Mirror the production delete path: capture the card id while the session
	// is still readable, delete the session (its transaction now removes the
	// card and job rows), then drop the view rows best-effort.
	deletedCard, err := store.GetEpisodeBySession(t.Context(), scenario.userID, scenario.sessionA)
	if err != nil {
		t.Fatalf("read the card before delete: %v", err)
	}
	if err := store.DeleteSession(t.Context(), scenario.userID, scenario.sessionA); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	// The delete transaction already removed the card and job rows; a store
	// that left them behind would be a cascade this assertion catches.
	if remaining, err := store.DeleteEpisodesBySession(t.Context(), scenario.userID, scenario.sessionA); err != nil || len(remaining) != 0 {
		t.Fatalf("the session delete left %v rows behind (err=%v)", remaining, err)
	}
	system.DropSession(t.Context(), scenario.userID, scenario.sessionA, []string{deletedCard.ID})

	cards, err = system.RecallForTurn(t.Context(), scenario.userID, scenario.sessionB, scenario.decidedB, "之前关于 JSON 编解码器 的决定")
	if err != nil {
		t.Fatalf("recall after delete: %v", err)
	}
	if len(cards) != 0 {
		t.Fatalf("a deleted session's card survived recall: %+v", cards)
	}
	// The turn's ledger was rebuilt by the recall above, so a stale citation
	// from the deleted card has nothing to stand on.
	if _, refusals, _ := system.ReadHistoryForTurn(t.Context(), scenario.userID, scenario.sessionB, scenario.decidedB, []string{scenario.decided}, 0); len(refusals) == 0 {
		t.Fatal("a deleted card's citation still authorised a read-back")
	}
}

// TestEpisodeVersionBumpInvalidatesOldCitations pins the version boundary: a
// re-projected card invalidates the old version's read-back rights even when
// the message itself still exists.
func TestEpisodeVersionBumpInvalidatesOldCitations(t *testing.T) {
	system, store, _ := newEpisodeTestSystem(t)
	scenario := seedEpisodeScenario(t, store)
	worker := system.Worker()
	if err := worker.Start(t.Context()); err != nil {
		t.Fatalf("start worker: %v", err)
	}
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = worker.Stop(stop)
	})
	if err := system.Publisher().PublishConversationCompleted(t.Context(), scenario.userID, scenario.sessionA, scenario.answered); err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitForIndexedCard(t, store, scenario.userID, scenario.sessionA)
	oldVersion, err := store.GetEpisodeBySession(t.Context(), scenario.userID, scenario.sessionA)
	if err != nil {
		t.Fatal(err)
	}
	cards, err := system.RecallForTurn(t.Context(), scenario.userID, scenario.sessionB, scenario.decidedB, "之前关于 JSON 编解码器 的决定")
	if err != nil || len(cards) != 1 {
		t.Fatalf("precondition failed: %v %v", cards, err)
	}
	if err := system.AuthorizeDisplayed(t.Context(), scenario.userID, scenario.sessionB, scenario.decidedB, []EpisodeDisplayedAuthorization{{EpisodeID: cards[0].ID, EpisodeVersion: cards[0].Version, MessageIDs: cards[0].Decisions[0].MessageIDs}}); err != nil {
		t.Fatalf("authorize displayed card: %v", err)
	}

	// The card moves on out-of-band (a re-projection the turn did not see).
	advanced, err := store.AppendMessage(t.Context(), scenario.userID, scenario.sessionA, &domain.Message{Role: "user", Content: "补充：还要支持流式。"})
	if err != nil {
		t.Fatal(err)
	}
	bumped := *oldVersion
	bumped.SourceLastMessageID = advanced.ID
	bumped.SearchText += " 支持流式"
	if err := store.SaveEpisode(t.Context(), &bumped, oldVersion.Version); err != nil {
		t.Fatalf("bump version: %v", err)
	}
	if _, refusals, _ := system.ReadHistoryForTurn(t.Context(), scenario.userID, scenario.sessionB, scenario.decidedB, []string{scenario.decided}, 0); len(refusals) == 0 || refusals[0].Reason != "episode_version_mismatch" {
		t.Fatalf("the old version's citation survived a version bump: %v", refusals)
	}
}

// TestEpisodeAbstentionAndWatermarkShortCircuit pins the two "do nothing
// again" rules: an assertion-less session records an abstained row, and a
// repeat at a covered watermark never re-opens the model.
func TestEpisodeAbstentionAndWatermarkShortCircuit(t *testing.T) {
	system, store, chat := newEpisodeTestSystem(t)
	scenario := seedEpisodeScenario(t, store)
	chat.emptyProjection = true
	worker := system.Worker()
	if err := worker.Start(t.Context()); err != nil {
		t.Fatalf("start worker: %v", err)
	}
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = worker.Stop(stop)
	})
	if err := system.Publisher().PublishConversationCompleted(t.Context(), scenario.userID, scenario.sessionB, scenario.answeredB); err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitFor(t, "the abstained row", 2*time.Second, func() bool {
		loaded, err := store.GetEpisodeBySession(t.Context(), scenario.userID, scenario.sessionB)
		return err == nil && loaded.Status == domain.EpisodeStatusAbstained
	})
	loaded, _ := store.GetEpisodeBySession(t.Context(), scenario.userID, scenario.sessionB)
	if loaded.AbstainReason != domain.AbstainNoDurableAssertion {
		t.Fatalf("abstain reason = %s", loaded.AbstainReason)
	}
	calls := chat.callCount()
	// Repeating the same target short-circuits without the model.
	if _, err := system.Projector().Project(t.Context(), scenario.userID, scenario.sessionB, scenario.answeredB); err != nil {
		t.Fatalf("repeat projection: %v", err)
	}
	if chat.callCount() != calls {
		t.Fatalf("a covered watermark spent %d extra model call(s)", chat.callCount()-calls)
	}
	// And the abstained row is invisible to recall.
	cards, err := system.RecallForTurn(t.Context(), scenario.userID, scenario.sessionA, scenario.decided, "关于会话经历了什么")
	if err != nil {
		t.Fatalf("recall abstained: %v", err)
	}
	if len(cards) != 0 {
		t.Fatalf("an abstained row leaked into recall: %+v", cards)
	}
}

// TestEpisodeWarmStartRebuildsTheView pins the restart case for the
// process-local view: a fresh index warmed from the store recalls the same
// card the original view served.
func TestEpisodeWarmStartRebuildsTheView(t *testing.T) {
	system, store, _ := newEpisodeTestSystem(t)
	scenario := seedEpisodeScenario(t, store)
	if err := system.Publisher().PublishConversationCompleted(t.Context(), scenario.userID, scenario.sessionA, scenario.answered); err != nil {
		t.Fatalf("publish: %v", err)
	}
	worker := system.Worker()
	if err := worker.Start(t.Context()); err != nil {
		t.Fatalf("start worker: %v", err)
	}
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = worker.Stop(stop)
	})
	waitForIndexedCard(t, store, scenario.userID, scenario.sessionA)

	// A restarted process: a fresh view over the same authority. The rows
	// survived; the process-local view did not, which is exactly the gap the
	// warm start exists to close.
	restarted, err := NewEpisodeSystem(store, &fakeEpisodeModel{}, retrieval.NewDeterministicEmbedder(0),
		NewMemoryEpisodeVectorIndex(), EpisodeSystemConfig{
			Worker: EpisodeWorkerConfig{
				WorkerCount: 1, QueueSize: 8, ScanInterval: 5 * time.Millisecond,
				ReconcileInterval: 20 * time.Millisecond, TaskTimeout: 2 * time.Second,
				Lease: 5 * time.Second, RecoveryInterval: 20 * time.Millisecond,
				RetryDelay: 5 * time.Millisecond, MaxAttempts: 3,
			},
			QuietWindow:     0,
			MaxMessages:     512,
			MaxInputTokens:  12000,
			MaxOutputTokens: 1800,
		})
	if err != nil {
		t.Fatalf("assemble restarted system: %v", err)
	}
	warmed, err := restarted.WarmStart(t.Context())
	if err != nil {
		t.Fatalf("warm start: %v", err)
	}
	if warmed == 0 {
		t.Fatal("the warm start read nothing")
	}
	cards, err := restarted.RecallForTurn(t.Context(), scenario.userID, scenario.sessionB, scenario.decidedB, "之前关于 JSON 编解码器 的决定")
	if err != nil || len(cards) != 1 {
		t.Fatalf("recall after warm start = %v, want the card (%v)", cards, err)
	}
}

// TestEpisodeAuthorizationGrantsOnlyTheDisplayedIntersection pins the last
// narrowing step: one stored assertion citing two message ids, of which the
// tool showed exactly one. The shown id reads back; its hidden sibling is
// refused; an id nobody stored is refused even if the caller names it. The
// stored card is only ever read - the filtering never rewrites it, which the
// second recall below proves by returning the assertion unchanged.
func TestEpisodeAuthorizationGrantsOnlyTheDisplayedIntersection(t *testing.T) {
	system, store, chat := newEpisodeTestSystem(t)
	worker := system.Worker()
	if err := worker.Start(t.Context()); err != nil {
		t.Fatalf("start worker: %v", err)
	}
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = worker.Stop(stop)
	})
	ctx := context.Background()
	user, err := store.CreateUser(ctx, &domain.User{Email: "intersection@example.com", DisplayName: "Owner"})
	if err != nil {
		t.Fatal(err)
	}
	sessionA, err := store.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "编解码器", Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.AppendMessage(ctx, user.ID, sessionA.ID, &domain.Message{Role: "user", Content: "我决定用 JSON 编解码器。"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.AppendMessage(ctx, user.ID, sessionA.ID, &domain.Message{Role: "user", Content: "补充：还要支持流式输出。"})
	if err != nil {
		t.Fatal(err)
	}
	answer, err := store.AppendMessage(ctx, user.ID, sessionA.ID, &domain.Message{Role: "assistant", Content: "好的。"})
	if err != nil {
		t.Fatal(err)
	}
	sessionB, err := store.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "新会话", Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}
	questionB, err := store.AppendMessage(ctx, user.ID, sessionB.ID, &domain.Message{Role: "user", Content: "当时怎么定的？"})
	if err != nil {
		t.Fatal(err)
	}
	_ = chat
	if err := system.Publisher().PublishConversationCompleted(ctx, user.ID, sessionA.ID, answer.ID); err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitForIndexedCard(t, store, user.ID, sessionA.ID)

	// The fake projects one decision assertion that cites both user messages.
	cards, err := system.RecallForTurn(ctx, user.ID, sessionB.ID, questionB.ID, "之前关于 JSON 编解码器 的决定")
	if err != nil || len(cards) != 1 || len(cards[0].Decisions) != 1 || len(cards[0].Decisions[0].MessageIDs) != 2 {
		t.Fatalf("recall = %+v (%v), want one card citing both ids", cards, err)
	}
	if cards[0].Decisions[0].MessageIDs[0] != first.ID || cards[0].Decisions[0].MessageIDs[1] != second.ID {
		t.Fatalf("assertion cites %v, want [%s %s]", cards[0].Decisions[0].MessageIDs, first.ID, second.ID)
	}

	// Rendering showed only the first id. The grant must be exactly that id:
	// not the whole assertion, and not the fabricated one riding along.
	err = system.AuthorizeDisplayed(ctx, user.ID, sessionB.ID, questionB.ID, []EpisodeDisplayedAuthorization{{
		EpisodeID: cards[0].ID, EpisodeVersion: cards[0].Version,
		MessageIDs: []string{first.ID, "msg-fabricated"},
	}})
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	messages, refusals, err := system.ReadHistoryForTurn(ctx, user.ID, sessionB.ID, questionB.ID, []string{first.ID, second.ID, "msg-fabricated"}, 0)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(messages) != 1 || messages[0].MessageID != first.ID {
		t.Fatalf("read back = %+v, want exactly the displayed id", messages)
	}
	refused := make(map[string]string, len(refusals))
	for _, refusal := range refusals {
		refused[refusal.MessageID] = refusal.Reason
	}
	if refused[second.ID] != "message_not_cited_this_turn" || refused["msg-fabricated"] != "message_not_cited_this_turn" {
		t.Fatalf("refusals = %v, want both hidden ids refused", refusals)
	}

	// The stored card is untouched: a fresh recall still shows the whole
	// assertion with both ids.
	stored, err := store.GetEpisodeBySession(ctx, user.ID, sessionA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Document.Decisions) != 1 || len(stored.Document.Decisions[0].EvidenceMessageIDs) != 2 {
		t.Fatalf("authorization rewrote the stored card: %+v", stored.Document.Decisions)
	}
	again, err := system.RecallForTurn(ctx, user.ID, sessionB.ID, questionB.ID, "之前关于 JSON 编解码器 的决定")
	if err != nil || len(again) != 1 || len(again[0].Decisions) != 1 || len(again[0].Decisions[0].MessageIDs) != 2 {
		t.Fatalf("recall after authorization = %+v (%v), want the assertion unchanged", again, err)
	}
}

// TestEpisodeDisplayedGrantsAccumulateWithinTurnAndDieWithIt pins the ledger's
// turn semantics at the displayed-authorization boundary: two searches in one
// turn each grant their own card and both grants stay live; the next turn
// inherits nothing.
func TestEpisodeDisplayedGrantsAccumulateWithinTurnAndDieWithIt(t *testing.T) {
	system, store, chat := newEpisodeTestSystem(t)
	worker := system.Worker()
	if err := worker.Start(t.Context()); err != nil {
		t.Fatalf("start worker: %v", err)
	}
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = worker.Stop(stop)
	})
	ctx := context.Background()
	user, err := store.CreateUser(ctx, &domain.User{Email: "accumulate@example.com", DisplayName: "Owner"})
	if err != nil {
		t.Fatal(err)
	}
	// Two source sessions, one codec decision each, so two searches can each
	// grant a different card.
	for index := 0; index < 2; index++ {
		session, err := store.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "编解码器", Mode: domain.SessionModeChat})
		if err != nil {
			t.Fatal(err)
		}
		question, err := store.AppendMessage(ctx, user.ID, session.ID, &domain.Message{Role: "user", Content: "我决定用 JSON 编解码器。"})
		if err != nil {
			t.Fatal(err)
		}
		answer, err := store.AppendMessage(ctx, user.ID, session.ID, &domain.Message{Role: "assistant", Content: "好的。"})
		if err != nil {
			t.Fatal(err)
		}
		if err := system.Publisher().PublishConversationCompleted(ctx, user.ID, session.ID, answer.ID); err != nil {
			t.Fatalf("publish: %v", err)
		}
		waitForIndexedCard(t, store, user.ID, session.ID)
		_ = question
	}
	sessionB, err := store.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "新会话", Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}
	turnOne, err := store.AppendMessage(ctx, user.ID, sessionB.ID, &domain.Message{Role: "user", Content: "当时怎么定的？"})
	if err != nil {
		t.Fatal(err)
	}
	turnTwo, err := store.AppendMessage(ctx, user.ID, sessionB.ID, &domain.Message{Role: "user", Content: "再讲讲细节。"})
	if err != nil {
		t.Fatal(err)
	}
	_ = chat

	// Search one of turn one: both cards come back, only one is shown and
	// granted. The granted id is the card's own first citation - the card the
	// tool showed, not a guess from session order.
	firstBatch, err := system.RecallForTurn(ctx, user.ID, sessionB.ID, turnOne.ID, "之前关于 JSON 编解码器 的决定")
	if err != nil || len(firstBatch) != 2 {
		t.Fatalf("first recall = %+v (%v), want both cards", firstBatch, err)
	}
	firstCard := firstBatch[0]
	firstGrantID := firstCard.Decisions[0].MessageIDs[0]
	if err := system.AuthorizeDisplayed(ctx, user.ID, sessionB.ID, turnOne.ID, []EpisodeDisplayedAuthorization{{
		EpisodeID: firstCard.ID, EpisodeVersion: firstCard.Version,
		MessageIDs: []string{firstGrantID},
	}}); err != nil {
		t.Fatalf("authorize first search: %v", err)
	}

	// Search two of the same turn: the ledger must keep the first search's
	// grant while adding this one. This search grants the OTHER card.
	secondBatch, err := system.RecallForTurn(ctx, user.ID, sessionB.ID, turnOne.ID, "之前关于 JSON 编解码器 的决定")
	if err != nil || len(secondBatch) != 2 {
		t.Fatalf("second recall = %+v (%v), want both cards", secondBatch, err)
	}
	var secondCard EpisodeCard
	for _, card := range secondBatch {
		if card.ID != firstCard.ID {
			secondCard = card
			break
		}
	}
	if secondCard.ID == "" {
		t.Fatal("the second search did not return the other card")
	}
	secondGrantID := secondCard.Decisions[0].MessageIDs[0]
	if secondGrantID == firstGrantID {
		t.Fatal("the two cards cite the same question; the scenario needs two distinct decisions")
	}
	if err := system.AuthorizeDisplayed(ctx, user.ID, sessionB.ID, turnOne.ID, []EpisodeDisplayedAuthorization{{
		EpisodeID: secondCard.ID, EpisodeVersion: secondCard.Version,
		MessageIDs: []string{secondGrantID},
	}}); err != nil {
		t.Fatalf("authorize second search: %v", err)
	}
	granted := []string{firstGrantID, secondGrantID}
	messages, refusals, err := system.ReadHistoryForTurn(ctx, user.ID, sessionB.ID, turnOne.ID, granted, 0)
	if err != nil {
		t.Fatalf("read back both: %v", err)
	}
	if len(messages) != 2 || len(refusals) != 0 {
		t.Fatalf("accumulated read = %+v refusals %v, want both granted questions readable", messages, refusals)
	}

	// The next turn starts its own ledger: neither grant survives it, even
	// though the same recall runs again.
	if _, err := system.RecallForTurn(ctx, user.ID, sessionB.ID, turnTwo.ID, "之前关于 JSON 编解码器 的决定"); err != nil {
		t.Fatalf("recall in the new turn: %v", err)
	}
	_, refusals, err = system.ReadHistoryForTurn(ctx, user.ID, sessionB.ID, turnTwo.ID, granted, 0)
	if err != nil {
		t.Fatalf("read back in the new turn: %v", err)
	}
	if len(refusals) != 2 {
		t.Fatalf("the new turn inherited %d grant(s): %v", 2-len(refusals), refusals)
	}
}

// TestEpisodeDisplayedAuthorizationSkipsAVersionBump pins the boundary between
// rendering and the store: a card that advances after it was recalled but
// before it is authorized grants nothing, and the read tool refuses the id
// instead of serving it from the stale grant.
func TestEpisodeDisplayedAuthorizationSkipsAVersionBump(t *testing.T) {
	system, store, chat := newEpisodeTestSystem(t)
	scenario := seedEpisodeScenario(t, store)
	worker := system.Worker()
	if err := worker.Start(t.Context()); err != nil {
		t.Fatalf("start worker: %v", err)
	}
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = worker.Stop(stop)
	})
	if err := system.Publisher().PublishConversationCompleted(t.Context(), scenario.userID, scenario.sessionA, scenario.answered); err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitForIndexedCard(t, store, scenario.userID, scenario.sessionA)
	cards, err := system.RecallForTurn(t.Context(), scenario.userID, scenario.sessionB, scenario.decidedB, "之前关于 JSON 编解码器 的决定")
	if err != nil || len(cards) != 1 {
		t.Fatalf("recall = %+v (%v)", cards, err)
	}
	oldVersion := cards[0].Version

	// The card moves on out-of-band, between the recall and the handoff.
	advanced, err := store.AppendMessage(t.Context(), scenario.userID, scenario.sessionA, &domain.Message{Role: "user", Content: "补充：还要支持流式。"})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetEpisodeBySession(t.Context(), scenario.userID, scenario.sessionA)
	if err != nil {
		t.Fatal(err)
	}
	bumped := *stored
	bumped.SourceLastMessageID = advanced.ID
	if err := store.SaveEpisode(t.Context(), &bumped, stored.Version); err != nil {
		t.Fatalf("bump version: %v", err)
	}

	// The stale displayed version grants nothing: the id reads back as
	// not-cited-this-turn rather than through the dead grant.
	if err := system.AuthorizeDisplayed(t.Context(), scenario.userID, scenario.sessionB, scenario.decidedB, []EpisodeDisplayedAuthorization{{
		EpisodeID: cards[0].ID, EpisodeVersion: oldVersion,
		MessageIDs: []string{scenario.decided},
	}}); err != nil {
		t.Fatalf("authorize must tolerate a moved card, not fail the turn: %v", err)
	}
	_, refusals, err := system.ReadHistoryForTurn(t.Context(), scenario.userID, scenario.sessionB, scenario.decidedB, []string{scenario.decided}, 0)
	if err != nil || len(refusals) == 0 {
		t.Fatalf("the stale displayed version granted a read: refusals %v err %v", refusals, err)
	}
	_ = chat
}

// errEpisodeStoreBoom is the injected store failure the partial-batch test
// reads back out of the authorization error chain.
var errEpisodeStoreBoom = errors.New("episode test: store read failure")

// flakyEpisodeStore shares the real in-memory store with the system under test
// and injects a read failure for exactly one episode id, toggled by the test.
type flakyEpisodeStore struct {
	repository.Repository
	failFor string
}

func (s *flakyEpisodeStore) GetEpisodeByID(ctx context.Context, userID, episodeID string) (*domain.Episode, error) {
	if episodeID == s.failFor {
		return nil, errEpisodeStoreBoom
	}
	return s.Repository.GetEpisodeByID(ctx, userID, episodeID)
}

// TestAuthorizeDisplayedFailureKeepsOldGrantsAndAddsNone pins the batch
// semantics of the display handoff: a store read failure anywhere in the batch
// fails the call with the original error chain, adds zero grants from that
// batch, and leaves grants the same turn legally earned earlier untouched.
func TestAuthorizeDisplayedFailureKeepsOldGrantsAndAddsNone(t *testing.T) {
	inner := repository.NewInMemoryRepository()
	wrapped := &flakyEpisodeStore{Repository: inner}
	chat := &fakeEpisodeModel{}
	system, err := NewEpisodeSystem(wrapped, chat, retrieval.NewDeterministicEmbedder(0),
		NewMemoryEpisodeVectorIndex(), EpisodeSystemConfig{
			Worker: EpisodeWorkerConfig{
				WorkerCount: 1, QueueSize: 8, ScanInterval: 5 * time.Millisecond,
				ReconcileInterval: 20 * time.Millisecond, TaskTimeout: 2 * time.Second,
				Lease: 5 * time.Second, RecoveryInterval: 20 * time.Millisecond,
				RetryDelay: 5 * time.Millisecond, MaxAttempts: 3,
			},
			QuietWindow:     0,
			MaxMessages:     512,
			MaxInputTokens:  12000,
			MaxOutputTokens: 1800,
		})
	if err != nil {
		t.Fatalf("assemble episode system: %v", err)
	}
	worker := system.Worker()
	if err := worker.Start(t.Context()); err != nil {
		t.Fatalf("start worker: %v", err)
	}
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = worker.Stop(stop)
	})
	ctx := context.Background()

	user, err := wrapped.CreateUser(ctx, &domain.User{Email: "batch-failure@example.com", DisplayName: "Owner"})
	if err != nil {
		t.Fatal(err)
	}
	sessionA, err := wrapped.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "编解码器", Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}
	first, err := wrapped.AppendMessage(ctx, user.ID, sessionA.ID, &domain.Message{Role: "user", Content: "我决定用 JSON 编解码器。"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := wrapped.AppendMessage(ctx, user.ID, sessionA.ID, &domain.Message{Role: "user", Content: "补充：还要支持流式输出。"})
	if err != nil {
		t.Fatal(err)
	}
	answer, err := wrapped.AppendMessage(ctx, user.ID, sessionA.ID, &domain.Message{Role: "assistant", Content: "好的。"})
	if err != nil {
		t.Fatal(err)
	}
	sessionB, err := wrapped.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "新会话", Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}
	questionB, err := wrapped.AppendMessage(ctx, user.ID, sessionB.ID, &domain.Message{Role: "user", Content: "当时怎么定的？"})
	if err != nil {
		t.Fatal(err)
	}
	if err := system.Publisher().PublishConversationCompleted(ctx, user.ID, sessionA.ID, answer.ID); err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitForIndexedCard(t, wrapped, user.ID, sessionA.ID)

	// First batch of the turn: one card, one shown id - a legal grant.
	cards, err := system.RecallForTurn(ctx, user.ID, sessionB.ID, questionB.ID, "之前关于 JSON 编解码器 的决定")
	if err != nil || len(cards) != 1 || len(cards[0].Decisions) != 1 {
		t.Fatalf("recall = %+v (%v)", cards, err)
	}
	cardA := cards[0]
	if err := system.AuthorizeDisplayed(ctx, user.ID, sessionB.ID, questionB.ID, []EpisodeDisplayedAuthorization{{
		EpisodeID: cardA.ID, EpisodeVersion: cardA.Version, MessageIDs: []string{first.ID},
	}}); err != nil {
		t.Fatalf("authorize the first batch: %v", err)
	}
	if got, _, err := system.ReadHistoryForTurn(ctx, user.ID, sessionB.ID, questionB.ID, []string{first.ID}, 0); err != nil || len(got) != 1 {
		t.Fatalf("precondition: the legal grant did not read back: %v %v", got, err)
	}

	// The failing batch: its first item would grant a NEW id (the second
	// assertion id) if processing survived, and its second item hits the
	// injected store failure mid-verification.
	wrapped.failFor = "ep-boom"
	batchErr := system.AuthorizeDisplayed(ctx, user.ID, sessionB.ID, questionB.ID, []EpisodeDisplayedAuthorization{
		{EpisodeID: cardA.ID, EpisodeVersion: cardA.Version, MessageIDs: []string{second.ID}},
		{EpisodeID: "ep-boom", EpisodeVersion: cardA.Version, MessageIDs: []string{first.ID}},
	})
	if !errors.Is(batchErr, errEpisodeStoreBoom) {
		t.Fatalf("batch failure = %v, want the original store error chain", batchErr)
	}

	// Zero grants from the failed batch, and the earlier grant is intact.
	_, refusals, err := system.ReadHistoryForTurn(ctx, user.ID, sessionB.ID, questionB.ID, []string{second.ID}, 0)
	if err != nil || len(refusals) != 1 || refusals[0].Reason != "message_not_cited_this_turn" {
		t.Fatalf("the failed batch leaked a grant: refusals %v err %v", refusals, err)
	}
	if got, refusals, err := system.ReadHistoryForTurn(ctx, user.ID, sessionB.ID, questionB.ID, []string{first.ID}, 0); err != nil || len(got) != 1 || len(refusals) != 0 {
		t.Fatalf("the failed batch cleared an earlier grant: got %v refusals %v err %v", got, refusals, err)
	}
}
