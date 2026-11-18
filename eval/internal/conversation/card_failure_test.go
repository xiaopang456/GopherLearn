package conversation

// One conversation's card being refused must not void the others.
//
// The product refuses a card it cannot store - a card that cited a message it
// was never shown, a card whose answer was cut in the middle - and both
// refusals are deliberate. A preparation that stopped at the first refusal
// would throw away the conversations that were written well, and the arm
// would measure an empty set instead of a degraded one.
//
// So the refusal is recorded and the preparation goes on. Four things have to
// hold at the same time:
//
//   - one refusal does not stop the others;
//   - the refusal carries the job's OWN code, so that the reader knows which
//     stage refused the card;
//   - EVERY conversation being refused still stops, because that is not a
//     degraded card path - it is no card path at all;
//   - the refused card's own words are kept, because the ledger holds hashes
//     and a hash says that two bodies differ, not what either of them said.
//
// The third one is the guard against tolerance becoming blindness.
//
// The test drives the REAL functions: it hands the arm a database that holds
// what the product's own worker would have left there, and it runs against a
// real sandbox, because the functions read and write the database.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"GopherAI/model"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// --- the raw bodies of the card generation -----------------------------------

// cardRequestBody is the request the card writer is sent.
//
// The payload carries the id the card may reach to: it is the one number in
// the body that names a conversation, because the conversation's own id is
// not in the body at all.
func cardRequestBody(t *testing.T, target uint) []byte {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"session_title":     "本地替身会话",
		"target_message_id": target,
		"messages":          []any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"model": "synthetic",
		"messages": []any{
			map[string]any{"role": "system", "content": "known"},
			map[string]any{"role": "user", "content": string(payload)},
		},
		"tools": []any{map[string]any{"type": "function",
			"function": map[string]any{"name": cardToolName}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// The card generation's own exchange is kept whole.
//
// The ledger holds hashes, and a hash says that two bodies differ, not what
// either of them said. When the server refuses a card, the question is what
// the model actually wrote - and that is in the body, which is why the body
// is kept.
func TestTheCardGenerationKeepsItsOwnBodies(t *testing.T) {
	ledger := &ModelLedger{}
	answer := `{"choices":[{"message":{"role":"assistant","tool_calls":[{"function":{"name":"publish_session_card","arguments":"{\"title\":\"本地替身卡片\"}"}}]}}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`
	handler := modelHTTPHandler(ledger, RolePrompts{"known": "background"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, answer)
	}))
	handler.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(cardRequestBody(t, 77))))
	exchanges := ledger.CardExchanges()
	if len(exchanges) != 1 {
		t.Fatalf("the card generation was captured %d times, want 1", len(exchanges))
	}
	exchange := exchanges[0]
	if exchange.TargetMessageID != 77 {
		t.Fatalf("the exchange does not say which conversation it is about: %+v", exchange)
	}
	if !strings.Contains(exchange.Request, cardToolName) {
		t.Fatalf("the request body was not kept: %q", exchange.Request)
	}
	if !strings.Contains(exchange.Response, "本地替身卡片") {
		t.Fatalf("the answer body was not kept: %q", exchange.Response)
	}
	if exchange.HTTPStatus != 200 || exchange.At.IsZero() {
		t.Fatalf("the exchange carries no status or no time: %+v", exchange)
	}
	// The bodies carry no credential: the handler that reads them never
	// reads a header, and the key the harness uses is a placeholder.
	lowered := strings.ToLower(exchange.Request + exchange.Response)
	if strings.Contains(lowered, "authorization") || strings.Contains(lowered, "bearer ") || strings.Contains(lowered, "api_key") {
		t.Fatalf("the captured bodies carry a credential: %q", lowered)
	}
}

// Only the card generation is captured.
//
// The Episode generator is a background call too, and the summary calls are
// background calls as well. What tells the card writer apart is the TOOL it
// binds - it is the only call that asks for publish_session_card - so the
// capture does not depend on the prompt's wording.
func TestOnlyTheCardGenerationIsCaptured(t *testing.T) {
	ledger := &ModelLedger{}
	handler := modelHTTPHandler(ledger, RolePrompts{"known": "background"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	body := []byte(`{"model":"synthetic","messages":[{"role":"system","content":"known"},{"role":"user","content":"{\"decisions\":[]}"}]}`)
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body)))
	if len(ledger.CardExchanges()) != 0 {
		t.Fatalf("a call that wrote no card was captured: %+v", ledger.CardExchanges())
	}
}

