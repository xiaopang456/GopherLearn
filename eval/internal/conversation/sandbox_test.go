package conversation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"GopherAI/model"
	agent "GopherAI/service/agent"
)

func TestSandboxRejectsForgedIdentity(t *testing.T) {
	s := &Sandbox{id: "not-an-id", nonce: "bad"}
	if s.verify(context.Background()) == nil {
		t.Fatal("forged identity accepted")
	}
	if s.Close(context.Background()) == nil {
		t.Fatal("forged cleanup accepted")
	}
}
func TestSandboxIntegration(t *testing.T) {
	if os.Getenv("GL_SANDBOX_TEST") != "1" {
		t.Skip("requires a local Docker daemon and preloaded MySQL image")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, err := NewSandbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		closeCtx, c := context.WithTimeout(context.Background(), 15*time.Second)
		defer c()
		if err := s.Close(closeCtx); err != nil {
			t.Error(err)
		}
	}()
	conn, err := s.ConnectionInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(conn)
	if err != nil {
		t.Fatal(err)
	}
	if conn.Host != "127.0.0.1" || conn.Password == "" || strings.Contains(fmt.Sprint(conn), conn.Password) || strings.Contains(fmt.Sprintf("%#v", conn), conn.Password) || strings.Contains(string(encoded), conn.Password) {
		t.Fatal("invalid or exposed sandbox credentials")
	}
	if err = s.db.AutoMigrate(&model.User{}, &model.Session{}, &model.Message{}, &model.SessionSummary{}); err != nil {
		t.Fatal(err)
	}
	if err = s.db.Create(&model.User{ID: 7, Username: "learner"}).Error; err != nil {
		t.Fatal(err)
	}
	snap := validSnapshot()
	if err = s.Restore(ctx, snap); err != nil {
		t.Fatal(err)
	}
	got, err := s.Capture(ctx, snap.Session.UserID, snap.Session.ID)
	if err != nil || len(got.Messages) != 2 {
		t.Fatalf("capture: %+v %v", got, err)
	}
	if err = s.Restore(ctx, snap); err == nil {
		t.Fatal("accepted nonempty database")
	}
	for _, q := range []string{"CREATE TABLE episode_projection_jobs (id VARCHAR(36), status VARCHAR(24), attempts INT)", "CREATE TABLE memory_extraction_jobs (id VARCHAR(36), status VARCHAR(24), attempts INT)", "INSERT INTO episode_projection_jobs VALUES ('task','pending',0)"} {
		if err = s.db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	_, counts, err := s.ObserveJobs(ctx)
	if err != nil || counts["episode"].Pending != 1 {
		t.Fatalf("counts=%v err=%v", counts, err)
	}
	if err = s.db.Exec("UPDATE eval_instance_guard SET nonce = ?", "wrong").Error; err != nil {
		t.Fatal(err)
	}
	if err = s.Restore(ctx, snap); err == nil {
		t.Fatal("accepted altered database marker")
	}
	if _, _, err = s.ObserveJobs(ctx); err == nil {
		t.Fatal("observed altered instance")
	}
}

type syntheticGenerator struct{ calls int }

func (g *syntheticGenerator) GenerateSummary(_ context.Context, _ string, _ []model.Message) (agent.SummaryGeneration, error) {
	g.calls++
	return agent.SummaryGeneration{Content: `{"schema_version":2,"current_goal":"practice"}`}, nil
}
func TestSandboxWarmupIntegration(t *testing.T) {
	if os.Getenv("GL_SANDBOX_TEST") != "1" {
		t.Skip("requires a local Docker daemon and preloaded MySQL image")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, err := NewSandbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cctx, c := context.WithTimeout(context.Background(), 15*time.Second)
		defer c()
		if err := s.Close(cctx); err != nil {
			t.Error(err)
		}
	}()
	if err = s.db.AutoMigrate(&model.User{}, &model.Session{}, &model.Message{}, &model.SessionSummary{}); err != nil {
		t.Fatal(err)
	}
	if err = s.db.Create(&model.User{ID: 7, Username: "learner"}).Error; err != nil {
		t.Fatal(err)
	}
	if err = s.db.Create(&model.Session{ID: "session", UserID: 7, UserName: "learner", Mode: model.SessionModeAgent}).Error; err != nil {
		t.Fatal(err)
	}
	turns := make([]Turn, 40)
	for i := range turns {
		turns[i] = testTurn(i)
	}
	g := &syntheticGenerator{}
	got, err := s.Prepare(ctx, 7, "session", "learner", turns, g, agent.DefaultSummaryConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 40 || g.calls != 12 {
		t.Fatalf("turns=%d calls=%d", len(got), g.calls)
	}
	snap, err := s.Capture(ctx, 7, "session")
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Messages) != 80 || snap.Summary.CoveredMessageID != 71 {
		t.Fatalf("messages=%d summary=%+v", len(snap.Messages), snap.Summary)
	}
	if _, err = s.Prepare(ctx, 7, "session", "learner", turns, g, agent.DefaultSummaryConfig()); err == nil {
		t.Fatal("prepared a nonempty session")
	}
}
