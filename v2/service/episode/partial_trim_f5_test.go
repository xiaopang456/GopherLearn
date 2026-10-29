package episode

// STABILIZE-065 F5: partial-trim behaviour at the real Projector seam.
//
// This is the genuine F5 required by C3-A.2: a fake Generator returns one
// EpisodeDocument carrying three assertions across at least two different
// slots; the SupportVerifier returns entailed, not_entailed and uncertain
// respectively; the test proves that only the entailed assertion survives,
// the Episode is active, the trimmed ones leave no assertion state, the
// read-back contains only the survivor, the verifier was called exactly three
// times, the generator exactly once, a same-watermark re-run calls neither,
// and the active Episode is not confused with an abstention row.
//
// If this test passes on first run, that is recorded as "test coverage was
// missing but the production behaviour was correct" — not as a red that was
// fixed.

import (
	"context"
	"testing"
	"time"

	episodedao "GopherAI/dao/episode"
	"GopherAI/model"
)

// sequencedVerifier returns its scripted verdicts in order, one per call.
type sequencedVerifier struct {
	verdicts []SupportVerdict
	calls    int
}

func (v *sequencedVerifier) Verify(_ context.Context, _ SupportRequest) (SupportVerdict, error) {
	if v.calls >= len(v.verdicts) {
		v.calls++
		return SupportUncertain, nil // past the script: fail safe
	}
	verdict := v.verdicts[v.calls]
	v.calls++
	return verdict, nil
}

// countingGenerator wraps stubGenerator to also count calls.
type countingGenerator struct {
	document model.EpisodeDocument
	calls    int
}

func (g *countingGenerator) Generate(_ context.Context, _ ProjectionEvidence) (ProjectionOutcome, error) {
	g.calls++
	return Generated(g.document), nil
}

// roundTripRepository wraps stubProjectionRepository so GetBySession returns
// the last saved row, modelling the database's read-after-write that the
// watermark guard depends on.
type roundTripRepository struct {
	stubProjectionRepository
}

func (r *roundTripRepository) GetBySession(ctx context.Context, userID int64, sessionID string) (*model.EpisodeMemory, error) {
	if r.saved == nil {
		return nil, episodedao.ErrEpisodeNotFound
	}
	copy := *r.saved
	return &copy, nil
}