// The card generation is told apart by the TOOL it binds, not by a word that
// happens to be in its prompt.
//
// The card writer's own prompt says "Call publish_session_card exactly
// once", so a body that merely MENTIONS the name is not a card generation -
// another background call that talks about the same tool must not be
// captured, or the evidence file would carry bodies that belong to nobody.
func TestTheCardGenerationIsRecognisedByItsToolTable(t *testing.T) {
	mentioned := []byte(`{"model":"synthetic","messages":[{"role":"system","content":"known"},{"role":"user","content":"please call publish_session_card"}],` +
		`"tools":[{"type":"function","function":{"name":"search_documents"}}]}`)
	if cardGenerationRequested(mentioned) {
		t.Fatal("a call that only mentions the tool name is not a card generation")
	}
	bound := []byte(`{"model":"synthetic","messages":[{"role":"system","content":"known"}],` +
		`"tools":[{"type":"function","function":{"name":"publish_session_card"}}]}`)
	if !cardGenerationRequested(bound) {
		t.Fatal("a call that binds the card tool is a card generation")
	}
	// A body whose table cannot be read must not cost the capture: the
	// request that names the tool is still a card request.
	unreadable := []byte(`{"model":"synthetic","messages":[{"role":"system","content":"known"}],` +
		`"tools":[{"type":"function","function":{"name":"publish_session_card"}}`)
	if !cardGenerationRequested(unreadable) {
		t.Fatal("a body that cannot be parsed must still be captured")
	}
}

// The exchange names the conversation it was written for.
//
// The body carries the message id the card may reach to, and that id belongs
// to exactly one conversation. Without the name a refused card would be a
// body with no owner, and the reader would have to guess which
// conversation's card the product refused.
func TestTheCardExchangeNamesItsConversation(t *testing.T) {
	projection := &ArmProjection{Targets: map[string]uint{"s-1": 11, "s-2": 22}}
	projection.CardExchanges = []CardExchange{{TargetMessageID: 22}, {TargetMessageID: 99}}
	projection.NameCardExchangeSessions()
	if projection.CardExchanges[0].SessionID != "s-2" {
		t.Fatalf("the exchange was not named: %+v", projection.CardExchanges[0])
	}
	if projection.CardExchanges[1].SessionID != "" {
		t.Fatalf("an exchange with no conversation was given one: %+v", projection.CardExchanges[1])
	}
}

// --- one refusal does not void the others ------------------------------------

