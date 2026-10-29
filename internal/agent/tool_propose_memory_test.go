package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/responder"
)

// recordingProposer is the memory write side as the tool sees it: it records the principal
// and the proposal it was handed, and answers whatever the test needs. It cannot be told to
// confirm anything, because the seam has no such method.
type recordingProposer struct {
	outcome  MemoryProposalOutcome
	failWith error
	calls    []proposalCall
}

type proposalCall struct {
	principal Principal
	proposal  MemoryProposal
}

func (p *recordingProposer) ProposeMemory(
	_ context.Context, principal Principal, proposal MemoryProposal,
) (MemoryProposalOutcome, error) {
	call := proposalCall{principal: principal, proposal: proposal}
	if p.failWith != nil {
		p.calls = append(p.calls, call)
		return MemoryProposalOutcome{}, p.failWith
	}
	p.calls = append(p.calls, call)
	return p.outcome, nil
}

var _ MemoryProposer = (*recordingProposer)(nil)

func proposalPrincipal() Principal {
	return Principal{UserID: "user-owner", SessionID: "session-1", UserMessageID: "message-7"}
}

// proposalRow is the pending record a write that reports Created=true hands back: the
// stored row, with its owner and provenance on it. A test that claims a draft was created
// without one is describing a writer that could not say what it stored, which is a case
// worth having a name for - so it gets one below rather than being the default fixture.
func proposalRow(scope domain.MemoryScope, topicKey, content string) *domain.Memory {
	return &domain.Memory{
		MemoryID:         "mem-draft-1",
		UserID:           "user-owner",
		Scope:            scope,
		TopicKey:         topicKey,
		Content:          content,
		Status:           domain.MemoryStatusPending,
		Confidence:       1,
		SourceSessionID:  "session-1",
		SourceMessageIDs: []string{"message-7"},
	}
}

// storedProposal is a real outcome: created, and here is the row.
func storedProposal(scope domain.MemoryScope, topicKey, content string) MemoryProposalOutcome {
	return MemoryProposalOutcome{Created: true, Draft: proposalRow(scope, topicKey, content)}
}

// TestDraftCardProjectsOnlyWhatADecisionNeeds is the closed field set, at the layer that
// decides it. The row carries an owner, a session, a message list, a confidence and three
// timestamps; the card carries four fields, and every one of them is there to be looked at
// or clicked. A duplicate is the sharpest case: the writer answers with the pre-existing
// row, and a card built from it would put a decision button on this conversation for a
// draft another one is waiting on.
func TestDraftCardProjectsOnlyWhatADecisionNeeds(t *testing.T) {
	global := domain.MemoryScopeGlobal
	topic := domain.MemoryScopeTopic
	for _, testCase := range []struct {
		name    string
		outcome MemoryProposalOutcome
		want    *responder.MemoryDraft
	}{
		{
			name:    "a global row this turn stored",
			outcome: storedProposal(global, "", "用户偏好先看代码再读解释"),
			want:    &responder.MemoryDraft{DraftID: "mem-draft-1", Scope: global, Content: "用户偏好先看代码再读解释"},
		},
		{
			name:    "a topic row keeps its subject, because the label needs it",
			outcome: storedProposal(topic, "go-concurrency", "用户在学 Go 的内存模型"),
			want: &responder.MemoryDraft{
				DraftID: "mem-draft-1", Scope: topic, TopicKey: "go-concurrency", Content: "用户在学 Go 的内存模型",
			},
		},
		{
			name:    "a subject on a global row is dropped with the row's own scope",
			outcome: storedProposal(global, "go-concurrency", "用户偏好先看代码"),
			want:    &responder.MemoryDraft{DraftID: "mem-draft-1", Scope: global, Content: "用户偏好先看代码"},
		},
		{
			name:    "a duplicate that echoes the existing row",
			outcome: MemoryProposalOutcome{Created: false, Draft: proposalRow(global, "", "用户偏好先看代码")},
			want:    nil,
		},
		{
			name:    "created without a row to point at",
			outcome: MemoryProposalOutcome{Created: true},
			want:    nil,
		},
		{
			name:    "a row with no id",
			outcome: MemoryProposalOutcome{Created: true, Draft: &domain.Memory{Scope: global, Content: "一句话"}},
			want:    nil,
		},
		{
			name:    "a row with no content",
			outcome: MemoryProposalOutcome{Created: true, Draft: &domain.Memory{MemoryID: "mem-draft-1", Scope: global}},
			want:    nil,
		},
		{
			name:    "a row whose scope this server does not offer",
			outcome: MemoryProposalOutcome{Created: true, Draft: &domain.Memory{MemoryID: "mem-draft-1", Scope: "preferences", Content: "一句话"}},
			want:    nil,
		},
	} {
		got := draftCard(testCase.outcome)
		if testCase.want == nil {
			if got != nil {
				t.Fatalf("%s: produced a card from an outcome that stored nothing for this turn: %+v", testCase.name, *got)
			}
			continue
		}
		if got == nil {
			t.Fatalf("%s: no card for %#v", testCase.name, testCase.outcome)
		}
		if *got != *testCase.want {
			t.Fatalf("%s: card = %+v, want %+v", testCase.name, *got, *testCase.want)
		}
	}
}

