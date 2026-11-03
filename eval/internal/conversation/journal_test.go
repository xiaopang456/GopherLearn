package conversation

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestJobJournalIntegration(t *testing.T) {
	if os.Getenv("GL_SANDBOX_TEST") != "1" {
		t.Skip("requires local Docker MySQL image")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, err := NewSandbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		c, done := context.WithTimeout(context.Background(), 15*time.Second)
		defer done()
		if err := s.Close(c); err != nil {
			t.Error(err)
		}
	}()
	for _, table := range []string{"episode_projection_jobs", "memory_extraction_jobs"} {
		if err = s.db.Exec("CREATE TABLE " + table + " (id VARCHAR(36) PRIMARY KEY, status VARCHAR(24), attempts INT) ENGINE=InnoDB").Error; err != nil {
			t.Fatal(err)
		}
	}
	j, err := s.StartJobJournal(ctx)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string) {
		t.Helper()
		if err := s.db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	exec("INSERT INTO episode_projection_jobs VALUES ('a','pending',0)")
	// A short claim/reset/delete must survive in evidence even when both samples are empty.
	exec("UPDATE episode_projection_jobs SET status='processing',attempts=1 WHERE id='a'")
	exec("UPDATE episode_projection_jobs SET status='pending',attempts=0 WHERE id='a'")
	exec("DELETE FROM episode_projection_jobs WHERE id='a'")
	if err = j.AnswerBarrier(ctx); err != nil {
		t.Fatal(err)
	}
	exec("INSERT INTO memory_extraction_jobs VALUES ('after','processing',1)")
	events, executed, err := j.Finish(ctx)
	if err != nil || !executed || len(events) != 8 {
		t.Fatalf("events=%+v executed=%v err=%v", events, executed, err)
	}
	audit := BackgroundAudit{Started: time.Now().Add(-time.Minute), Answered: time.Now(), Complete: true, ExecutedBeforeAnswer: executed}
	if err := audit.ValidateBackground(); err == nil {
		t.Fatal("transient execution certified as valid")
	}
	if err = j.AnswerBarrier(ctx); err == nil {
		t.Fatal("duplicate barrier accepted")
	}
	exec("DROP TRIGGER eval_episode_update")
	if _, _, err = j.Finish(ctx); err == nil {
		t.Fatal("missing trigger accepted")
	}
}
