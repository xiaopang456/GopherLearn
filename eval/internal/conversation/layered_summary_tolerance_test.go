package conversation

// One conversation's layered summary failing must not void the others.
//
// The product refuses a summary it cannot store - a document with a field the
// decoder does not know, or a summary that does not fit the budget it is read
// under - and both refusals are deliberate. A preparation that stopped at the
// first refusal would throw away the conversations that were summarized well,
// and the arm would measure an empty set instead of a degraded one.
//
// So the failure is recorded and the preparation goes on. Two things have to
// hold at the same time:
//
//   - one failure does not stop the others;
//   - a preparation where NO conversation got a layered summary still stops,
//     because that is not a degraded card path - it is no card path at all.
//
// The second one is the guard against tolerance becoming blindness.
//
// The test drives the REAL function - it hands it a generator that refuses one
// batch, which is what the real model does - and it runs against a real
// sandbox, because the function reads and writes the database.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"GopherAI/model"
	agent "GopherAI/service/agent"
	"gorm.io/gorm"
)

// aGeneratorThatRefusesOneBatch refuses the batch of one conversation and
// answers the others.
//
// The refusal is raised for the batch whose last message id is the refused
// one, because that is how the harness says "this batch": the batch is the
// conversation's whole stretch.
type aGeneratorThatRefusesOneBatch struct {
	// refusedLastMessageID is the last message id of the refused batch.
	refusedLastMessageID uint
	// refused is the error the refusal carries.
	refused error
	// asked records the batches that were asked for.
	asked []uint
}

func (g *aGeneratorThatRefusesOneBatch) GenerateSegment(_ context.Context,
	input agent.SegmentGenerationInput) (agent.SegmentGenerationResult, error) {
	last := uint(input.LastMessageID)
	g.asked = append(g.asked, last)
	if last == g.refusedLastMessageID {
		return agent.SegmentGenerationResult{}, g.refused
	}
	return aWrittenSegment(input), nil
}

// aWrittenSegment is what a generator answers when it works.
//
// The segment has to carry a point that cites a message of the batch, because
// a point that cites nothing is not a segment - and the program refuses one.
func aWrittenSegment(input agent.SegmentGenerationInput) agent.SegmentGenerationResult {
	content := model.SessionSummarySegmentContent{
		SchemaVersion: model.SessionSummarySegmentSchemaV1,
		Topic:         "本地替身摘要段",
	}
	if len(input.Messages) > 0 {
		content.Points = []model.SessionSummarySegmentPoint{{
			Text:               "学习者说过的话",
			EvidenceMessageIDs: []uint{input.Messages[0].ID},
			Source:             model.SessionSummarySegmentSourceUser,
		}}
	}
	encoded, err := json.Marshal(content)
	if err != nil {
		return agent.SegmentGenerationResult{}
	}
	return agent.SegmentGenerationResult{
		Segment:           content,
		SegmentContent:    string(encoded),
		WhiteboardContent: `{"schema_version":1,"goal":null,"constraints":[],"plan":[],"open_items":[]}`,
	}
}

// The two refusals the real model produced on 2026-11-17.
//
// The first is a summary document with a field the decoder does not know: the
// prompt asks for "at most 6 of your products", the model wrote a `products`
// key, and the strict decoder refused the whole document. The second is a
// summary that does not fit the budget it is read under: the product asks
// again for a shorter one and gives up after that.
//
// They are different failures and they land the same way: the conversation has
// no segment and no board.
var (
	errUnknownField = errors.New("decode summary segment output: json: unknown field \"products\"")
	errOverBudget   = errors.New("summary segment does not fit the budget it is read under")
)