func TestPartialTrimEntailedOnly(t *testing.T) {
	now := time.Now()
	evidence := ProjectionEvidence{
		UserID: 7, Session: model.Session{ID: "session-f5", UserID: 7, Title: "F5"},
		TargetMessageID: 3, TargetMessageAt: now,
		Messages: []model.Message{
			{ID: 1, SessionID: "session-f5", IsUser: true, Content: "决定用长度前缀解决粘包", CreatedAt: now},
			{ID: 2, SessionID: "session-f5", IsUser: true, Content: "之前以为轮询能自动适配，后来发现不行", CreatedAt: now},
			{ID: 3, SessionID: "session-f5", IsUser: true, Content: "以后每次压测前先跑 baseline", CreatedAt: now},
		},
	}
	document := model.EpisodeDocument{
		Synopsis: "用户在 RPC 学习中做出粘包决策、纠正负载均衡理解和留下压测待办。",
		Topics:   []model.EpisodeTopic{{Key: "rpc-粘包", Label: "RPC 粘包"}},
		// Three assertions across at least two different slots:
		Decisions: []model.EpisodeEvidence{
			{Text: "决定用长度前缀解决粘包", EvidenceMessageIDs: []uint{1}},
		},
		UserCorrections: []model.EpisodeEvidence{
			{Text: "纠正了轮询能自动适配的理解", EvidenceMessageIDs: []uint{2}},
		},
		OpenLoops: []model.EpisodeEvidence{
			{Text: "以后每次压测前先跑 baseline", EvidenceMessageIDs: []uint{3}},
		},
		SourceHandles: []string{"message:1", "message:2", "message:3"},
	}

	verifier := &sequencedVerifier{verdicts: []SupportVerdict{
		SupportEntailed,    // decision survives
		SupportNotEntailed, // correction trimmed
		SupportUncertain,   // open loop trimmed (uncertain = trimmed)
	}}
	generator := &countingGenerator{document: document}
	loader := stubEvidenceLoader{evidence: evidence}
	repo := &roundTripRepository{}

	projector, err := NewProjector(loader, generator, repo, verifier)
	if err != nil {
		t.Fatalf("NewProjector: %v", err)
	}

	episode, err := projector.Project(context.Background(), 7, "session-f5", 3)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}

	// 1. Only the entailed assertion survives.
	if len(episode.Document.Decisions) != 1 {
		t.Fatalf("Decisions should have exactly 1 surviving assertion, got %d", len(episode.Document.Decisions))
	}
	// 2. Trimmed slots have zero surviving assertions.
	if len(episode.Document.UserCorrections) != 0 {
		t.Fatalf("UserCorrections should be trimmed to 0, got %d", len(episode.Document.UserCorrections))
	}
	if len(episode.Document.OpenLoops) != 0 {
		t.Fatalf("OpenLoops should be trimmed to 0, got %d", len(episode.Document.OpenLoops))
	}
	// 3. Episode status is active (not abstained).
	if episode.AbstainReason != "" {
		t.Fatalf("active Episode must not carry an abstain reason, got %q", episode.AbstainReason)
	}
	// 4. Read-back: the saved copy contains only the survivor.
	saved := repo.saved
	if saved == nil {
		t.Fatal("repository saved nothing; an active Episode must be saved")
	}
	if saved.AbstainReason != "" {
		t.Fatalf("saved active Episode must not carry abstain reason, got %q", saved.AbstainReason)
	}
	if len(saved.Document.Decisions) != 1 {
		t.Fatalf("saved document Decisions should have 1, got %d", len(saved.Document.Decisions))
	}
	if len(saved.Document.UserCorrections) != 0 || len(saved.Document.OpenLoops) != 0 {
		t.Fatalf("saved document must not carry trimmed assertions")
	}
	// 5. Source handle and slot correctness for the survivor.
	survivor := saved.Document.Decisions[0]
	if survivor.Text != "决定用长度前缀解决粘包" {
		t.Fatalf("survivor text mismatch: %q", survivor.Text)
	}
	if len(survivor.EvidenceMessageIDs) != 1 || survivor.EvidenceMessageIDs[0] != 1 {
		t.Fatalf("survivor evidence message ID should be 1, got %v", survivor.EvidenceMessageIDs)
	}
	// 6. Verifier called exactly 3 times (one per assertion).
	if verifier.calls != 3 {
		t.Fatalf("verifier should be called exactly 3 times, got %d", verifier.calls)
	}
	// 7. Generator called exactly 1 time.
	if generator.calls != 1 {
		t.Fatalf("generator should be called exactly 1 time, got %d", generator.calls)
	}

	// 8. Same-watermark re-run: generator=0 new calls, verifier=0 new calls.
	beforeGen := generator.calls
	beforeVer := verifier.calls
	_, err = projector.Project(context.Background(), 7, "session-f5", 3)
	if err != nil {
		t.Fatalf("re-Project at same watermark: %v", err)
	}
	if generator.calls != beforeGen {
		t.Fatalf("same-watermark re-run must not re-call generator: %d -> %d", beforeGen, generator.calls)
	}
	if verifier.calls != beforeVer {
		t.Fatalf("same-watermark re-run must not re-call verifier: %d -> %d", beforeVer, verifier.calls)
	}

	// 9. No duplicate write: the second Project did not add a second save.
	// (stubProjectionRepository overwrites `saved` on Save; a duplicate write
	// would be indistinguishable here, but the watermark guard above is the
	// mechanism that prevents it, and the existing-version check in the
	// repository enforces it at the database boundary.)
}
