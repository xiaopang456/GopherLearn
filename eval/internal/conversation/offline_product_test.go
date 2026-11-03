package conversation

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCompleteOfflineProductIntegration(t *testing.T) {
	if os.Getenv("GL_SANDBOX_TEST") != "1" {
		t.Skip("requires local Docker MySQL image")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	repository := filepath.Join("..", "..", "..")
	turns := make([]Turn, 40)
	for i := range turns {
		turns[i] = testTurn(i)
	}
	snapshot, calls, err := PrepareOfflineProduct(ctx, repository, turns)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Messages) != 80 || snapshot.Summary == nil || len(calls) != 12 {
		t.Fatalf("messages=%d summary=%+v calls=%d", len(snapshot.Messages), snapshot.Summary, len(calls))
	}
	for _, c := range calls {
		if c.Role != "summary" || c.Finished.IsZero() {
			t.Fatalf("warmup ledger: %+v", c)
		}
	}
	for repeat := 0; repeat < 3; repeat++ {
		result, err := RunOfflineProduct(ctx, repository, snapshot, "请回顾当前会话里的练习安排。")
		if err != nil {
			t.Fatalf("repeat %d: %v; response=%s audit=%+v", repeat, err, result.Response, result.Audit)
		}
		if !result.BackgroundAudit.Valid || !result.ShutdownComplete || result.Answer != "synthetic offline answer" {
			t.Fatalf("invalid product result: %+v", result)
		}
		if result.BeforeCounts["episode"].Pending != 1 || result.AfterCounts["episode"].Processing != 0 || result.Audit.ExecutedBeforeAnswer {
			t.Fatalf("unexpected background: %+v", result)
		}
		foundSummary := false
		foundRecent := false
		for _, m := range result.ContextMessages {
			foundSummary = foundSummary || strings.Contains(m.Content, "synthetic offline summary")
			foundRecent = foundRecent || m.Content == turns[39].User
		}
		if !foundSummary || !foundRecent {
			t.Fatalf("missing summary/recent context: %+v", result.ContextMessages)
		}
		answers := 0
		for _, call := range result.Calls {
			if call.Role == "background" {
				t.Fatal("background model ran")
			}
			if call.Role == "answer" {
				answers++
			}
			if call.Usage != nil {
				t.Fatal("invented offline usage")
			}
		}
		if answers != 1 {
			t.Fatalf("answer model calls=%d", answers)
		}
	}
}
