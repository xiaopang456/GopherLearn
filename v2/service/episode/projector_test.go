package episode

import (
	episodedao "GopherAI/dao/episode"
	"GopherAI/model"
	"context"
	"errors"
	"testing"
	"time"
)

type stubEvidenceLoader struct {
	evidence ProjectionEvidence
	err      error
}

func (s stubEvidenceLoader) Load(context.Context, int64, string, uint) (ProjectionEvidence, error) {
	return s.evidence, s.err
}

type stubGenerator struct {
	document model.EpisodeDocument
	err      error
	calls    int
}

func (s *stubGenerator) Generate(context.Context, ProjectionEvidence) (ProjectionOutcome, error) {
	s.calls++
	if s.err != nil {
		return ProjectionOutcome{}, s.err
	}
	return Generated(s.document), nil
}

type stubProjectionRepository struct {
	existing        *model.EpisodeMemory
	saved           *model.EpisodeMemory
	expectedVersion uint64
	saveErr         error
}

func (s *stubProjectionRepository) GetBySession(context.Context, int64, string) (*model.EpisodeMemory, error) {
	if s.existing == nil {
		return nil, episodedao.ErrEpisodeNotFound
	}
	copy := *s.existing
	return &copy, nil
}

func (s *stubProjectionRepository) Save(_ context.Context, episode *model.EpisodeMemory, expectedVersion uint64) error {
	copy := *episode
	copy.Version = expectedVersion + 1
	s.saved, s.expectedVersion = &copy, expectedVersion
	if s.saveErr == nil {
		episode.Version = copy.Version
	}
	return s.saveErr
}

func validProjectionDocument() model.EpisodeDocument {
	return model.EpisodeDocument{
		Synopsis: "用户排查了 RPC 粘包。", Topics: []model.EpisodeTopic{{Key: "rpc-粘包", Label: "RPC 粘包"}},
		Decisions: []model.EpisodeEvidence{{Text: "采用长度前缀", EvidenceMessageIDs: []uint{1}}}, SourceHandles: []string{"message:1"},
	}
}

func validProjectionEvidence() ProjectionEvidence {
	now := time.Now()
	return ProjectionEvidence{
		UserID: 7, Session: model.Session{ID: "session-1", UserID: 7, Title: "RPC"}, TargetMessageID: 1, TargetMessageAt: now,
		Messages: []model.Message{{ID: 1, SessionID: "session-1", IsUser: true, Content: "采用长度前缀", CreatedAt: now}},
	}
}

func TestProjectorCreatesShortSessionWithoutSummary(t *testing.T) {
	evidence := validProjectionEvidence()
	repository := &stubProjectionRepository{}
	generator := &stubGenerator{document: validProjectionDocument()}
	projector, _ := NewProjector(stubEvidenceLoader{evidence: evidence}, generator, repository, allowAll())
	episode, err := projector.Project(context.Background(), 7, "session-1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if repository.saved == nil || repository.expectedVersion != 0 || episode.SourceLastMessageID != 1 || episode.SourceSummaryVersion != 0 || episode.ID == "" {
		t.Fatalf("episode=%#v expected=%d", episode, repository.expectedVersion)
	}
	if repository.saved.Title != "RPC" || repository.saved.IndexStatus != model.EpisodeIndexPending {
		t.Fatalf("saved=%#v", repository.saved)
	}
}

func TestProjectorPersistsAssistantEpisodeDependencyClosure(t *testing.T) {
	evidence := validProjectionEvidence()
	evidence.TargetMessageID = 2
	evidence.TargetMessageAt = evidence.TargetMessageAt.Add(time.Second)
	evidence.Messages = append(evidence.Messages, model.Message{
		ID: 2, SessionID: "session-1", IsUser: false, Content: "recalled answer", CreatedAt: evidence.TargetMessageAt,
		EpisodeSources: model.EpisodeSourceRefs{{EpisodeID: "episode-a", Version: 2}, {EpisodeID: "episode-parent", Version: 4}},
	})
	repository := &stubProjectionRepository{}
	projector, _ := NewProjector(stubEvidenceLoader{evidence: evidence}, &stubGenerator{document: validProjectionDocument()}, repository, allowAll())
	episode, err := projector.Project(context.Background(), 7, "session-1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(episode.SourceEpisodes) != 2 || episode.SourceEpisodes[0].EpisodeID != "episode-a" || episode.SourceEpisodes[1].EpisodeID != "episode-parent" {
		t.Fatalf("source closure=%+v", episode.SourceEpisodes)
	}
}

