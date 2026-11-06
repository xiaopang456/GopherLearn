package conversation

import (
	"GopherAI/common/memoryrecall"
	agent "GopherAI/service/agent"
	"context"
	"errors"
	modelcomponent "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"io"
	"path/filepath"
	"testing"
)

type ledgerStub struct {
	calls int
	fail  bool
}

func (s *ledgerStub) WithTools([]*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	return s, nil
}
func (s *ledgerStub) Generate(context.Context, []*schema.Message, ...modelcomponent.Option) (*schema.Message, error) {
	s.calls++
	if s.fail {
		return nil, errors.New("offline failure")
	}
	return &schema.Message{Role: schema.Assistant, Content: "synthetic", ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{PromptTokens: 12, CompletionTokens: 3, TotalTokens: 15, PromptTokenDetails: schema.PromptTokenDetails{CachedTokens: 4}}}}, nil
}
func (s *ledgerStub) Stream(ctx context.Context, in []*schema.Message, opts ...modelcomponent.Option) (*schema.StreamReader[*schema.Message], error) {
	out, err := s.Generate(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{out}), nil
}
func TestLedgerPreservesBoundModelAndStreamingUsage(t *testing.T) {
	l := &ModelLedger{}
	stub := &ledgerStub{}
	m, err := WrapModel(stub, l, RolePrompts{"answer prompt": "answer", "summary prompt": "summary", "background prompt": "background"})
	if err != nil {
		t.Fatal(err)
	}
	m, err = m.WithTools([]*schema.ToolInfo{{Name: "synthetic"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"answer prompt", "summary prompt", "background prompt"} {
		r, e := m.Stream(context.Background(), []*schema.Message{schema.SystemMessage(p), schema.UserMessage("private content")})
		if e != nil {
			t.Fatal(e)
		}
		for {
			_, e = r.Recv()
			if e == io.EOF {
				break
			}
			if e != nil {
				t.Fatal(e)
			}
		}
		r.Close()
	}
	if !l.Complete() || len(l.Calls()) != 3 {
		t.Fatal("incomplete stream ledger")
	}
	for _, c := range l.Calls() {
		if c.Usage == nil || c.Usage.PromptTokenDetails.CachedTokens != 4 || len(c.UsageSamples) != 1 || c.InputSHA256 == "" {
			t.Fatalf("bad usage: %+v", c)
		}
	}
	copy := l.Calls()
	copy[0].Usage.PromptTokens = 999
	if l.Calls()[0].Usage.PromptTokens != 12 {
		t.Fatal("mutable ledger snapshot")
	}
	if _, err = m.Generate(context.Background(), []*schema.Message{schema.SystemMessage("unknown"), schema.UserMessage("answer prompt")}); err == nil {
		t.Fatal("unknown call allowed")
	}
	if stub.calls != 3 || l.Complete() {
		t.Fatal("unknown telemetry accepted")
	}
}
func TestRolePromptsLoadCheckout(t *testing.T) {
	roles, err := LoadRolePrompts(filepath.Join("..", "..", ".."), memoryrecall.ReadMode("legacy_v2"))
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, role := range roles {
		counts[role]++
	}
	// The answer role holds one entry per prompt the product can send, and
	// that count grows with the modes and the capability sets, so it is a
	// lower bound. The two fixed roles are the ones with one prompt each
	// besides the summary.
	//
	// Three prompts carry the summary role: the rolling summary, the layered
	// summary's own segment prompt, and the changes-only prompt that same
	// batch is asked for when the board no longer fits. A call shape that is
	// not in this list is refused, so these counts are what say a new shape
	// was classified and reviewed.
	if counts["background"] != 5 {
		t.Fatal(counts)
	}
	if counts["summary"] < 3 {
		t.Fatalf("the summary role holds %d prompts, want at least 3: %v", counts["summary"], counts)
	}
	if counts["answer"] < 4 {
		t.Fatalf("the answer role holds %d prompts, want at least 4: %v", counts["answer"], counts)
	}
}

// The trimming must not turn the role table into an open door. A prompt that
// the product's renderer cannot produce is still refused.
func TestRolePromptsStillRefuseAPromptTheProductCannotProduce(t *testing.T) {
	roles, err := LoadRolePrompts(filepath.Join("..", "..", ".."), memoryrecall.ReadMode("legacy_v2"))
	if err != nil {
		t.Fatal(err)
	}
	base := agent.SystemPromptFor(memoryrecall.ModeLegacyV2)
	for _, prompt := range []string{
		"",
		"you are a helpful assistant",
		base + "\nan extra line the product never sends",
		agent.LayeredSummaryPrompt(),
	} {
		if role, ok := roles[prompt]; ok {
			t.Fatalf("a prompt the product cannot produce was classified as %q: %q", role, prompt)
		}
	}
}

// The prompt a layered deployment actually sends is base + the layered
// summary's contract. A table that held only the base prompt would refuse it
// as unclassified, and the turn would end before the answer existed.
func TestRolePromptsHoldTheLayeredFormOfTheAnswerPrompt(t *testing.T) {
	roles, err := LoadRolePrompts(filepath.Join("..", "..", ".."), memoryrecall.ReadMode("legacy_v2"))
	if err != nil {
		t.Fatal(err)
	}
	base := agent.SystemPromptFor(memoryrecall.ModeLegacyV2)
	layered := base + agent.LayeredSummaryPrompt()
	if layered == base {
		t.Fatal("the fixture did not append the layered contract, so it proves nothing")
	}
	if role, ok := roles[layered]; !ok || role != "answer" {
		t.Fatalf("the layered answer prompt is not classified as an answer prompt: %q", role)
	}
	// The same prompt under a trimmed tool table is also what the model sees.
	trimmed := agent.RenderSystemPrompt(layered, agent.PromptCapabilities{
		Available: map[agent.MemoryToolCapability]bool{
			agent.CapabilityKnowledgeSearch:       true,
			agent.CapabilityDocumentRead:          true,
			agent.CapabilityBrowseSummarySegments: true,
			agent.CapabilityHistoryRead:           true,
			agent.CapabilityTopicMemoryRead:       true,
		},
	})
	if role, ok := roles[trimmed]; !ok || role != "answer" {
		t.Fatalf("the trimmed layered prompt is not classified as an answer prompt: %q", role)
	}
}

// The product trims the answer prompt to the turn's tool table, so the prompt
// that reaches the model is a variant of the base prompt. A role table that
// held only the base prompt refused the product's own request as unclassified,
// which ended the turn before the answer existed.
//
// The test uses the product's own renderer to build the variant, so it states
// the same claim the product makes: the trimmed prompt is the same prompt.
func TestRolePromptsHoldTheTrimmedVariantsOfTheBasePrompt(t *testing.T) {
	roles, err := LoadRolePrompts(filepath.Join("..", "..", ".."), memoryrecall.ReadMode("legacy_v2"))
	if err != nil {
		t.Fatal(err)
	}
	base := agent.SystemPromptFor(memoryrecall.ModeLegacyV2)
	trimmed := agent.RenderSystemPrompt(base, agent.PromptCapabilities{
		Available: map[agent.MemoryToolCapability]bool{
			agent.CapabilityKnowledgeSearch: true,
			agent.CapabilityDocumentRead:    true,
		},
	})
	if trimmed == base {
		t.Fatal("the fixture did not trim the prompt, so it proves nothing")
	}
	if role, ok := roles[trimmed]; !ok || role != "answer" {
		t.Fatalf("the trimmed prompt is not classified as an answer prompt: %q", role)
	}
	if role, ok := roles[base]; !ok || role != "answer" {
		t.Fatalf("the base prompt is not classified as an answer prompt: %q", role)
	}
}
