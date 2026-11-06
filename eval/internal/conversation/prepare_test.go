package conversation

import (
	"context"
	"fmt"
	"testing"
	"time"

	sessiondao "GopherAI/dao/session"
	"GopherAI/model"
	agent "GopherAI/service/agent"
)

type storage struct {
	rows    []model.Message
	summary *model.SessionSummary
	calls   int
}

func (s *storage) Create(_ context.Context, m *model.Message) (*model.Message, error) {
	x := *m
	x.ID = uint(len(s.rows) + 1)
	s.rows = append(s.rows, x)
	return &x, nil
}
func (s *storage) ListAfter(_ context.Context, session string, id uint, limit int) ([]model.Message, error) {
	var out []model.Message
	for _, m := range s.rows {
		if m.SessionID == session && m.ID > id {
			out = append(out, m)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}
func (s *storage) GetSummary(_ context.Context, user int64, session string) (*model.SessionSummary, error) {
	if s.summary == nil {
		return nil, sessiondao.ErrSummaryNotFound
	}
	x := *s.summary
	return &x, nil
}
func (s *storage) SaveSummary(_ context.Context, su *model.SessionSummary, version uint64) error {
	x := *su
	x.Version = version + 1
	su.Version = x.Version
	s.summary = &x
	return nil
}
func (s *storage) GenerateSummary(_ context.Context, prev string, msgs []model.Message) (agent.SummaryGeneration, error) {
	s.calls++
	// A document, not bare prose. The real generator parses and normalises before it
	// returns, so a stored summary can never be a sentence like "summary-71"; returning
	// one here was modelling a state the product cannot produce. Compaction now reads the
	// stored row before spending a call, so a prose fake would silently stop every later
	// compaction and this harness would be measuring the skip instead of the policy.
	return agent.SummaryGeneration{Content: fmt.Sprintf(
		`{"schema_version":2,"current_goal":"summary-%d","confirmed_decisions":[],"completed_items":[],"open_loops":[],"user_corrections":[],"key_errors":[],"references":[]}`,
		msgs[len(msgs)-1].ID)}, nil
}
func TestPreparationUsesProductPolicyAfterUserMessage(t *testing.T) {
	s := &storage{}
	p, err := agent.NewRollingSummaryProvider(s, s, s, agent.DefaultSummaryConfig())
	if err != nil {
		t.Fatal(err)
	}
	turns := make([]Turn, 40)
	for i := range turns {
		turns[i] = testTurn(i)
	}
	got, err := Prepare(context.Background(), s, p, 7, "synthetic", "learner", turns)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.rows) != 80 || len(got) != 40 || s.calls != 12 {
		t.Fatalf("messages=%d turns=%d calls=%d", len(s.rows), len(got), s.calls)
	}
	if s.summary.CoveredMessageID != 71 {
		t.Fatalf("summary watermark=%d", s.summary.CoveredMessageID)
	}
	for i, v := range got {
		if v.UserID != uint(2*i+1) || v.AssistantID != uint(2*i+2) {
			t.Fatalf("wrong order: %+v", v)
		}
		if !s.rows[2*i].CreatedAt.Equal(turns[i].UserAt) || !s.rows[2*i+1].CreatedAt.Equal(turns[i].AssistantAt) {
			t.Fatal("timestamps changed")
		}
	}
}
func TestPreparationRejectsUnorderedTimesBeforeWriting(t *testing.T) {
	s := &storage{}
	p, _ := agent.NewRollingSummaryProvider(s, s, s, agent.DefaultSummaryConfig())
	bad := testTurn(1)
	bad.UserAt = testTurn(0).UserAt
	_, err := Prepare(context.Background(), s, p, 7, "synthetic", "learner", []Turn{testTurn(0), bad})
	if err == nil || len(s.rows) != 0 {
		t.Fatalf("err=%v rows=%d", err, len(s.rows))
	}
}
func testTurn(i int) Turn {
	t := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Minute)
	return Turn{User: fmt.Sprintf("u%d", i), Assistant: fmt.Sprintf("a%d", i), UserAt: t, AssistantAt: t.Add(time.Second)}
}
func TestPreparationRejectsEmptyTurnsBeforeWriting(t *testing.T) {
	s := &storage{}
	p, _ := agent.NewRollingSummaryProvider(s, s, s, agent.DefaultSummaryConfig())
	bad := testTurn(1)
	bad.User = ""
	_, err := Prepare(context.Background(), s, p, 7, "synthetic", "learner", []Turn{testTurn(0), bad})
	if err == nil || len(s.rows) != 0 {
		t.Fatalf("err=%v rows=%d", err, len(s.rows))
	}
}

type failingSummary struct{}

func (failingSummary) SessionSummary(context.Context, agent.RunContext) (agent.SummarySnapshot, error) {
	return agent.SummarySnapshot{Content: "old", CoveredMessageID: 3, Version: 1}, fmt.Errorf("temporary failure")
}
func TestSummaryFailureContinuesAndRecordsDegradation(t *testing.T) {
	s := &storage{}
	got, err := Prepare(context.Background(), s, failingSummary{}, 7, "synthetic", "learner", []Turn{testTurn(0)})
	if err != nil || len(got) != 1 || len(s.rows) != 2 || !got[0].SummaryDegraded || got[0].Summary.CoveredMessageID != 3 {
		t.Fatalf("got=%+v err=%v rows=%d", got, err, len(s.rows))
	}
}

// A replay that spends the product's failure cooldown in one breath never
// compacts again. The preparation waits the cooldown out before the next turn.
func TestPreparationWaitsOutTheFailureCooldownBeforeTheNextTurn(t *testing.T) {
	s := &storage{}
	turns := []Turn{testTurn(0), testTurn(1)}
	started := time.Now()
	got, err := PrepareWithCooldown(context.Background(), s, failingSummary{}, 7, "synthetic", "learner", turns, 60*time.Millisecond)
	if err != nil || len(got) != 2 {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	if elapsed := time.Since(started); elapsed < 60*time.Millisecond {
		t.Fatalf("the replay wrote the next turn after %s, before the cooldown was over", elapsed)
	}
	if got[0].CooldownWaitMilliseconds != 0 {
		t.Fatalf("the first turn waited %dms, nothing had failed yet", got[0].CooldownWaitMilliseconds)
	}
	if got[1].CooldownWaitMilliseconds <= 0 {
		t.Fatalf("the second turn waited %dms, the cooldown was not waited out", got[1].CooldownWaitMilliseconds)
	}
	if !got[0].SummaryDegraded || !got[1].SummaryDegraded {
		t.Fatal("a failed summary must stay a degraded turn")
	}
}

func TestPreparationWithoutACooldownDoesNotWait(t *testing.T) {
	s := &storage{}
	started := time.Now()
	got, err := PrepareWithCooldown(context.Background(), s, failingSummary{}, 7, "synthetic", "learner", []Turn{testTurn(0), testTurn(1)}, 0)
	if err != nil || len(got) != 2 {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	if elapsed := time.Since(started); elapsed > 30*time.Millisecond {
		t.Fatalf("the replay waited %s although no cooldown was asked for", elapsed)
	}
	for _, turn := range got {
		if turn.CooldownWaitMilliseconds != 0 {
			t.Fatalf("a turn waited %dms although no cooldown was asked for", turn.CooldownWaitMilliseconds)
		}
	}
}