func TestProjectionEvidenceRejectsUserOwnedEpisodeSources(t *testing.T) {
	evidence := validProjectionEvidence()
	evidence.Messages[0].EpisodeSources = model.EpisodeSourceRefs{{EpisodeID: "forged", Version: 1}}
	if err := evidence.Validate(); err == nil {
		t.Fatal("user message episode provenance must be rejected")
	}
}

func TestProjectorUpdatesSameEpisodeAndSummaryWatermark(t *testing.T) {
	evidence := validProjectionEvidence()
	evidence.TargetMessageID = 2
	evidence.TargetMessageAt = evidence.TargetMessageAt.Add(time.Second)
	evidence.Messages = []model.Message{{ID: 2, SessionID: "session-1", IsUser: true, Content: "继续处理", CreatedAt: evidence.TargetMessageAt}}
	evidence.Summary = &model.SessionSummary{UserID: 7, SessionID: "session-1", Content: `{}`, CoveredMessageID: 1, Version: 3}
	evidence.Existing = &model.EpisodeMemory{ID: "episode-stable", UserID: 7, SessionID: "session-1", Version: 4, SourceLastMessageID: 1}
	repository := &stubProjectionRepository{}
	// This fixture replaces the evidence with message 2 only, so the document
	// has to cite message 2. Before Phase 2-C1 it could cite message 1 and still
	// be saved, because the evidence boundary ran only inside ModelGenerator and
	// a stub generator walked straight past it.
	document := validProjectionDocument()
	document.Decisions = []model.EpisodeEvidence{
		{Text: "采用长度前缀", EvidenceMessageIDs: []uint{2}}}
	document.SourceHandles = []string{"message:2"}
	projector, _ := NewProjector(stubEvidenceLoader{evidence: evidence}, &stubGenerator{document: document}, repository, allowAll())
	episode, err := projector.Project(context.Background(), 7, "session-1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if episode.ID != "episode-stable" || repository.expectedVersion != 4 || episode.SourceSummaryVersion != 3 || episode.SourceSummaryWatermark != 1 {
		t.Fatalf("episode=%#v expected=%d", episode, repository.expectedVersion)
	}
}

func TestProjectorDoesNotOverwriteOldEpisodeOnGenerationFailure(t *testing.T) {
	evidence := validProjectionEvidence()
	evidence.Existing = &model.EpisodeMemory{ID: "old", UserID: 7, SessionID: "session-1", Version: 2}
	repository := &stubProjectionRepository{}
	generator := &stubGenerator{err: ErrEpisodeToolProtocol}
	projector, _ := NewProjector(stubEvidenceLoader{evidence: evidence}, generator, repository, allowAll())
	_, err := projector.Project(context.Background(), 7, "session-1", 1)
	if !errors.Is(err, ErrEpisodeToolProtocol) || repository.saved != nil {
		t.Fatalf("error=%v saved=%#v", err, repository.saved)
	}
}

func TestProjectorSkipsAlreadyCoveredTarget(t *testing.T) {
	evidence := validProjectionEvidence()
	evidence.Existing = &model.EpisodeMemory{ID: "current", UserID: 7, SessionID: "session-1", Version: 2, SourceLastMessageID: 5}
	repository := &stubProjectionRepository{}
	generator := &stubGenerator{document: validProjectionDocument()}
	projector, _ := NewProjector(stubEvidenceLoader{evidence: evidence}, generator, repository, allowAll())
	episode, err := projector.Project(context.Background(), 7, "session-1", 1)
	if err != nil || episode.ID != "current" || generator.calls != 0 || repository.saved != nil {
		t.Fatalf("episode=%#v error=%v calls=%d", episode, err, generator.calls)
	}
}

func TestProjectorRejectsSensitiveSessionTitle(t *testing.T) {
	evidence := validProjectionEvidence()
	evidence.Session.Title = "password=do-not-store"
	repository := &stubProjectionRepository{}
	projector, _ := NewProjector(stubEvidenceLoader{evidence: evidence}, &stubGenerator{document: validProjectionDocument()}, repository, allowAll())
	_, err := projector.Project(context.Background(), 7, "session-1", 1)
	if !errors.Is(err, ErrEpisodeSensitive) || repository.saved != nil {
		t.Fatalf("error=%v saved=%#v", err, repository.saved)
	}
}
