package conversation

import (
	"context"
	"fmt"
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
	// At 42 turns the final user message crosses the production trigger. This
	// exercises compaction through the standard HTTP SDK path as well as ReAct.
	turns := make([]Turn, 42)
	for i := range turns {
		turns[i] = testTurn(i)
	}
	warmup, err := PrepareOfflineProductReport(ctx, repository, turns)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, calls := warmup.Snapshot, warmup.Calls
	writeOfflineEvidence(t, "warmup.json", warmup)
	if evidence := os.Getenv("GL_CONVERSATION_EVIDENCE_DIR"); evidence != "" {
		if err := WriteSnapshot(filepath.Join(evidence, "snapshot.json"), snapshot); err != nil {
			t.Fatal(err)
		}
	}
	if len(warmup.Turns) != 42 || warmup.DegradedTurns != 0 {
		t.Fatalf("warmup turns=%d degraded=%d", len(warmup.Turns), warmup.DegradedTurns)
	}
	if len(snapshot.Messages) != 84 || snapshot.Summary == nil || len(calls) != 12 {
		t.Fatalf("messages=%d summary=%+v calls=%d", len(snapshot.Messages), snapshot.Summary, len(calls))
	}
	for _, c := range calls {
		if c.Role != "summary" || c.Finished.IsZero() {
			t.Fatalf("warmup ledger: %+v", c)
		}
	}
	var provenance Provenance
	for repeat := 0; repeat < 3; repeat++ {
		result, err := RunOfflineProduct(ctx, repository, snapshot, "请回顾当前会话里的练习安排。")
		writeOfflineEvidence(t, fmt.Sprintf("repeat-%d.json", repeat), result)
		if err != nil {
			t.Fatalf("repeat %d: %v; response=%s audit=%+v", repeat, err, result.Response, result.Audit)
		}
		if !result.BackgroundAudit.Valid || !result.ShutdownComplete || result.Answer != "synthetic offline answer" {
			t.Fatalf("invalid product result: %+v", result)
		}
		if result.Entry != "gopherlearn-integrated:hybrid.Run" || result.StoppedCounts["episode"].Processing != 0 {
			t.Fatal("standard entry/shutdown evidence missing")
		}
		if repeat > 0 && (provenance.SnapshotSHA256 != result.Provenance.SnapshotSHA256 || provenance.ConfigSHA256 != result.Provenance.ConfigSHA256) {
			t.Fatal("repeated initial state drift")
		}
		provenance = result.Provenance
		if result.BeforeCounts["episode"].Pending != 1 || result.AfterCounts["episode"].Processing != 0 || result.Audit.ExecutedBeforeAnswer {
			t.Fatalf("unexpected background: %+v", result)
		}
		for _, job := range result.Audit.Before {
			if job.AvailableAt == nil || job.AvailableAt.Before(result.Audit.Started.Add(23*time.Hour)) || job.AvailableAt.After(result.Audit.Started.Add(25*time.Hour)) {
				t.Fatalf("quiet-window task evidence: %+v", job)
			}
		}
		foundSummary := false
		foundRecent := false
		for _, m := range result.ContextMessages {
			foundSummary = foundSummary || strings.Contains(m.Content, "synthetic offline summary")
			foundRecent = foundRecent || m.Content == turns[41].User
		}
		if !foundSummary || !foundRecent {
			t.Fatalf("missing summary/recent context: %+v", result.ContextMessages)
		}
		answers := 0
		summaries := 0
		for _, call := range result.Calls {
			if call.Role == "background" {
				t.Fatal("background model ran")
			}
			if call.Role == "answer" {
				answers++
			}
			if call.Role == "summary" {
				summaries++
			}
			if call.Usage != nil {
				t.Fatal("invented offline usage")
			}
		}
		if answers != 1 || summaries != 1 {
			t.Fatalf("answer model calls=%d summary calls=%d", answers, summaries)
		}
		t.Logf("repeat=%d restore_ms=%d question_ms=%d pending_before=%d calls=%d snapshot=%s config=%s", repeat, result.RestoreMilliseconds, result.QuestionMilliseconds, result.BeforeCounts["episode"].Pending, len(result.Calls), result.Provenance.SnapshotSHA256, result.Provenance.ConfigSHA256)
	}
}