func newProposalTool(t *testing.T, proposer MemoryProposer) *ProposeMemory {
	t.Helper()
	tool, err := NewProposeMemory(proposer, ProposeMemoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return tool
}

// TestNewProposeMemoryRequiresAWriter pins the construction-time refusal: a tool that can
// only answer "could not store" would be an offer the server cannot keep.
func TestNewProposeMemoryRequiresAWriter(t *testing.T) {
	if _, err := NewProposeMemory(nil, ProposeMemoryOptions{}); !errors.Is(err, ErrNoMemoryProposer) {
		t.Fatalf("nil writer = %v, want the construction-time refusal", err)
	}
}

// TestProposeMemorySchemaDeclaresThreeKeysAndNothingElse: the advertised shape is the
// attack surface, so what it omits is the security property.
func TestProposeMemorySchemaDeclaresThreeKeysAndNothingElse(t *testing.T) {
	tool := newProposalTool(t, &recordingProposer{})
	var schema map[string]any
	if err := json.Unmarshal(tool.Parameters(), &schema); err != nil {
		t.Fatalf("Parameters() is not valid JSON: %v", err)
	}
	if schema["type"] != "object" {
		t.Fatalf("schema type = %v, want object", schema["type"])
	}
	if schema["additionalProperties"] != false {
		t.Fatal("the schema must refuse properties it never declared")
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("schema has no properties object")
	}
	if len(properties) != 3 {
		t.Fatalf("declared properties = %v, want exactly scope, topic_key and content", keysOf(properties))
	}
	for _, name := range []string{"scope", "topic_key", "content"} {
		if _, exists := properties[name]; !exists {
			t.Fatalf("property %q is missing from %v", name, keysOf(properties))
		}
	}
	required, _ := schema["required"].([]any)
	if len(required) != 2 {
		t.Fatalf("required = %v, want scope and content only", required)
	}
	// Scope is a closed set in the schema as well as in the parser, so a provider cannot
	// offer a fourth layer this server would not store.
	scope, _ := properties["scope"].(map[string]any)
	enum, _ := scope["enum"].([]any)
	if len(enum) != 3 {
		t.Fatalf("scope enum = %v, want the three scopes this server knows", enum)
	}
	for _, want := range []string{"global", "topic", "episodic"} {
		found := false
		for _, value := range enum {
			if value == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("scope enum %v is missing %q", enum, want)
		}
	}
	// A schema for a write must not name the fields that would decide the write.
	for _, forbidden := range []string{
		"user_id", "session_id", "source_session_id", "source_message_ids", "memory_id",
		"status", "confidence", "created_at", "updated_at", "superseded_at",
	} {
		if _, exists := properties[forbidden]; exists {
			t.Fatalf("the schema advertises %q, which is a server-owned decision", forbidden)
		}
	}
	if strings.Contains(string(tool.Parameters()), "confirm") {
		t.Fatal("the schema mentions confirming, which this tool cannot do")
	}
}

func keysOf(values map[string]any) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	return names
}

