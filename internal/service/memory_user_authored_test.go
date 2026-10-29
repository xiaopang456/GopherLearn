package service

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

// The user-authored door: a person typing in a fact about themselves.
//
// These cases exist because the temptation this feature invites is to treat it as
// `CreateMemoryDraft` with one check removed. It is not: the draft path's provenance gate is
// what makes an agent's claim checkable, and a shared write path would either weaken that
// gate for the agent or demand a conversation from a statement that has none. So the rules
// below are the authored path's own - and each one is stated as what the code cannot do,
// because that is the part a later refactor is most likely to undo quietly.

func authoredFor(scope domain.MemoryScope, topicKey, content string) domain.UserAuthoredMemory {
	return domain.UserAuthoredMemory{Scope: scope, TopicKey: topicKey, Content: content}
}

// exportedFields lists what a struct asks for, by name, in order. It is how the tests
// below state a negative: not "the server ignores a field", which a caller can undo, but
// "there is no such field to send".
func exportedFields(target any) []string {
	structType := reflect.TypeOf(target)
	for structType.Kind() == reflect.Pointer {
		structType = structType.Elem()
	}
	names := make([]string, 0, structType.NumField())
	for index := 0; index < structType.NumField(); index++ {
		field := structType.Field(index)
		if !field.IsExported() {
			continue
		}
		names = append(names, field.Name)
	}
	sort.Strings(names)
	return names
}

func TestAddUserMemoryIsAlreadyDecided(t *testing.T) {
	ctx := context.Background()
	service, store, userID, sessionID, messageID := memoryFixture(t)

	for _, scope := range []domain.MemoryScope{domain.MemoryScopeGlobal, domain.MemoryScopeTopic} {
		topicKey := ""
		if scope == domain.MemoryScopeTopic {
			topicKey = "redis"
		}
		created, err := service.AddUserMemory(ctx, userID, authoredFor(scope, topicKey, "  我把部署交给了 SRE 值班  "))
		if err != nil {
			t.Fatal(err)
		}
		if created.Status != domain.MemoryStatusActive {
			t.Fatalf("%s: status = %q, want active in the same request", scope, created.Status)
		}
		if created.Origin != domain.MemoryOriginUserAuthored {
			t.Fatalf("%s: origin = %q, want %q", scope, created.Origin, domain.MemoryOriginUserAuthored)
		}
		if created.Content != "我把部署交给了 SRE 值班" {
			t.Fatalf("%s: content = %q, want the surrounding space trimmed", scope, created.Content)
		}
		if created.Confidence != 1 {
			t.Fatalf("%s: confidence = %v, want the server's own 1", scope, created.Confidence)
		}
		if created.SourceSessionID != "" || len(created.SourceMessageIDs) != 0 {
			t.Fatalf("%s: an authored memory grew provenance: %+v", scope, created)
		}
		// Listed as a fact, and nowhere in the queue that waits on a decision.
		if _, err := store.GetMemory(ctx, userID, created.MemoryID); err != nil {
			t.Fatalf("%s: the owner cannot read their own row: %v", scope, err)
		}
		switch scope {
		case domain.MemoryScopeGlobal:
			active, err := service.ActiveMemories(ctx, userID, domain.MemoryScopeGlobal, "")
			if err != nil || len(active) != 1 || active[0].MemoryID != created.MemoryID {
				t.Fatalf("global listing = %+v err %v, want the one hand-written row", active, err)
			}
		case domain.MemoryScopeTopic:
			active, err := service.ActiveMemories(ctx, userID, domain.MemoryScopeTopic, topicKey)
			if err != nil || len(active) != 1 || active[0].MemoryID != created.MemoryID {
				t.Fatalf("topic listing = %+v err %v, want the one hand-written row", active, err)
			}
		}
	}

	pending, err := service.PendingDrafts(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending drafts after two hand-written adds = %+v, want none: a memory the author just typed is not work waiting on a decision", pending)
	}
	// The draft path still works, and still lands pending: what changed is a second door,
	// not a looser one.
	proposed, created, err := service.ProposeMemoryDraft(ctx, userID, draftFor("", sessionID, messageID))
	if err != nil || !created || proposed.Status != domain.MemoryStatusPending {
		t.Fatalf("proposal = %+v created=%t err=%v, want a new pending draft", proposed, created, err)
	}
	if proposed.Origin != domain.MemoryOriginAgentProposed {
		t.Fatalf("a proposal stored as %q, want agent_proposed", proposed.Origin)
	}
	if err := proposed.Validate(); err != nil {
		t.Fatalf("the proposed row no longer validates: %v", err)
	}
}

