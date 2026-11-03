package conversation

import (
	"GopherAI/common/memoryrecall"
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
	if counts["answer"] != 4 || counts["summary"] != 1 || counts["background"] != 5 {
		t.Fatal(counts)
	}
}
