package memory

import (
	memorydao "GopherAI/dao/memory"
	"GopherAI/model"
	"context"
	"errors"
	"testing"
	"time"
)

// These tests are experiment-only fault traces for EXP-002A. They exercise
// failure boundaries that are intentionally not reachable from the HTTP API.
func TestEXP002AExtractorIndependentFaultMatrix(t *testing.T) {
	base := `{"subject":"scope","proposed_content":"project scope","confidence":0.8,"importance":5,"sensitive":false}`
	tests := []struct {
		name         string
		responseName string
		arguments    string
		nilResponse  bool
		wantCode     string
	}{
		{name: "empty_response", nilResponse: true, wantCode: "empty_response"},
		{name: "unknown_tag_field_is_not_accepted", responseName: toolRememberTopic, arguments: `{"subject":"scope","proposed_content":"project scope","confidence":0.8,"importance":5,"sensitive":false,"tags":["invented"]}`, wantCode: "unknown_field"},
		{name: "illegal_memory_type_tool_is_not_accepted", responseName: "remember_preference", arguments: base, wantCode: "tool_protocol"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			chat := &memoryChat{}
			if !tc.nilResponse {
				chat.response = toolMessage(tc.responseName, tc.arguments)
			}
			extractor, err := NewModelExtractor(chat)
			if err != nil {
				t.Fatal(err)
			}
			_, err = extractor.Extract(context.Background(), Evidence{EventType: model.MemoryEventExplicitRemember, Authority: EvidenceAuthorityUserConfirmed, UserID: 1, SourceID: "fault", SourceRefs: []string{"explicit_remember:fault"}, Content: "remember project scope"})
			if code := ExtractionErrorCode(err); code != tc.wantCode {
				t.Fatalf("code=%q err=%v want=%q", code, err, tc.wantCode)
			}
		})
	}
}

func TestEXP002APlannerHallucinatedTargetFailsClosed(t *testing.T) {
	claim := dreamClaim(model.MemoryScopeTopic, model.MemoryEventExplicitRemember)
	target := activeMemory("real-target", "older fact")
	repository := &governanceRepositoryFake{claim: claim, comparable: []model.LongTermMemory{target}}
	planner := &plannerFake{decision: DreamDecision{Action: model.MemoryDreamMerge, TargetMemoryID: "hallucinated-id", MergedSubject: "merged", MergedContent: "merged fact", Reason: "merge"}}
	service, _ := NewGovernanceService(repository, planner, nil, DefaultDreamConfig())
	_, err := service.Process(context.Background(), DreamRequest{UserID: claim.Candidate.UserID, CandidateID: claim.Candidate.ID})
	if !errors.Is(err, ErrInvalidDreamOutput) || repository.failureCode != "invalid_dream_decision" || repository.apply.Action != "" {
		t.Fatalf("err=%v failure=%q apply=%+v", err, repository.failureCode, repository.apply)
	}
}

func TestEXP002APlannerInvalidActionAndMissingTargetFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		decision DreamDecision
	}{
		{name: "invalid_action", decision: DreamDecision{Action: model.MemoryDreamAction("invented"), Reason: "invalid"}},
		{name: "missing_target", decision: DreamDecision{Action: model.MemoryDreamArchive, Reason: "missing target"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claim := dreamClaim(model.MemoryScopeTopic, model.MemoryEventExplicitRemember)
			target := activeMemory("real-target", "older fact")
			repository := &governanceRepositoryFake{claim: claim, comparable: []model.LongTermMemory{target}}
			planner := &plannerFake{decision: tc.decision}
			service, _ := NewGovernanceService(repository, planner, nil, DefaultDreamConfig())
			_, err := service.Process(context.Background(), DreamRequest{UserID: claim.Candidate.UserID, CandidateID: claim.Candidate.ID})
			if !errors.Is(err, ErrInvalidDreamOutput) || repository.failureCode != "invalid_dream_decision" || repository.apply.Action != "" {
				t.Fatalf("err=%v failure=%q apply=%+v", err, repository.failureCode, repository.apply)
			}
		})
	}
}

