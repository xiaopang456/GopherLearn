package conversation

import (
	"context"
	"os"
	"testing"
	"time"

	"GopherAI/model"
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
