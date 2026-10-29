package memory

import (
	memorydao "GopherAI/dao/memory"
	"GopherAI/model"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cloudwego/eino/schema"
	einojsonschema "github.com/eino-contrib/jsonschema"
)

type governanceRepositoryFake struct {
	claim        *memorydao.DreamClaim
	claimErr     error
	comparable   []model.LongTermMemory
	tombstone    bool
	apply        memorydao.ApplyDreamParams
	applyErr     error
	failureCode  string
	recovered    int64
	recoverCalls int
	pendingCalls int
}

func (r *governanceRepositoryFake) ClaimCandidate(context.Context, int64, string, time.Time) (*memorydao.DreamClaim, error) {
	return r.claim, r.claimErr
}
func (r *governanceRepositoryFake) RecoverExpiredDreams(context.Context, int64, time.Time, time.Duration) (int64, error) {
	r.recoverCalls++
	return r.recovered, nil
}
func (r *governanceRepositoryFake) ListComparable(context.Context, int64, model.MemoryScope, []string, string, int) ([]model.LongTermMemory, error) {
	return append([]model.LongTermMemory(nil), r.comparable...), nil
}
func (r *governanceRepositoryFake) HasDeletedHash(context.Context, int64, model.MemoryScope, string) (bool, error) {
	return r.tombstone, nil
}
func (r *governanceRepositoryFake) ApplyDream(_ context.Context, params memorydao.ApplyDreamParams) (*memorydao.DreamApplyResult, error) {
	r.apply = params
	if r.applyErr != nil {
		return nil, r.applyErr
	}
	run := r.claim.Run
	run.Status, run.Action = model.MemoryDreamRunCompleted, params.Action
	return &memorydao.DreamApplyResult{Run: run, Memory: params.ResultMemory, Revision: model.MemoryRevision{ID: "revision", UserID: params.UserID, Action: params.Action, Actor: model.MemoryRevisionDream, Reason: params.Reason}}, nil
}
func (r *governanceRepositoryFake) FailDream(_ context.Context, _ int64, _, _, errorCode string, _ time.Time) error {
	r.failureCode = errorCode
	return nil
}
func (r *governanceRepositoryFake) ListMemories(context.Context, memorydao.MemoryListQuery) ([]model.LongTermMemory, error) {
	return nil, nil
}
func (r *governanceRepositoryFake) PendingCandidateCount(context.Context, int64) (int64, error) {
	r.pendingCalls++
	return 0, nil
}
func (r *governanceRepositoryFake) ListRevisions(context.Context, int64, string, int) ([]model.MemoryRevision, error) {
	return nil, nil
}
func (r *governanceRepositoryFake) CorrectMemory(context.Context, memorydao.CorrectMemoryParams) (*model.LongTermMemory, *model.MemoryRevision, error) {
	return nil, nil, nil
}
func (r *governanceRepositoryFake) DeleteMemory(context.Context, int64, string, string, time.Time) (*model.MemoryRevision, error) {
	return nil, nil
}

type plannerFake struct {
	decision DreamDecision
	err      error
	calls    int
}

type blockingPlanner struct{}

func (blockingPlanner) Plan(ctx context.Context, _ model.MemoryCandidate, _ []model.LongTermMemory) (DreamDecision, error) {
	<-ctx.Done()
	return DreamDecision{}, ctx.Err()
}

func (p *plannerFake) Plan(context.Context, model.MemoryCandidate, []model.LongTermMemory) (DreamDecision, error) {
	p.calls++
	return p.decision, p.err
}

type progressFake struct{ progress *model.TopicProgress }

func (p progressFake) GetProgressForLearningSession(context.Context, int64, string) (*model.TopicProgress, error) {
	return p.progress, nil
}

func topicKeyFor(memoryType model.MemoryScope) string {
	if memoryType == model.MemoryScopeTopic {
		return "docker"
	}
	return ""
}

func durabilityFor(memoryType model.MemoryScope) model.MemoryDurability {
	return model.MemoryDurabilityStable
}