// TestForgedProposalFieldsAreRefusedWithoutWriting covers the identity rule from both ends:
// every field a model might try to steer is refused, and the writer is never reached, so a
// refusal cannot half-apply.
func TestForgedProposalFieldsAreRefusedWithoutWriting(t *testing.T) {
	forgeries := []string{
		`{"scope":"global","content":"好","user_id":"user-victim"}`,
		`{"scope":"global","content":"好","session_id":"session-victim"}`,
		`{"scope":"global","content":"好","source_session_id":"session-victim"}`,
		`{"scope":"global","content":"好","source_message_ids":["message-1"]}`,
		`{"scope":"global","content":"好","memory_id":"mem-1"}`,
		`{"scope":"global","content":"好","status":"active"}`,
		`{"scope":"global","content":"好","confidence":0.2}`,
		`{"scope":"global","content":"好","created_at":"2026-01-01T00:00:00Z"}`,
		`{"scope":"global","content":"好","updated_at":"2026-01-01T00:00:00Z"}`,
		`{"scope":"global","content":"好","superseded_at":"2026-01-01T00:00:00Z"}`,
		`{"scope":"global","content":"好","active":true}`,
		`{"scope":"global","content":"好","all_users":true}`,
		`{"scope":"global","content":"好","confirm":true}`,
		`{"scope":"global","content":"好","skip_confirmation":true}`,
	}
	for _, arguments := range forgeries {
		proposer := &recordingProposer{}
		tool := newProposalTool(t, proposer)
		_, err := tool.Execute(context.Background(), proposalPrincipal(), json.RawMessage(arguments))
		if !errors.Is(err, ErrInvalidArguments) || !errors.Is(err, ErrUnknownArgumentField) {
			t.Fatalf("arguments %s = %v, want an undeclared-field refusal", arguments, err)
		}
		if len(proposer.calls) != 0 {
			t.Fatalf("arguments %s reached the writer %d time(s)", arguments, len(proposer.calls))
		}
	}
}

// TestProposalArgumentsMustBeAnObject pins the envelope rules, including the doubled JSON
// a provider sometimes hands over as a string.
func TestProposalArgumentsMustBeAnObject(t *testing.T) {
	tool := newProposalTool(t, &recordingProposer{})
	for _, arguments := range []string{``, `[]`, `"scope":"global"`, `{"scope":"global"}{"scope":"topic"}`} {
		_, err := tool.Execute(context.Background(), proposalPrincipal(), json.RawMessage(arguments))
		if !errors.Is(err, ErrInvalidArguments) {
			t.Fatalf("arguments %q = %v, want an invalid-arguments refusal", arguments, err)
		}
	}
}

// TestProposalIdentityComesFromThePrincipalOnly: the same arguments under two different
// turns must produce two different drafts. If any part of the source could be steered from
// the payload, this is the test that would fail.
func TestProposalIdentityComesFromThePrincipalOnly(t *testing.T) {
	proposer := &recordingProposer{}
	tool := newProposalTool(t, proposer)
	arguments := json.RawMessage(`{"scope":"topic","topic_key":"redis","content":"RDB 与 AOF 的取舍点在于恢复速度"}`)

	for _, principal := range []Principal{
		proposalPrincipal(),
		{UserID: "user-other", SessionID: "session-9", UserMessageID: "message-42"},
	} {
		if _, err := tool.Execute(context.Background(), principal, arguments); err != nil {
			t.Fatal(err)
		}
	}
	if len(proposer.calls) != 2 {
		t.Fatalf("writer saw %d calls, want 2", len(proposer.calls))
	}
	for index, call := range proposer.calls {
		want := []Principal{proposalPrincipal(), {UserID: "user-other", SessionID: "session-9", UserMessageID: "message-42"}}[index]
		if call.principal != want {
			t.Fatalf("call %d ran as %+v, want %+v", index, call.principal, want)
		}
		if call.proposal.Scope != domain.MemoryScopeTopic || call.proposal.TopicKey != "redis" {
			t.Fatalf("call %d proposal = %+v, want the topic the model named", index, call.proposal)
		}
	}
}