func TestEXP002APlannerDecisionBecomesStaleAfterCorrection(t *testing.T) {
	claim := dreamClaim(model.MemoryScopeTopic, model.MemoryEventExplicitRemember)
	target := activeMemory("corrected-target", "older fact")
	repository := &governanceRepositoryFake{claim: claim, comparable: []model.LongTermMemory{target}, applyErr: memorydao.ErrMemoryConflict}
	planner := &plannerFake{decision: DreamDecision{Action: model.MemoryDreamSupersede, TargetMemoryID: target.ID, Reason: "new explicit fact"}}
	service, _ := NewGovernanceService(repository, planner, nil, DefaultDreamConfig())
	_, err := service.Process(context.Background(), DreamRequest{UserID: claim.Candidate.UserID, CandidateID: claim.Candidate.ID})
	if !errors.Is(err, memorydao.ErrMemoryConflict) || repository.failureCode != "stale_dream_decision" {
		t.Fatalf("err=%v failure=%q", err, repository.failureCode)
	}
}

func TestEXP002AExplicitRememberMayRebuildDeletedFact(t *testing.T) {
	claim := dreamClaim(model.MemoryScopeTopic, model.MemoryEventExplicitRemember)
	repository := &governanceRepositoryFake{claim: claim, tombstone: true}
	planner := new(plannerFake)
	service, _ := NewGovernanceService(repository, planner, nil, DefaultDreamConfig())
	result, err := service.Process(context.Background(), DreamRequest{UserID: claim.Candidate.UserID, CandidateID: claim.Candidate.ID})
	if err != nil || planner.calls != 0 || repository.apply.Action != model.MemoryDreamAdd || result.Memory == nil || !result.Memory.UserConfirmed {
		t.Fatalf("result=%+v err=%v calls=%d apply=%+v", result, err, planner.calls, repository.apply)
	}
}

type completeFailRepository struct {
	*workerRepository
	completeCalls int
}

func (r *completeFailRepository) Complete(context.Context, *model.MemoryExtractionJob, *model.MemoryCandidate, model.MemoryJobStatus, string, time.Time) error {
	r.completeCalls++
	return errors.New("injected completion database failure")
}

func TestEXP002AWorkerCompletionFailureLeavesLeaseForRecovery(t *testing.T) {
	job := model.MemoryExtractionJob{ID: "fault-job", UserID: 1, EventType: model.MemoryEventAgentRun, SourceID: "fault", Status: model.MemoryJobPending, AvailableAt: time.Now()}
	base := &workerRepository{jobs: []model.MemoryExtractionJob{job}}
	repository := &completeFailRepository{workerRepository: base}
	content := "verified durable fact"
	candidate := &model.MemoryCandidate{ID: "fault-candidate", UserID: 1, Scope: model.MemoryScopeTopic, TopicKey: "project", Subject: "fact", ProposedContent: content, ContentHash: model.MemoryContentHash(content), StructuredJSON: `{}`, Tags: model.StringList{"project"}, SourceRefs: model.StringList{"agent_run:fault"}, Confidence: .9, Importance: 7, Sensitivity: model.MemorySensitivitySafe, Status: model.MemoryCandidatePending}
	worker, err := NewWorker(repository, fixedLoader{}, fixedExtractor{candidate: candidate}, DefaultWorkerConfig())
	if err != nil {
		t.Fatal(err)
	}
	worker.process(context.Background(), job)
	base.mu.Lock()
	defer base.mu.Unlock()
	if repository.completeCalls != 1 || base.retried != 0 || base.completed != 0 || base.rejected != 0 {
		t.Fatalf("complete=%d retried=%d completed=%d rejected=%d", repository.completeCalls, base.retried, base.completed, base.rejected)
	}
}