func dreamClaim(memoryType model.MemoryScope, event model.MemoryEventType) *memorydao.DreamClaim {
	content := "GopherAI runs in Docker"
	return &memorydao.DreamClaim{
		Candidate: model.MemoryCandidate{
			ID: "candidate", JobID: "job", UserID: 7, Scope: memoryType, Subject: "GopherAI deployment",
			ProposedContent: content, ContentHash: model.MemoryContentHash(content), StructuredJSON: `{}`,
			Tags: model.StringList{"docker", "gopherai", "deployment"},
			// AGENT-018 shape: a topic note is bound to a topic, a profile is not.
			TopicKey:   topicKeyFor(memoryType),
			Durability: durabilityFor(memoryType),
			SourceRefs: model.StringList{"agent_run:r"},
			Confidence: .9, Importance: 8, Sensitivity: model.MemorySensitivitySafe, Status: model.MemoryCandidateProcessing,
		},
		Job: model.MemoryExtractionJob{ID: "job", UserID: 7, EventType: event, SourceID: "source"},
		Run: model.MemoryDreamRun{ID: "dream", UserID: 7, CandidateID: "candidate", Status: model.MemoryDreamRunProcessing, StartedAt: time.Now()},
	}
}

func activeMemory(id, content string) model.LongTermMemory {
	return model.LongTermMemory{
		ID: id, UserID: 7, Scope: model.MemoryScopeTopic, Subject: "deployment", Content: content,
		ContentHash: model.MemoryContentHash(content), StructuredJSON: `{}`, Tags: model.StringList{"docker", "gopherai", "deployment"},
		SourceRefs: model.StringList{"agent_run:old"}, Confidence: .8, Importance: 7, Status: model.LongTermMemoryActive,
	}
}