// TestProposalNeedsAStoredQuestionToCite is the "no source, no draft" rule: every missing
// half of the provenance is refused rather than filled in with a placeholder.
func TestProposalNeedsAStoredQuestionToCite(t *testing.T) {
	arguments := json.RawMessage(`{"scope":"global","content":"用户偏好先看代码"}`)
	for name, principal := range map[string]Principal{
		"no user":            {SessionID: "session-1", UserMessageID: "message-7"},
		"no session":         {UserID: "user-owner", UserMessageID: "message-7"},
		"no stored question": {UserID: "user-owner", SessionID: "session-1"},
		"blank question":     {UserID: "user-owner", SessionID: "session-1", UserMessageID: "   "},
	} {
		proposer := &recordingProposer{}
		tool := newProposalTool(t, proposer)
		_, err := tool.Execute(context.Background(), principal, arguments)
		if name == "no user" {
			if !errors.Is(err, ErrNoPrincipal) {
				t.Fatalf("%s: %v, want the missing-principal refusal", name, err)
			}
		} else if !errors.Is(err, ErrProposalProvenanceMissing) {
			t.Fatalf("%s: %v, want the missing-provenance refusal", name, err)
		}
		if len(proposer.calls) != 0 {
			t.Fatalf("%s: the writer was still called %d time(s)", name, len(proposer.calls))
		}
	}
}

// TestProposalShapeRulesMirrorTheDomain: an illegal proposal is refused as a bad argument,
// not discovered later as a write failure.
func TestProposalShapeRulesMirrorTheDomain(t *testing.T) {
	cases := []struct {
		name      string
		arguments string
		want      error
	}{
		{"unknown scope", `{"scope":"mood","content":"好"}`, ErrProposalScopeInvalid},
		{"scope cased differently", `{"scope":"GLOBAL","content":"好"}`, ErrProposalScopeInvalid},
		{"scope missing", `{"content":"好"}`, ErrProposalScopeInvalid},
		{"global with a topic key", `{"scope":"global","topic_key":"redis","content":"好"}`, ErrProposalTopicKeyUse},
		{"topic without a key", `{"scope":"topic","content":"好"}`, ErrProposalTopicKeyUse},
		{"topic key too long", `{"scope":"topic","topic_key":"` + strings.Repeat("k", 97) + `","content":"好"}`, ErrProposalTopicKeyUse},
		{"topic key with a newline", `{"scope":"topic","topic_key":"red\nis","content":"好"}`, ErrProposalTopicKeyUse},
		{"blank content", `{"scope":"global","content":"   "}`, ErrProposalContentInvalid},
		{"content missing", `{"scope":"global"}`, ErrProposalContentInvalid},
		{"content too long", `{"scope":"global","content":"` + strings.Repeat("记", 401) + `"}`, ErrProposalContentInvalid},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			proposer := &recordingProposer{}
			scoped := newProposalTool(t, proposer)
			_, err := scoped.Execute(context.Background(), proposalPrincipal(), json.RawMessage(testCase.arguments))
			if !errors.Is(err, ErrInvalidArguments) || !errors.Is(err, testCase.want) {
				t.Fatalf("%s = %v, want a refusal naming %v", testCase.name, err, testCase.want)
			}
			if len(proposer.calls) != 0 {
				t.Fatalf("%s reached the writer", testCase.name)
			}
		})
	}
	// And the accepted shapes, so the refusals above are not just "everything fails".
	for _, arguments := range []string{
		`{"scope":"global","content":"用户偏好先看代码"}`,
		`{"scope":"topic","topic_key":"redis","content":"RDB 与 AOF 的取舍点在于恢复速度"}`,
		`{"scope":"episodic","content":"这次讲清了 fsync 的三档"}`,
		`{"scope":"episodic","topic_key":"redis","content":"这次讲清了 fsync 的三档"}`,
	} {
		proposer := &recordingProposer{}
		accepted := newProposalTool(t, proposer)
		if _, err := accepted.Execute(context.Background(), proposalPrincipal(), json.RawMessage(arguments)); err != nil {
			t.Fatalf("%s was refused: %v", arguments, err)
		}
		if len(proposer.calls) != 1 {
			t.Fatalf("%s stored %d drafts, want 1", arguments, len(proposer.calls))
		}
	}
}

