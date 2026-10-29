package model

import (
	"strings"
	"testing"
	"time"
)

func TestMemoryCandidateValidation(t *testing.T) {
	valid := &MemoryCandidate{ID: "c", JobID: "j", UserID: 1, Scope: MemoryScopeTopic, TopicKey: "docker", Durability: MemoryDurabilityStable, Subject: "容器改代码要重建镜像", ProposedContent: "用户踩过坑：只重启容器没重建镜像，跑的还是旧代码", ContentHash: MemoryContentHash("用户踩过坑：只重启容器没重建镜像，跑的还是旧代码"), StructuredJSON: `{}`, Tags: StringList{"docker", "gopherai", "deployment"}, SourceRefs: StringList{"agent_run:r"}, Confidence: .9, Importance: 7, Sensitivity: MemorySensitivitySafe, Status: MemoryCandidatePending}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	copy := *valid
	copy.Scope = "free_form"
	if err := copy.Validate(); err == nil {
		t.Fatal("invalid memory type accepted")
	}
	copy = *valid
	copy.SourceRefs = nil
	if err := copy.Validate(); err == nil {
		t.Fatal("candidate without source accepted")
	}

	// AGENT-018: the per-type shape is what makes the two-type taxonomy
	// decidable. A topic note without a topic, or a profile carrying one, would
	// blur exactly the boundary the migration exists to keep sharp.
	copy = *valid
	copy.TopicKey = ""
	if err := copy.Validate(); err == nil {
		t.Fatal("topic note without a topic key accepted")
	}
	copy = *valid
	copy.Scope = MemoryTypeProfile
	copy.Durability = MemoryDurabilityStable
	if err := copy.Validate(); err == nil {
		t.Fatal("profile bound to a technical topic accepted")
	}
	copy.TopicKey = ""
	if err := copy.Validate(); err != nil {
		t.Fatalf("valid profile rejected: %v", err)
	}
	copy.Durability = ""
	if err := copy.Validate(); err == nil {
		t.Fatal("profile without durability accepted")
	}

	// Retired types must not become writable again by accident.
	for _, legacy := range []MemoryType{MemoryTypeLegacyUserProfile, MemoryTypeLegacyLearningState, MemoryTypeLegacyProjectFact, MemoryTypeLegacyExperience} {
		if !legacy.Legacy() {
			t.Errorf("%s should be recognised as a retired type", legacy)
		}
		if legacy.Valid() {
			t.Errorf("retired type %s must not be valid for new writes", legacy)
		}
		copy = *valid
		copy.Scope = legacy
		if err := copy.Validate(); err == nil {
			t.Errorf("retired type %s accepted for a new candidate", legacy)
		}
	}

	// The content ceiling dropped from 2000 to 400: five memories at the old
	// limit could never fit the 2000-token memory budget.
	copy = *valid
	copy.ProposedContent = strings.Repeat("测", MemoryContentMaxRunes+1)
	copy.ContentHash = MemoryContentHash(copy.ProposedContent)
	if err := copy.Validate(); err == nil {
		t.Fatal("over-long content accepted")
	}
}

func TestMemoryJobRequiresPendingInitialState(t *testing.T) {
	job := &MemoryExtractionJob{ID: "j", UserID: 1, EventType: MemoryEventAgentRun, SourceID: "r", Status: MemoryJobPending, AvailableAt: time.Now()}
	if err := job.ValidateForCreate(); err != nil {
		t.Fatal(err)
	}
	job.Status = MemoryJobProcessing
	if err := job.ValidateForCreate(); err == nil {
		t.Fatal("processing job accepted for create")
	}
}

func TestLongTermMemoryValidation(t *testing.T) {
	memory := &LongTermMemory{
		ID: "m", UserID: 1, Scope: MemoryTypeTopicNote, Subject: "RPC Watch",
		Content: "Watch revision 是当前薄弱点", ContentHash: MemoryContentHash("Watch revision 是当前薄弱点"), StructuredJSON: `{}`, Tags: StringList{"rpc", "watch", "learning"},
		TopicKey: "rpc", SourceRefs: StringList{"learning_attempt:a"}, Confidence: .8, Importance: 7, Status: LongTermMemoryActive,
		Durability: MemoryDurabilityStable,
	}
	if err := memory.Validate(); err != nil {
		t.Fatal(err)
	}
	memory.Status = "pending"
	if err := memory.Validate(); err == nil {
		t.Fatal("invalid long-term memory status accepted")
	}
}

func TestMemoryDurabilityControlsValidUntil(t *testing.T) {
	created := time.Now().UTC().Add(-time.Hour)
	expires := created.Add(2 * time.Hour)
	memory := &LongTermMemory{
		ID: "m", UserID: 1, Scope: MemoryScopeTopic, TopicKey: "rpc", Subject: "RPC", Content: "理解",
		ContentHash: MemoryContentHash("理解"), StructuredJSON: `{}`, Tags: StringList{}, SourceRefs: StringList{"source:test"},
		Durability: MemoryDurabilityTimeBound, ValidUntil: &expires, Confidence: .8, Importance: 7, Status: LongTermMemoryActive, CreatedAt: created,
	}
	if err := memory.Validate(); err != nil {
		t.Fatal(err)
	}
	if memory.ExpiredAt(created) || !memory.ExpiredAt(expires) {
		t.Fatal("expiry boundary is not half-open")
	}
	memory.ValidUntil = nil
	if err := memory.Validate(); err == nil {
		t.Fatal("time_bound without valid_until accepted")
	}
	memory.Durability, memory.ValidUntil = MemoryDurabilityStable, &expires
	if err := memory.Validate(); err == nil {
		t.Fatal("stable memory with valid_until accepted")
	}
}