// aLayeredFixture is the sandbox with a few conversations in it.
func aLayeredFixture(t *testing.T, ctx context.Context, sessions []string, turnsPerSession int) *Sandbox {
	t.Helper()
	s, err := NewSandbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeSandbox(s) })
	if err := s.InitProductSchema(ctx); err != nil {
		t.Fatal(err)
	}
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
			for i := 0; i < turnsPerSession; i++ {
				message := model.Message{SessionID: sessionID, UserName: user.Username,
					IsUser: i%2 == 0, Content: fmt.Sprintf("第 %d 轮的话", i), CreatedAt: time.Now()}
				if err := tx.Create(&message).Error; err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

// One conversation's refusal does not void the others.
//
// The three conversations each get their own batch, and the generator refuses
// the second one. The preparation has to come back with the other two
// summarized and the refusal recorded.
func TestOneConversationThatFailsDoesNotVoidTheOthers(t *testing.T) {
	if os.Getenv("GL_SANDBOX_TEST") != "1" {
		t.Skip("requires local Docker MySQL and Redis images")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	sessions := []string{"s-1", "s-2", "s-3"}
	s := aLayeredFixture(t, ctx, sessions, 4)

	// The refused batch is the second conversation's: its last message id is
	// the one the generator refuses.
	var refusedLast uint
	if err := (guardedStore{sandbox: s}).with(ctx, func(tx *gorm.DB) error {
		var ids []uint
		if err := tx.Model(&model.Message{}).Where("session_id = ?", "s-2").
			Order("id ASC").Pluck("id", &ids).Error; err != nil {
			return err
		}
		refusedLast = ids[len(ids)-1]
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	generator := &aGeneratorThatRefusesOneBatch{refusedLastMessageID: refusedLast, refused: errUnknownField}
	records, err := WriteLayeredSummariesWithGenerator(ctx, s, 7, sessions, generator, SegmentSettings{
		MaxMessages: 64, FetchLimit: 256, MaxInputTokens: 8000, MaxOutputTokens: 2000,
		SegmentTokens: 450, AssistantContent: true,
	})
	if err != nil {
		t.Fatalf("one conversation's refusal voided the preparation: %v", err)
	}
	if len(records) != len(sessions) {
		t.Fatalf("the preparation recorded %d conversations, want %d", len(records), len(sessions))
	}
	// Every conversation is accounted for, and the refused one carries the
	// reason.
	bySession := map[string]LayeredSummaryRecord{}
	for _, record := range records {
		bySession[record.SessionID] = record
	}
	for _, sessionID := range sessions {
		if _, ok := bySession[sessionID]; !ok {
			t.Fatalf("the conversation %s is not in the record", sessionID)
		}
	}
	if refused := bySession["s-2"]; refused.Error == "" {
		t.Fatalf("the refused conversation says nothing about why: %+v", refused)
	}
	if refused := bySession["s-2"]; refused.Segments != 0 || refused.Whiteboard {
		t.Fatalf("the refused conversation claims a compressed state: %+v", refused)
	}
	if failures := countLayeredSummaryFailures(records); failures != 1 {
		t.Fatalf("the failures are %d, want 1", failures)
	}
	// The refusal was asked for exactly once, and the other two were asked
	// too - the preparation went on.
	if len(generator.asked) != len(sessions) {
		t.Fatalf("the generator was asked for %d batches, want %d", len(generator.asked), len(sessions))
	}
}

// A preparation where every conversation failed has no card path at all, and
// it has to stop: tolerance must not become blindness.
func TestAPreparationWhereEveryConversationFailedStops(t *testing.T) {
	if os.Getenv("GL_SANDBOX_TEST") != "1" {
		t.Skip("requires local Docker MySQL and Redis images")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	sessions := []string{"s-1", "s-2"}
	s := aLayeredFixture(t, ctx, sessions, 4)

	// The generator refuses every batch: its last message id is 0, and no
	// batch ends at 0.
	generator := &aGeneratorThatRefusesOneBatch{refusedLastMessageID: 0, refused: errUnknownField}
	// The refusal has to fire for every batch, so it is keyed on "any batch".
	records, err := WriteLayeredSummariesWithGenerator(ctx, s, 7, sessions, alwaysRefusing{refused: errUnknownField},
		SegmentSettings{MaxMessages: 64, FetchLimit: 256, MaxInputTokens: 8000, MaxOutputTokens: 2000,
			SegmentTokens: 450, AssistantContent: true})
	if err == nil {
		t.Fatalf("a preparation with no layered summary at all went on: %+v", records)
	}
	if countLayeredSummaryFailures(records) != len(sessions) {
		t.Fatalf("the failures are %d, want %d", countLayeredSummaryFailures(records), len(sessions))
	}
	_ = generator
}

// alwaysRefusing refuses every batch.
type alwaysRefusing struct{ refused error }

func (g alwaysRefusing) GenerateSegment(context.Context, agent.SegmentGenerationInput) (agent.SegmentGenerationResult, error) {
	return agent.SegmentGenerationResult{}, g.refused
}

// The over-budget refusal lands the same way as the unknown field: the
// conversation has no segment, and the preparation goes on.
func TestTheOverBudgetRefusalIsRecordedLikeTheOtherOne(t *testing.T) {
	if os.Getenv("GL_SANDBOX_TEST") != "1" {
		t.Skip("requires local Docker MySQL and Redis images")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	sessions := []string{"s-1", "s-2"}
	s := aLayeredFixture(t, ctx, sessions, 4)

	records, err := WriteLayeredSummariesWithGenerator(ctx, s, 7, sessions,
		alwaysRefusing{refused: errOverBudget},
		SegmentSettings{MaxMessages: 64, FetchLimit: 256, MaxInputTokens: 8000, MaxOutputTokens: 2000,
			SegmentTokens: 450, AssistantContent: true})
	if err == nil {
		t.Fatalf("a preparation with no layered summary at all went on: %+v", records)
	}
	for _, record := range records {
		if record.Error == "" {
			t.Fatalf("the refusal is not recorded: %+v", record)
		}
		if record.Segments != 0 {
			t.Fatalf("the refused conversation claims a segment: %+v", record)
		}
	}
}