// TestProposalObservationCarriesNoIdentifiers: what the model reads back is a status
// sentence. A memory id it never sees is one it cannot cite, reuse, or ask for by name -
// including the id of a row the write really stored, which travels to the progress frame
// and not into the model's transcript.
func TestProposalObservationCarriesNoIdentifiers(t *testing.T) {
	for name, outcome := range map[string]MemoryProposalOutcome{
		"stored":     storedProposal(domain.MemoryScopeGlobal, "", "用户偏好先看代码"),
		"duplicate":  {Created: false, Draft: proposalRow(domain.MemoryScopeGlobal, "", "用户偏好先看代码")},
		"normalized": storedProposal(domain.MemoryScopeGlobal, "", "用户偏好先看代码，再读解释"),
		"not stored": {Created: false},
	} {
		proposer := &recordingProposer{outcome: outcome}
		tool := newProposalTool(t, proposer)
		observation, err := tool.Execute(context.Background(), proposalPrincipal(),
			json.RawMessage(`{"scope":"global","content":"用户偏好先看代码"}`))
		if err != nil {
			t.Fatal(err)
		}
		if observation.Found {
			t.Fatalf("%s: a proposal is not evidence the answer can be grounded in", name)
		}
		if len(observation.Sources) != 0 {
			t.Fatalf("%s: a draft that was not recalled must earn no citation: %+v", name, observation.Sources)
		}
		for _, forbidden := range []string{"mem-", "message-7", "session-1", "user-owner", "src-"} {
			if strings.Contains(observation.Text, forbidden) {
				t.Fatalf("%s: the observation leaks %q: %s", name, forbidden, observation.Text)
			}
		}
		if !strings.Contains(observation.Text, "确认") {
			t.Fatalf("%s: the observation must say confirmation is somebody else's: %s", name, observation.Text)
		}
		if strings.Contains(observation.Text, "已生效") || strings.Contains(observation.Text, "已记住") {
			t.Fatalf("%s: the observation claims a stored draft took effect: %s", name, observation.Text)
		}
	}
}

// TestProposalWriteFailureStaysAFailure: "could not record" must not come back as an
// observation that reads like success. The cause stays in the Go error chain - that is how
// a caller classifies it, and an internal error is not conversation content - while nothing
// at all reaches the model, because a failed write produces no observation.
func TestProposalWriteFailureStaysAFailure(t *testing.T) {
	offline := errors.New("store is offline")
	proposer := &recordingProposer{failWith: offline}
	tool := newProposalTool(t, proposer)
	observation, err := tool.Execute(context.Background(), proposalPrincipal(),
		json.RawMessage(`{"scope":"global","content":"用户偏好先看代码"}`))
	if !errors.Is(err, ErrProposalWrite) || !errors.Is(err, offline) {
		t.Fatalf("write failure = %v, want the write category keeping its cause", err)
	}
	if observation.Text != "" || observation.Found || len(observation.Sources) != 0 {
		t.Fatalf("a failed write produced an observation: %+v", observation)
	}
}

