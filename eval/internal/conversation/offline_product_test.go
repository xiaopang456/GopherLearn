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
	warmup, err := PrepareOfflineProductReport(ctx, repository, SingleHistory("synthetic-evaluation", turns), AttemptInput{}, "")
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
	// The preparation makes the rolling summary's calls and ONE layered
	// summary call: the card path reads the summary segments and the
	// whiteboard, so the segments have to exist before the card is written.
	// The call count is the evidence that the preparation really wrote them.
	if len(primaryOf(snapshot).Messages) != 84 || snapshot.Sessions[0].Summary == nil || len(calls) != 13 {
		t.Fatalf("messages=%d summary=%+v calls=%d", len(primaryOf(snapshot).Messages), snapshot.Sessions[0].Summary, len(calls))
	}
	// The layered summary's own record says what it wrote, and it has to say
	// that the segments and the board exist: a preparation that wrote neither
	// would leave the card written from the raw messages alone.
	if len(warmup.LayeredSummary) != 1 {
		t.Fatalf("the preparation wrote no layered summary: %+v", warmup.LayeredSummary)
	}
	written := warmup.LayeredSummary[0]
	if written.Segments == 0 || written.Whiteboard == false || written.Points == 0 {
		t.Fatalf("the layered summary wrote nothing: %+v", written)
	}
	for _, c := range calls {
		if c.Role != "summary" || c.Finished.IsZero() {
			t.Fatalf("warmup ledger: %+v", c)
		}
	}
	var provenance Provenance
	for repeat := 0; repeat < 3; repeat++ {
		result, err := RunOfflineProduct(ctx, repository, snapshot, AttemptInput{Questions: []Question{{Text: "请回顾当前会话里的练习安排。"}}})
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
		if result.BeforeCounts["episode"].Pending != 1 || result.AfterCounts["episode"].Processing != 0 || result.Audit.ExecutedInQueryPhase {
			t.Fatalf("unexpected background: %+v", result)
		}
		for _, job := range result.Audit.Before {
			if job.AvailableAt == nil || job.AvailableAt.Before(result.Audit.Started.Add(23*time.Hour)) || job.AvailableAt.After(result.Audit.Started.Add(25*time.Hour)) {
				t.Fatalf("quiet-window task evidence: %+v", job)
			}
		}
		foundSummary := false
		foundRecent := false
		foundSegment := false
		foundBoard := false
		for _, m := range result.ContextMessages {
			foundSummary = foundSummary || strings.Contains(m.Content, "synthetic offline summary")
			foundRecent = foundRecent || m.Content == turns[41].User
			foundSegment = foundSegment || strings.Contains(m.Content, "synthetic offline segment")
			foundBoard = foundBoard || strings.Contains(m.Content, "Current state")
		}
		// The window carries the layered state: the board and the segments. It
		// used to carry the rolling summary instead, and the assertion looked
		// for that text. The difference is the point of the change: the
		// preparation now writes the segments and the board, so the window
		// reads THEM - and a window that fell back to the rolling summary
		// would be evidence that the compressed state was not there.
		if !foundRecent {
			t.Fatalf("missing the recent context: %+v", result.ContextMessages)
		}
		if !foundSegment {
			t.Fatalf("the window carries no summary segment: %+v", result.ContextMessages)
		}
		if !foundBoard {
			t.Fatalf("the window carries no board: %+v", result.ContextMessages)
		}
		// The rolling summary is not what the window reads any more, and its
		// absence is the point: the compressed state is there instead.
		if foundSummary {
			t.Fatalf("the window still carries the rolling summary: %+v", result.ContextMessages)
		}
		answers := 0
		summaries := 0
		reranks := 0
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
			if call.Role == "rerank" {
				reranks++
			}
			if call.Usage != nil {
				t.Fatal("invented offline usage")
			}
		}
		if answers != 1 || summaries != 1 || reranks != 1 {
			t.Fatalf("answer model calls=%d summary calls=%d rerank calls=%d", answers, summaries, reranks)
		}
		t.Logf("repeat=%d restore_ms=%d question_ms=%d pending_before=%d calls=%d snapshot=%s config=%s", repeat, result.RestoreMilliseconds, result.QuestionMilliseconds, result.BeforeCounts["episode"].Pending, len(result.Calls), result.Provenance.SnapshotSHA256, result.Provenance.ConfigSHA256)
	}
}