func TestDeletedMemoryRequiresBodylessTombstone(t *testing.T) {
	now := time.Now()
	hash := MemoryContentHash("deleted fact")
	memory := &LongTermMemory{
		ID: "m", UserID: 1, Scope: MemoryTypeTopicNote, ContentHash: hash,
		StructuredJSON: `{}`, Status: LongTermMemoryDeleted, TombstoneHash: hash, DeletedAt: &now,
	}
	if err := memory.Validate(); err != nil {
		t.Fatal(err)
	}
	memory.Content = "deleted fact"
	if err := memory.Validate(); err == nil {
		t.Fatal("deleted memory retained plaintext")
	}
}

func TestMemoryRevisionRequiresTargetExceptReject(t *testing.T) {
	revision := &MemoryRevision{ID: "r", UserID: 1, Action: MemoryDreamReject, Actor: MemoryRevisionDream, Reason: "duplicate"}
	if err := revision.Validate(); err != nil {
		t.Fatal(err)
	}
	revision.Action = MemoryDreamMerge
	if err := revision.Validate(); err == nil {
		t.Fatal("merge revision without target accepted")
	}
}

func TestMemoryDraftRequiresPendingAuthorizationShape(t *testing.T) {
	now := time.Now()
	content := "用户希望先看整体调用链，再进入代码锚点。"
	draft := &MemoryDraft{
		ID: "draft", UserID: 1, SessionID: "session", RunID: "run", IdempotencyKey: "request",
		Scope: MemoryScopeGlobal, Subject: "讲解顺序", Content: content, ContentHash: MemoryContentHash(content),
		Tags: StringList{"learning"}, Durability: MemoryDurabilityStable, ConflictIDs: StringList{}, Status: MemoryDraftPending, CreatedAt: now,
	}
	if err := draft.Validate(); err != nil {
		t.Fatal(err)
	}
	draft.MemoryID = "memory"
	if err := draft.Validate(); err == nil {
		t.Fatal("pending draft accepted an active memory result")
	}
	draft.MemoryID = ""
	draft.ContentHash = MemoryContentHash("model changed the text")
	if err := draft.Validate(); err == nil {
		t.Fatal("draft accepted a mismatched content hash")
	}
}

func TestMemoryRevisionRequestKeyIsBounded(t *testing.T) {
	empty := " "
	revision := &MemoryRevision{ID: "r", UserID: 1, MemoryID: "m", RequestKey: &empty, Action: MemoryDreamAdd, Actor: MemoryRevisionUser, Reason: "user confirmed"}
	if err := revision.Validate(); err == nil {
		t.Fatal("empty idempotency key accepted")
	}
	long := strings.Repeat("x", 129)
	revision.RequestKey = &long
	if err := revision.Validate(); err == nil {
		t.Fatal("oversized idempotency key accepted")
	}
}

// BUG-AGENT-029: AGENT-018 removed the per-type fallback tag but left the
// validation floor at one, so any memory whose wording misses the 30-word
// vocabulary could not be stored at all. Real notes hit this immediately:
// titles like "两个请求并发完成时，什么会回滚、什么不会" contain no vocabulary term.
func TestMemoryWithoutTagsIsStorable(t *testing.T) {
	content := "用户没搞懂两个请求并发完成时什么会回滚、什么不会。"
	note := &LongTermMemory{
		ID: "m", UserID: 1, Scope: MemoryTypeTopicNote, Subject: "并发回滚边界",
		Content: content, ContentHash: MemoryContentHash(content), StructuredJSON: `{}`,
		Tags: StringList{}, TopicKey: "并发回滚", SourceRefs: StringList{"agent_run:r"},
		Durability: MemoryDurabilityStable, Confidence: .8, Importance: 7, Status: LongTermMemoryActive,
	}
	if err := note.Validate(); err != nil {
		t.Fatalf("a topic note without tags must remain storable, it is still reachable by topic key: %v", err)
	}

	// A profile is injected wholesale and never retrieved by tag, so tags are
	// not what makes it reachable either.
	profile := &LongTermMemory{
		ID: "p", UserID: 1, Scope: MemoryTypeProfile, Subject: "讲解顺序",
		Content: "先讲整体链路再看代码。", ContentHash: MemoryContentHash("先讲整体链路再看代码。"),
		StructuredJSON: `{}`, Tags: StringList{}, Durability: MemoryDurabilityStable,
		SourceRefs: StringList{"agent_run:r"}, Confidence: .9, Importance: 8, Status: LongTermMemoryActive,
	}
	if err := profile.Validate(); err != nil {
		t.Fatalf("a profile without tags must remain storable: %v", err)
	}

	// The ceiling still holds: six tags is a contract violation, not a memory
	// that merely missed the vocabulary.
	profile.Tags = StringList{"go", "gin", "gorm", "mysql", "redis", "docker"}
	if err := profile.Validate(); err == nil {
		t.Fatal("more than five tags must still be rejected")
	}
}