// TestProposalOptionsCeilings: a configured bound outside its sane range falls back to the
// default rather than being clamped upward, which is the rule every other tool option in
// this package follows - a typo must not buy a looser limit than nobody reviewed.
func TestProposalOptionsCeilings(t *testing.T) {
	if got := (ProposeMemoryOptions{ContentRunes: 1_000_000}).Normalize(); got.ContentRunes != 400 {
		t.Fatalf("an out-of-range content bound = %d, want the default back", got.ContentRunes)
	}
	if got := (ProposeMemoryOptions{ContentRunes: -5}).Normalize(); got.ContentRunes != 400 {
		t.Fatalf("unset content bound = %d, want the default", got.ContentRunes)
	}
	if got := (ProposeMemoryOptions{TopicKeyRunes: 9_000}).Normalize(); got.TopicKeyRunes != domain.MaxMemoryTopicKeyRunes {
		t.Fatalf("topic key bound = %d, want the default back", got.TopicKeyRunes)
	}
	// A configured bound inside the range is honoured, and the schema says so too.
	tool, err := NewProposeMemory(&recordingProposer{}, ProposeMemoryOptions{ContentRunes: 20})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(tool.Parameters()), `"maxLength":20`) {
		t.Fatalf("the advertised schema still says the default: %s", tool.Parameters())
	}
	if _, err := tool.Execute(context.Background(), proposalPrincipal(),
		json.RawMessage(`{"scope":"global","content":"`+strings.Repeat("记", 21)+`"}`)); !errors.Is(err, ErrProposalContentInvalid) {
		t.Fatalf("a 21-rune content past the configured 20 = %v, want a refusal", err)
	}
}

// The harness-level rules: one write per turn, counted by the loop rather than by the tool,
// and unaffected by however the tool was decorated.

// The write ceiling is a stop, not an observation: a model that has spent its
// one proposal and asks again ends the turn. That is the direction the
// reference project measured its budgets under; answering a spent capability
// out of observations would spend the user's remaining model calls on a
// foregone conclusion.
func TestProposeMemoryRunsOncePerTurn(t *testing.T) {
	chat := &scriptedToolModel{replies: []model.Completion{
		toolCallReply("call-1", ProposeMemoryName, `{"scope":"global","content":"用户偏好先看代码"}`),
		// Different arguments, so the consecutive fingerprint does not fire
		// first and the per-capability ceiling is what stops the turn.
		toolCallReply("call-2", ProposeMemoryName, `{"scope":"global","content":"另一条不同的提议"}`),
		answerReply("不该走到这里"),
	}}
	proposer := &recordingProposer{}
	harness := newHarness(t, chat, Budget{MaxModelCalls: 4, MaxToolCalls: 3, MaxSteps: 10, Deadline: time.Second},
		newProposalTool(t, proposer))

	request := baseRequest()
	request.UserMessageID = "message-7"
	_, spent, err := harness.Run(context.Background(), request)
	if !errors.Is(err, ErrToolCallLimit) {
		t.Fatalf("error = %v, want ErrToolCallLimit", err)
	}
	if len(proposer.calls) != 1 {
		t.Fatalf("the writer ran %d times, want once per turn", len(proposer.calls))
	}
	if spent.ToolCalls != 1 || spent.ModelCalls != 2 {
		t.Fatalf("usage = %+v, want one charged write and two model calls", spent)
	}
}

// countingWrapper is what DecorateTool hands back: the same behaviour, no extra methods.
// The per-turn limit must survive the wrapping, which is why it is keyed by tool name.
type countingWrapper struct {
	inner Tool
	seen  *int
}

func (w countingWrapper) Name() string        { return w.inner.Name() }
func (w countingWrapper) Description() string { return w.inner.Description() }
func (w countingWrapper) Parameters() json.RawMessage {
	return w.inner.Parameters()
}
func (w countingWrapper) Execute(ctx context.Context, principal Principal, arguments json.RawMessage) (Observation, error) {
	*w.seen++
	return w.inner.Execute(ctx, principal, arguments)
}

var _ Tool = countingWrapper{}