// TestAddUserMemoryRefusesWhatOnlyAConversationCanSay is the scope rule and the shape rules,
// with the store read back after each one: a refusal that left a row behind would be the
// same outcome as success with an error attached.
func TestAddUserMemoryRefusesWhatOnlyAConversationCanSay(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name  string
		shape domain.UserAuthoredMemory
	}{
		{"an episode with no conversation behind it", authoredFor(domain.MemoryScopeEpisodic, "", "第一次把 AOF 调到 everysec 才敢上线")},
		{"an episode with a subject key", authoredFor(domain.MemoryScopeEpisodic, "redis", "一句话")},
		{"a scope this server does not have", authoredFor("profile", "", "一句话")},
		{"a global fact naming a subject", authoredFor(domain.MemoryScopeGlobal, "redis", "一句话")},
		{"a topic fact naming no subject", authoredFor(domain.MemoryScopeTopic, "", "一句话")},
		{"no sentence at all", authoredFor(domain.MemoryScopeGlobal, "", "   ")},
		{"a sentence past the stored limit", authoredFor(domain.MemoryScopeGlobal, "", strings.Repeat("长", domain.MaxMemoryContentRunes+1))},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			// One fixture per case: the assertion after each refusal is "the store is still
			// empty", and a shared store would turn one wrongly accepted write into a cascade
			// of failures that all blame the wrong case.
			service, store, userID, _, _ := memoryFixture(t)
			created, err := service.AddUserMemory(ctx, userID, testCase.shape)
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("error = %v (%+v), want an invalid request", err, created)
			}
			if created != nil {
				t.Fatalf("a refused add still returned %+v", created)
			}
			for _, status := range []domain.MemoryStatus{domain.MemoryStatusActive, domain.MemoryStatusPending} {
				rows, err := store.ListMemoriesByStatus(ctx, userID, status)
				if err != nil {
					t.Fatal(err)
				}
				if len(rows) != 0 {
					t.Fatalf("the refused write left %d %s row(s): %+v", len(rows), status, rows)
				}
			}
		})
	}
}

// TestTheAuthoredDoorHasNoFieldForAStateOrASource is a statement about the type, and it is
// here because the type is the only thing that makes it true. `UserAuthoredMemory` carries
// four names; confidence, status, memory id, timestamps, source session and source messages
// are not among them, so no caller - HTTP client, agent, or a future internal one - can hand
// this path a provenance to store or a state to arrive in. The confirmation gate cannot be
// walked around by a field nobody can set.
func TestTheAuthoredDoorHasNoFieldForAStateOrASource(t *testing.T) {
	got := exportedFields(domain.UserAuthoredMemory{})
	want := []string{"Content", "Scope", "TopicKey", "UserID"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("domain.UserAuthoredMemory fields = %v, want exactly %v: adding a field here means adding a rule about it to this test", got, want)
	}
	// And the same closed set on the wire, so the client cannot reach a field the type
	// happens to have kept private.
	for _, forbidden := range []string{"status", "confidence", "memory_id", "user_id", "origin",
		"source_session_id", "source_message_ids", "created_at", "updated_at"} {
		if fieldNamed(domain.UserAuthoredMemory{}, forbidden) {
			t.Fatalf("the authored request can carry %q", forbidden)
		}
	}
}