// cardFailureFixture is a sandbox with a few conversations in it.
//
// Two of them have a card that was written and indexed, and one of them has
// a projection job that FAILED - the row the product's own worker leaves
// behind when it refuses a card.
func cardFailureFixture(t *testing.T, ctx context.Context, sessions []string) (*Sandbox, Snapshot) {
	t.Helper()
	s, err := NewSandbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeSandbox(s) })
	if err := s.InitProductSchema(ctx); err != nil {
		t.Fatal(err)
	}
	snapshot := Snapshot{}
	if err := (guardedStore{sandbox: s}).with(ctx, func(tx *gorm.DB) error {
		user := model.User{ID: 7, Username: "learner"}
		if err := tx.Create(&user).Error; err != nil {
			return err
		}
		for _, sessionID := range sessions {
			session := model.Session{ID: sessionID, UserID: user.ID, UserName: user.Username,
				Mode: model.SessionModeAgent, Title: "Evaluation conversation"}
			if err := tx.Create(&session).Error; err != nil {
				return err
			}
			conversation := SessionSnapshot{Session: session}
			for turn := 0; turn < 3; turn++ {
				message := model.Message{SessionID: sessionID, UserName: user.Username,
					IsUser: turn%2 == 0, Content: fmt.Sprintf("第 %d 轮的话", turn), CreatedAt: time.Now()}
				if err := tx.Create(&message).Error; err != nil {
					return err
				}
				conversation.Messages = append(conversation.Messages, message)
			}
			snapshot.Sessions = append(snapshot.Sessions, conversation)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return s, snapshot
}

// cardTargetOf is how far one conversation of the fixture reaches.
func cardTargetOf(t *testing.T, snapshot Snapshot, sessionID string) uint {
	t.Helper()
	for _, conversation := range snapshot.Sessions {
		if conversation.Session.ID != sessionID {
			continue
		}
		var target uint
		for _, message := range conversation.Messages {
			if message.ID > target {
				target = message.ID
			}
		}
		return target
	}
	t.Fatalf("the fixture holds no conversation %q", sessionID)
	return 0
}

// aWrittenCard is the row the product's worker and indexer leave behind when
// a card was written.
func aWrittenCard(userID int64, sessionID string, target uint) model.EpisodeMemory {
	return model.EpisodeMemory{
		ID: uuid.NewString(), UserID: userID, SessionID: sessionID,
		ContentKind: model.EpisodeContentSessionCard, Title: "本地替身卡片",
		SearchText: "本地替身卡片正文", TopicKeys: model.StringList{"local-stub"},
		SourceLastMessageID: target, LastMessageAt: time.Now(), Version: 1,
		Status: model.EpisodeStatusActive, IndexStatus: model.EpisodeIndexIndexed,
	}
}

// aRefusedCardJob is the row the product's worker leaves behind when it
// refused a card.
//
// The code is the card path's own: the shared worker writes the code of the
// path that failed, so a card that cited something it was never shown reads
// "card_source_boundary" and not the Episode's code.
func aRefusedCardJob(userID int64, sessionID string, target uint) model.EpisodeProjectionJob {
	now := time.Now().UTC()
	return model.EpisodeProjectionJob{
		ID: uuid.NewString(), UserID: userID, SessionID: sessionID,
		DesiredMessageID: target, Status: model.EpisodeProjectionFailed,
		Attempts: 3, AvailableAt: now, ErrorCode: "card_source_boundary", CompletedAt: &now,
	}
}

// One conversation's card was refused. The other two were written.
//
// The refusal has to be recorded with the job's own code, and the attempt
// has to go on: the two conversations that were prepared well are worth
// keeping, and the refused one is the server's judgement about one card.
func TestOneRefusedCardDoesNotVoidTheOtherConversations(t *testing.T) {
	if os.Getenv("GL_SANDBOX_TEST") != "1" {
		t.Skip("requires local Docker MySQL and Redis images")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	sessions := []string{"s-1", "s-2", "s-3"}
	s, snapshot := cardFailureFixture(t, ctx, sessions)

	if err := (guardedStore{sandbox: s}).with(ctx, func(tx *gorm.DB) error {
		for _, sessionID := range []string{"s-1", "s-3"} {
			card := aWrittenCard(7, sessionID, cardTargetOf(t, snapshot, sessionID))
			if err := tx.Create(&card).Error; err != nil {
				return err
			}
		}
		job := aRefusedCardJob(7, "s-2", cardTargetOf(t, snapshot, "s-2"))
		return tx.Create(&job).Error
	}); err != nil {
		t.Fatal(err)
	}

	arm := &CardArm{Mode: CardArmModeOn, IndexName: "gopherai_session_card_eval_idx",
		KeyPrefix: "gopherai:session_card:eval:", WaitTimeout: 30 * time.Second}
	evidence, err := DriveArm(ctx, s, snapshot, arm)
	if err != nil {
		t.Fatalf("one refused card voided the attempt: %v; evidence=%+v", err, evidence)
	}
	// The refusal is recorded, and it names the conversation and the stage.
	if len(evidence.Failures) != 1 {
		t.Fatalf("the failures are %d, want 1: %+v", len(evidence.Failures), evidence.Failures)
	}
	failure := evidence.Failures[0]
	if failure.SessionID != "s-2" {
		t.Fatalf("the failure names another conversation: %+v", failure)
	}
	if failure.ErrorCode != "card_source_boundary" {
		t.Fatalf("the failure carries no code of its own: %+v", failure)
	}
	if failure.Error == "" {
		t.Fatalf("the failure says nothing about what happened: %+v", failure)
	}
	if failure.TargetMessageID != cardTargetOf(t, snapshot, "s-2") {
		t.Fatalf("the failure does not say how far the card was asked to reach: %+v", failure)
	}
	// The other two conversations were prepared, and the attempt says so.
	if !evidence.Ready {
		t.Fatalf("the attempt says no memory was written: %+v", evidence)
	}
	if len(evidence.Memories) != 2 {
		t.Fatalf("the memories are %d, want 2: %+v", len(evidence.Memories), evidence.Memories)
	}
	// The preparation record carries the failure too, because the record
	// travels with the archive and the query has to see it.
	if evidence.Preparation == nil || len(evidence.Preparation.Failures) != 1 {
		t.Fatalf("the preparation record does not carry the failure: %+v", evidence.Preparation)
	}
	if evidence.Preparation.Failures[0].SessionID != "s-2" {
		t.Fatalf("the preparation record names another conversation: %+v", evidence.Preparation.Failures)
	}
	// The conversations are still all there: a refusal must not remove the
	// conversation from the attempt.
	if len(evidence.SourceSessions) != len(sessions) {
		t.Fatalf("the attempt holds %d conversations, want %d: %+v",
			len(evidence.SourceSessions), len(sessions), evidence.SourceSessions)
	}
}

// EVERY conversation being refused stops the attempt.
//
// That is not a degraded card path - it is no card path at all, and an
// attempt that went on would measure a window with nothing in it and call it
// a measurement.
func TestEveryRefusedCardStopsTheAttempt(t *testing.T) {
	if os.Getenv("GL_SANDBOX_TEST") != "1" {
		t.Skip("requires local Docker MySQL and Redis images")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	sessions := []string{"s-1", "s-2"}
	s, snapshot := cardFailureFixture(t, ctx, sessions)

	if err := (guardedStore{sandbox: s}).with(ctx, func(tx *gorm.DB) error {
		for _, sessionID := range sessions {
			job := aRefusedCardJob(7, sessionID, cardTargetOf(t, snapshot, sessionID))
			if err := tx.Create(&job).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	arm := &CardArm{Mode: CardArmModeOn, IndexName: "gopherai_session_card_eval_idx",
		KeyPrefix: "gopherai:session_card:eval:", WaitTimeout: 30 * time.Second}
	evidence, err := DriveArm(ctx, s, snapshot, arm)
	if err == nil {
		t.Fatalf("an attempt with no card at all went on: %+v", evidence)
	}
	if len(evidence.Failures) != len(sessions) {
		t.Fatalf("the failures are %d, want %d: %+v", len(evidence.Failures), len(sessions), evidence.Failures)
	}
}