func TestExplicitGovernanceDoesNotExposeCandidateDreamLifecycle(t *testing.T) {
	repository := new(governanceRepositoryFake)
	service, err := NewExplicitGovernanceService(repository, nil, DefaultDreamConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Process(context.Background(), DreamRequest{UserID: 7}); !errors.Is(err, ErrDreamNotConfigured) {
		t.Fatalf("Process() error = %v, want ErrDreamNotConfigured", err)
	}
	if _, err := service.List(context.Background(), ListRequest{UserID: 7, Status: model.LongTermMemoryActive}); err != nil {
		t.Fatal(err)
	}
	if repository.recoverCalls != 0 || repository.pendingCalls != 0 {
		t.Fatalf("retired lifecycle was queried: recover=%d pending=%d", repository.recoverCalls, repository.pendingCalls)
	}
}

func TestDreamAddsWithoutCallingPlannerAndReplaysCompletedCandidate(t *testing.T) {
	repository := &governanceRepositoryFake{claim: dreamClaim(model.MemoryScopeTopic, model.MemoryEventAgentRun)}
	planner := new(plannerFake)
	service, err := NewGovernanceService(repository, planner, nil, DefaultDreamConfig())
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Process(context.Background(), DreamRequest{UserID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if planner.calls != 0 || repository.apply.Action != model.MemoryDreamAdd || result.Memory == nil || result.Memory.OriginCandidateID == nil || *result.Memory.OriginCandidateID != "candidate" {
		t.Fatalf("calls=%d apply=%+v result=%+v", planner.calls, repository.apply, result)
	}

	repository.claim.Replay = true
	repository.claim.Run.Status = model.MemoryDreamRunCompleted
	result, err = service.Process(context.Background(), DreamRequest{UserID: 7, CandidateID: "candidate"})
	if err != nil || !result.Replay || planner.calls != 0 {
		t.Fatalf("replay=%+v err=%v calls=%d", result, err, planner.calls)
	}
}

func TestDreamExactDuplicateMergesSourcesDeterministically(t *testing.T) {
	claim := dreamClaim(model.MemoryScopeTopic, model.MemoryEventAgentRun)
	target := activeMemory("old", claim.Candidate.ProposedContent)
	repository := &governanceRepositoryFake{claim: claim, comparable: []model.LongTermMemory{target}}
	planner := new(plannerFake)
	service, _ := NewGovernanceService(repository, planner, nil, DefaultDreamConfig())
	_, err := service.Process(context.Background(), DreamRequest{UserID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if planner.calls != 0 || repository.apply.Action != model.MemoryDreamMerge || repository.apply.ResultMemory.ID != target.ID || len(repository.apply.ResultMemory.SourceRefs) != 2 {
		t.Fatalf("calls=%d params=%+v", planner.calls, repository.apply)
	}
}

func TestDreamCannotOverrideUserConfirmedMemoryWithoutExplicitCorrection(t *testing.T) {
	claim := dreamClaim(model.MemoryScopeTopic, model.MemoryEventAgentRun)
	target := activeMemory("confirmed", "GopherAI runs on a host")
	target.UserConfirmed = true
	repository := &governanceRepositoryFake{claim: claim, comparable: []model.LongTermMemory{target}}
	planner := &plannerFake{decision: DreamDecision{Action: model.MemoryDreamSupersede, TargetMemoryID: target.ID, Reason: "newer"}}
	service, _ := NewGovernanceService(repository, planner, nil, DefaultDreamConfig())
	_, err := service.Process(context.Background(), DreamRequest{UserID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if repository.apply.Action != model.MemoryDreamReject || repository.apply.TargetMemoryID != "" || repository.apply.ResultMemory != nil {
		t.Fatalf("user-confirmed target was not protected: %+v", repository.apply)
	}
}

func TestExplicitRememberCanSupersedeAndRemainsUserConfirmed(t *testing.T) {
	claim := dreamClaim(model.MemoryScopeTopic, model.MemoryEventExplicitRemember)
	target := activeMemory("confirmed", "GopherAI runs on a host")
	target.UserConfirmed = true
	repository := &governanceRepositoryFake{claim: claim, comparable: []model.LongTermMemory{target}}
	planner := &plannerFake{decision: DreamDecision{Action: model.MemoryDreamSupersede, TargetMemoryID: target.ID, Reason: "explicit user correction"}}
	service, _ := NewGovernanceService(repository, planner, nil, DefaultDreamConfig())
	_, err := service.Process(context.Background(), DreamRequest{UserID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if repository.apply.Action != model.MemoryDreamSupersede || !repository.apply.AllowUserConfirmed || repository.apply.ResultMemory == nil || !repository.apply.ResultMemory.UserConfirmed {
		t.Fatalf("explicit correction was not authoritative: %+v", repository.apply)
	}
}

// AGENT-018 removed mastery from long-term memory entirely. It is structured
// state in TopicProgress, read live by the agent, so Dream has nothing to
// promote and nothing to keep in sync. The previous test asserted that a
// learning candidate was rebuilt from authoritative progress; the guarantee is
// now stronger, because no such candidate can be produced in the first place.
func TestLearningEvidenceProducesNoMemoryTools(t *testing.T) {
	if tools := extractionTools(EvidenceAuthorityLearningAssessment); len(tools) != 0 {
		t.Fatalf("a graded quiz must not offer any memory tool, got %d", len(tools))
	}
}

func TestDreamPlannerFailureReturnsCandidateToPending(t *testing.T) {
	claim := dreamClaim(model.MemoryScopeTopic, model.MemoryEventAgentRun)
	repository := &governanceRepositoryFake{claim: claim, comparable: []model.LongTermMemory{activeMemory("old", "older fact")}}
	planner := &plannerFake{err: errors.New("provider unavailable")}
	service, _ := NewGovernanceService(repository, planner, nil, DefaultDreamConfig())
	if _, err := service.Process(context.Background(), DreamRequest{UserID: 7}); err == nil {
		t.Fatal("planner failure accepted")
	}
	if repository.failureCode != "dream_failed" {
		t.Fatalf("failure code=%q", repository.failureCode)
	}
}

func TestDreamTimeoutReturnsCandidateToPending(t *testing.T) {
	claim := dreamClaim(model.MemoryScopeTopic, model.MemoryEventAgentRun)
	repository := &governanceRepositoryFake{claim: claim, comparable: []model.LongTermMemory{activeMemory("old", "older fact")}}
	config := DefaultDreamConfig()
	config.Timeout = 5 * time.Millisecond
	service, _ := NewGovernanceService(repository, blockingPlanner{}, nil, config)
	if _, err := service.Process(context.Background(), DreamRequest{UserID: 7}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout err=%v", err)
	}
	if repository.failureCode != "dream_timeout" {
		t.Fatalf("failure code=%q", repository.failureCode)
	}
}

func TestDecodeDreamDecisionRejectsUnknownFieldsAndCorrectionTagsAreStrict(t *testing.T) {
	if _, err := decodeDreamDecision(`{"action":"add","target_memory_id":"","merged_subject":"","merged_content":"","reason":"new","extra":true}`); err == nil {
		t.Fatal("unknown dream field accepted")
	}
	if _, err := strictTags([]string{"Go", "project"}); err == nil {
		t.Fatal("non-canonical correction tag accepted")
	}
}

func TestModelDreamPlannerMapsRelationshipToolsToLifecycleActions(t *testing.T) {
	tests := []struct {
		name      string
		tool      string
		arguments string
		action    model.MemoryDreamAction
		target    string
	}{
		{name: "independent", tool: dreamToolIndependent, arguments: `{"reason":"new"}`, action: model.MemoryDreamAdd},
		{name: "complementary", tool: dreamToolComplementary, arguments: `{"target_memory_id":"old","merged_subject":"merged","merged_content":"both facts","reason":"adds detail"}`, action: model.MemoryDreamMerge, target: "old"},
		{name: "replacement", tool: dreamToolReplacement, arguments: `{"target_memory_id":"old","reason":"new value"}`, action: model.MemoryDreamSupersede, target: "old"},
		{name: "retraction", tool: dreamToolRetraction, arguments: `{"target_memory_id":"old","reason":"removed without replacement"}`, action: model.MemoryDreamArchive, target: "old"},
		{name: "duplicate", tool: dreamToolDuplicate, arguments: `{"target_memory_id":"old","reason":"same meaning"}`, action: model.MemoryDreamReject},
		{name: "unsupported", tool: dreamToolUnsupported, arguments: `{"reason":"temporary"}`, action: model.MemoryDreamReject},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			chat := &memoryChat{response: toolMessage(tc.tool, tc.arguments)}
			planner, _ := NewModelDreamPlanner(chat)
			decision, err := planner.Plan(context.Background(), dreamClaim(model.MemoryScopeTopic, model.MemoryEventExplicitRemember).Candidate, []model.LongTermMemory{activeMemory("old", "old fact")})
			if err != nil || decision.Action != tc.action || decision.TargetMemoryID != tc.target || len(chat.tools) != 6 {
				t.Fatalf("decision=%+v tools=%d err=%v", decision, len(chat.tools), err)
			}
		})
	}
}

func TestModelDreamPlannerRejectsPlainJSONAndUnauthorizedTool(t *testing.T) {
	for _, response := range []*schema.Message{
		schema.AssistantMessage(`{"action":"merge"}`, nil),
		toolMessage("invented_relation", `{"reason":"x"}`),
	} {
		chat := &memoryChat{response: response}
		planner, _ := NewModelDreamPlanner(chat)
		if _, err := planner.Plan(context.Background(), dreamClaim(model.MemoryScopeTopic, model.MemoryEventExplicitRemember).Candidate, []model.LongTermMemory{activeMemory("old", "old fact")}); !errors.Is(err, ErrInvalidDreamOutput) {
			t.Fatalf("error=%v", err)
		}
	}
}

// Scale-1 lost five governance decisions to this: the model returned a valid
// relationship, and the whole thing was discarded because the explanation ran
// past the column width by a few characters. The relationship is the operative
// output; the reason is documentation written into the revision.
func TestDreamKeepsDecisionWhenReasonOverrunsTheColumn(t *testing.T) {
	long := strings.Repeat("因", dreamReasonMaxRunes+56)

	bounded := boundDreamReason(long)

	if got := utf8.RuneCountInString(bounded); got > dreamReasonMaxRunes {
		t.Fatalf("bounded reason is %d runes, column holds %d", got, dreamReasonMaxRunes)
	}
	// A cut explanation must not read as a complete one.
	if !strings.HasSuffix(bounded, "…") {
		t.Fatalf("truncation was not marked: %q", bounded[len(bounded)-12:])
	}
}

func TestDreamLeavesAFittingReasonUntouched(t *testing.T) {
	reason := "候选与目标是同一持久事实的不同措辞"
	if got := boundDreamReason(reason); got != reason {
		t.Fatalf("reason was rewritten:\n got %q\nwant %q", got, reason)
	}
	// Empty is a contract violation rather than a length problem, and the
	// caller still refuses it.
	if got := boundDreamReason("   "); got != "" {
		t.Fatalf("blank reason became %q", got)
	}
}

// The published schema has to say what the server accepts. It previously asked
// for a "concise" reason while rejecting anything past the column, and allowed
// extra properties while decodeStrictJSON refused them — a model obeying the
// contract could still be rejected.
func TestDreamToolSchemaPublishesTheLimitsTheServerEnforces(t *testing.T) {
	for _, tool := range dreamRelationTools() {
		document, err := tool.ParamsOneOf.ToJSONSchema()
		if err != nil {
			t.Fatalf("%s: %v", tool.Name, err)
		}
		if document.AdditionalProperties != einojsonschema.FalseSchema {
			t.Fatalf("%s allows properties the strict decoder refuses", tool.Name)
		}
		reason, ok := document.Properties.Get("reason")
		if !ok || reason == nil {
			t.Fatalf("%s has no reason parameter", tool.Name)
		}
		if reason.MaxLength == nil || *reason.MaxLength != uint64(dreamReasonMaxRunes) {
			t.Fatalf("%s does not publish the reason limit: %v", tool.Name, reason.MaxLength)
		}
	}
}