// fieldNamed reports whether a struct has an exported field whose JSON name is the one given
// (or the Go name, when no tag asks otherwise).
func fieldNamed(target any, jsonName string) bool {
	structType := reflect.TypeOf(target)
	for index := 0; index < structType.NumField(); index++ {
		field := structType.Field(index)
		if !field.IsExported() {
			continue
		}
		name := strings.TrimSpace(strings.Split(field.Tag.Get("json"), ",")[0])
		if name == "" {
			name = field.Name
		}
		if strings.EqualFold(name, jsonName) {
			return true
		}
	}
	return false
}

func TestAddUserMemoryTakesTheOwnerFromTheCaller(t *testing.T) {
	ctx := context.Background()
	service, store, userID, _, _ := memoryFixture(t)
	intruder, err := store.CreateUser(ctx, &domain.User{Email: "intruder@example.com", DisplayName: "Intruder"})
	if err != nil {
		t.Fatal(err)
	}

	// The payload names an account that really exists, and it still changes nothing: the
	// authenticated caller is the only owner this path will write for.
	shape := authoredFor(domain.MemoryScopeGlobal, "", "我习惯先看代码再读解释")
	shape.UserID = intruder.ID
	created, err := service.AddUserMemory(ctx, userID, shape)
	if err != nil {
		t.Fatal(err)
	}
	if created.UserID != userID {
		t.Fatalf("row owned by %q, want the authenticated caller %q", created.UserID, userID)
	}
	if rows, err := store.ListMemoriesByStatus(ctx, intruder.ID, domain.MemoryStatusActive); err != nil || len(rows) != 0 {
		t.Fatalf("the named account holds %d memory(s) err %v, want nothing written into it", len(rows), err)
	}
}