func TestDecoratedProposalKeepsItsPerTurnLimit(t *testing.T) {
	chat := &scriptedToolModel{repeatLast: true, varyRepeat: true, replies: []model.Completion{
		toolCallReply("call-1", ProposeMemoryName, `{"scope":"global","content":"用户偏好先看代码"}`),
	}}
	proposer := &recordingProposer{}
	wrapped := 0
	harness := newHarness(t, chat, Budget{MaxModelCalls: 2, MaxToolCalls: 3, MaxSteps: 4, Deadline: time.Second},
		countingWrapper{inner: newProposalTool(t, proposer), seen: &wrapped})

	request := baseRequest()
	request.UserMessageID = "message-7"
	// The script varies its asks, so the consecutive fingerprint does not fire
	// and the per-capability ceiling is what ends the turn.
	_, spent, err := harness.Run(context.Background(), request)
	if !errors.Is(err, ErrToolCallLimit) {
		t.Fatalf("error = %v, want ErrToolCallLimit", err)
	}
	if len(proposer.calls) != 1 {
		t.Fatalf("a wrapped tool ran %d times, want once: the limit was lost in decoration", len(proposer.calls))
	}
	if spent.ToolCalls != 1 {
		t.Fatalf("%d tool calls were charged, want the refused repeat to cost nothing", spent.ToolCalls)
	}
}

func TestReadToolsHaveNoPerTurnLimit(t *testing.T) {
	search := &recordingTool{name: MemorySearchName, observations: []Observation{{Text: "读到一条", Found: true}}}
	// The second ask differs in arguments: an exact consecutive repeat is
	// refused before it runs, while a read tool with no per-turn ceiling may
	// run again.
	chat := &scriptedToolModel{replies: []model.Completion{
		toolCallReply("call-1", MemorySearchName, `{"query":"fsync"}`),
		toolCallReply("call-2", MemorySearchName, `{"query":"fsync latency"}`),
		answerReply("根据记忆回答。"),
	}}
	harness := newHarness(t, chat, Budget{}, search)

	if _, _, err := harness.Run(context.Background(), baseRequest()); err != nil {
		t.Fatal(err)
	}
	if len(search.calls) != 2 {
		t.Fatalf("a read tool ran %d times, want it free to run twice", len(search.calls))
	}
}

func TestFailedProposalEndsTheTurnAndStoresNothing(t *testing.T) {
	chat := &scriptedToolModel{replies: []model.Completion{
		toolCallReply("call-1", ProposeMemoryName, `{"scope":"global","content":"用户偏好先看代码"}`),
		answerReply("不该走到这里"),
	}}
	proposer := &recordingProposer{failWith: errors.New("store is offline")}
	harness := newHarness(t, chat, Budget{}, newProposalTool(t, proposer))

	request := baseRequest()
	request.UserMessageID = "message-7"
	_, _, err := harness.Run(context.Background(), request)
	if !errors.Is(err, ErrToolExecution) || !errors.Is(err, ErrProposalWrite) {
		t.Fatalf("run error = %v, want a tool execution failure naming the write", err)
	}
	if len(chat.requests) != 1 {
		t.Fatalf("the model was called %d times after a failed write, want the turn to stop", len(chat.requests))
	}
}

// The principal the loop hands over is the one assembled from the turn, never from text.
func TestProposalPrincipalIsAssembledFromTheTurn(t *testing.T) {
	chat := &scriptedToolModel{replies: []model.Completion{
		toolCallReply("call-1", ProposeMemoryName, `{"scope":"global","content":"用户偏好先看代码"}`),
		answerReply("好"),
	}}
	proposer := &recordingProposer{}
	harness := newHarness(t, chat, Budget{}, newProposalTool(t, proposer))

	request := baseRequest()
	request.UserID = "user-actual"
	request.SessionID = "session-actual"
	request.UserMessageID = "message-actual"
	if _, _, err := harness.Run(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if len(proposer.calls) != 1 {
		t.Fatalf("writer saw %d calls", len(proposer.calls))
	}
	got := proposer.calls[0].principal
	if got != (Principal{UserID: "user-actual", SessionID: "session-actual", UserMessageID: "message-actual"}) {
		t.Fatalf("principal = %+v, want exactly what the turn carried", got)
	}
}
