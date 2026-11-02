package conversation

import (
	"context"
	"fmt"
	"testing"

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
func (s *storage) GenerateSummary(_ context.Context, prev string, msgs []model.Message) (string, error) {
	s.calls++
	return fmt.Sprintf("summary-%d", msgs[len(msgs)-1].ID), nil
}
func TestPreparationUsesProductPolicyAfterUserMessage(t *testing.T) {
	s := &storage{}
	p, err := agent.NewRollingSummaryProvider(s, s, s, agent.DefaultSummaryConfig())
	if err != nil {
		t.Fatal(err)
	}
	turns := make([]Turn, 40)
	for i := range turns {
		turns[i] = Turn{User: fmt.Sprintf("u%d", i), Assistant: fmt.Sprintf("a%d", i)}
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
	}
}
func TestPreparationRejectsEmptyTurnsBeforeWriting(t *testing.T) {
	s := &storage{}
	p, _ := agent.NewRollingSummaryProvider(s, s, s, agent.DefaultSummaryConfig())
	_, err := Prepare(context.Background(), s, p, 7, "synthetic", "learner", []Turn{{User: "ok", Assistant: "ok"}, {User: "", Assistant: "x"}})
	if err == nil || len(s.rows) != 0 {
		t.Fatalf("err=%v rows=%d", err, len(s.rows))
	}
}