// TestAuthoredWriteReplacesTheAnswerForItsSubjectAndNothingElse checks the version rule from
// the service's side: one subject, one active answer - including when the previous answer was
// produced by the other path.
func TestAuthoredWriteReplacesTheAnswerForItsSubjectAndNothingElse(t *testing.T) {
	ctx := context.Background()
	service, _, userID, sessionID, messageID := memoryFixture(t)

	confirmed, err := service.CreateMemoryDraft(ctx, userID, draftFor("", sessionID, messageID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ConfirmMemoryDraft(ctx, userID, confirmed.MemoryID); err != nil {
		t.Fatal(err)
	}
	// A second subject, and an undecided proposal on the first one: neither is the row this
	// hand-written statement replaces.
	other, err := service.AddUserMemory(ctx, userID, authoredFor(domain.MemoryScopeTopic, "kafka", "Redlock 的安全性我不采信"))
	if err != nil {
		t.Fatal(err)
	}
	waiting, _, err := service.ProposeMemoryDraft(ctx, userID, domain.MemoryDraft{
		UserID: userID, Scope: domain.MemoryScopeGlobal, Content: "代理还想记一条",
		SourceSessionID: sessionID, SourceMessageIDs: []string{messageID},
	})
	if err != nil {
		t.Fatal(err)
	}

	replacement, err := service.AddUserMemory(ctx, userID, authoredFor(domain.MemoryScopeGlobal, "", "我现在主要写 Rust"))
	if err != nil {
		t.Fatal(err)
	}
	global, err := service.ActiveMemories(ctx, userID, domain.MemoryScopeGlobal, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(global) != 1 || global[0].MemoryID != replacement.MemoryID {
		t.Fatalf("global listing = %+v, want exactly the hand-written replacement", global)
	}
	if retired, err := service.Memory(ctx, userID, confirmed.MemoryID); err != nil ||
		retired.Status != domain.MemoryStatusSuperseded {
		t.Fatalf("the confirmed predecessor is %q err %v, want superseded", retired.Status, err)
	}
	if kept, err := service.ActiveMemories(ctx, userID, domain.MemoryScopeTopic, "kafka"); err != nil ||
		len(kept) != 1 || kept[0].MemoryID != other.MemoryID {
		t.Fatalf("an unrelated subject was disturbed: %+v err %v", kept, err)
	}
	if still, err := service.PendingDrafts(ctx, userID); err != nil || len(still) != 1 ||
		still[0].MemoryID != waiting.MemoryID {
		t.Fatalf("the pending candidate was disturbed: %+v err %v", still, err)
	}
}

func TestAddUserMemoryRefusesWithoutAnIdentityOrAStore(t *testing.T) {
	ctx := context.Background()
	service, _, userID, _, _ := memoryFixture(t)

	if _, err := service.AddUserMemory(ctx, "   ", authoredFor(domain.MemoryScopeGlobal, "", "一句话")); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("an anonymous add = %v, want unauthorized", err)
	}
	// A service assembled without a memory store says so instead of panicking, and the
	// authored door is no exception to that rule.
	empty := NewMemoryService(nil)
	if _, err := empty.AddUserMemory(ctx, userID, authoredFor(domain.MemoryScopeGlobal, "", "一句话")); !errors.Is(err, ErrInternal) {
		t.Fatalf("add with no store = %v, want the internal refusal", err)
	}
}

// TestProposalStillCannotBeStoredWithoutEvidence is the half that matters for the agent: the
// new door did not loosen the old one. A draft with no session, or with a session and no
// message, is still refused, and nothing about the authored path can be reached by a caller
// that has a *MemoryDraft in hand.
func TestProposalStillCannotBeStoredWithoutEvidence(t *testing.T) {
	ctx := context.Background()
	service, store, userID, sessionID, messageID := memoryFixture(t)

	withoutSession := draftFor("", sessionID, messageID)
	withoutSession.SourceSessionID = ""
	if _, err := service.CreateMemoryDraft(ctx, userID, withoutSession); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a draft with no source session = %v, want refused", err)
	}
	withoutMessages := draftFor("", sessionID, messageID)
	withoutMessages.SourceMessageIDs = nil
	if _, err := service.CreateMemoryDraft(ctx, userID, withoutMessages); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a draft with no source messages = %v, want refused", err)
	}
	// And the authored door is not a way to store a claim about a conversation without one:
	// it has no field to put the conversation in, so the record it writes cites nothing -
	// which is what the read back here proves.
	created, err := service.AddUserMemory(ctx, userID, authoredFor(domain.MemoryScopeGlobal, "", "我以为这条能带上会话"))
	if err != nil {
		t.Fatal(err)
	}
	read, err := store.GetMemory(ctx, userID, created.MemoryID)
	if err != nil {
		t.Fatal(err)
	}
	if read.SourceSessionID != "" || len(read.SourceMessageIDs) != 0 {
		t.Fatalf("a hand-written row cites a conversation it was never given: %+v", read)
	}
	if read.Status != domain.MemoryStatusActive || read.Origin != domain.MemoryOriginUserAuthored {
		t.Fatalf("hand-written row = %+v, want active and user-authored", read)
	}
}

// TestMemoryServiceWriteDoorsAreSeparateMethods is a compile-time statement in the same shape
// as the proposal seam's: the store exposes two writes with different signatures, and the
// only one that can carry provenance is the one that must have it. Should a future change
// merge them into a single entry point with a flag, this stops compiling and the decision
// becomes visible instead of silent.
var _ interface {
	CreateMemoryDraft(context.Context, *domain.MemoryDraft) (*domain.Memory, error)
	StoreUserAuthoredMemory(context.Context, *domain.UserAuthoredMemory) (*domain.Memory, error)
} = (repository.Repository)(nil)

var _ interface {
	AddUserMemory(context.Context, string, domain.UserAuthoredMemory) (*domain.Memory, error)
	ProposeMemoryDraft(context.Context, string, domain.MemoryDraft) (*domain.Memory, bool, error)
} = (*MemoryService)(nil)
