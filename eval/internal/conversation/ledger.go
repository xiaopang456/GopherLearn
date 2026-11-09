package conversation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"GopherAI/common/memoryrecall"
	agent "GopherAI/service/agent"
	modelcomponent "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// ModelCall deliberately retains unknown usage as nil, never an invented zero.
// UsageSamples are provider snapshots; Usage is the last reported snapshot.
// Monetary cost needs a separately approved price table and is not inferred here.
type ModelCall struct {
	ID      int    `json:"id"`
	Role    string `json:"role"`
	Purpose string `json:"purpose,omitempty"`
	// RunID and SessionID are the identity of the run the call belongs to.
	// They are empty when the call was not made for a run of this evaluation.
	RunID     string `json:"run_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	// Phase says whether the call was sent before the answer came back
	// ("answer") or after it ("after_answer"). It is derived from the run's
	// answer boundary, not from the role the prompt suggested.
	Phase                        string              `json:"phase,omitempty"`
	Method                       string              `json:"method"`
	Started                      time.Time           `json:"started"`
	Finished                     time.Time           `json:"finished"`
	InputSHA256                  string              `json:"input_sha256"`
	PromptSHA256                 string              `json:"prompt_sha256"`
	Usage                        *schema.TokenUsage  `json:"usage"`
	UsageSamples                 []schema.TokenUsage `json:"usage_samples,omitempty"`
	Model                        string              `json:"model,omitempty"`
	HTTPStatus                   int                 `json:"http_status,omitempty"`
	RawUsageSamples              []json.RawMessage   `json:"raw_usage_samples,omitempty"`
	TelemetryError               string              `json:"telemetry_error,omitempty"`
	RequestedMaxTokens           *int                `json:"requested_max_tokens"`
	RequestedMaxCompletionTokens *int                `json:"requested_max_completion_tokens"`
	Error                        string              `json:"error,omitempty"`
}
type ModelLedger struct {
	mu    sync.Mutex
	calls []ModelCall
}

func digest(b []byte) string { v := sha256.Sum256(b); return hex.EncodeToString(v[:]) }
func (l *ModelLedger) begin(role, method string, input any, prompt string) int {
	b, _ := json.Marshal(input)
	l.mu.Lock()
	defer l.mu.Unlock()
	id := len(l.calls) + 1
	l.calls = append(l.calls, ModelCall{ID: id, Role: role, Method: method, Started: time.Now().UTC(), InputSHA256: digest(b), PromptSHA256: digest([]byte(prompt))})
	return id
}
func (l *ModelLedger) usage(id int, m *schema.Message) {
	if m == nil || m.ResponseMeta == nil || m.ResponseMeta.Usage == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	u := *m.ResponseMeta.Usage
	c := &l.calls[id-1]
	c.Usage = &u
	c.UsageSamples = append(c.UsageSamples, u)
}
func (l *ModelLedger) finish(id int, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := &l.calls[id-1]
	c.Finished = time.Now().UTC()
	if err != nil {
		c.Error = err.Error()
	}
}
func (l *ModelLedger) Calls() []ModelCall {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := append([]ModelCall(nil), l.calls...)
	for i := range out {
		if out[i].RequestedMaxTokens != nil {
			v := *out[i].RequestedMaxTokens
			out[i].RequestedMaxTokens = &v
		}
		if out[i].RequestedMaxCompletionTokens != nil {
			v := *out[i].RequestedMaxCompletionTokens
			out[i].RequestedMaxCompletionTokens = &v
		}
		out[i].UsageSamples = append([]schema.TokenUsage(nil), out[i].UsageSamples...)
		out[i].RawUsageSamples = make([]json.RawMessage, len(l.calls[i].RawUsageSamples))
		for j, raw := range l.calls[i].RawUsageSamples {
			out[i].RawUsageSamples[j] = append(json.RawMessage(nil), raw...)
		}
		if out[i].Usage != nil {
			u := *out[i].Usage
			out[i].Usage = &u
		}
	}
	return out
}
func (l *ModelLedger) Complete() bool {
	for _, c := range l.Calls() {
		if c.Finished.IsZero() || c.Role == "unknown" || c.TelemetryError != "" {
			return false
		}
	}
	return true
}

// RolePrompts compares the entire first system prompt, not a user-controlled
// substring. Literal prompts are loaded from this checkout's product sources;
// any new/changed call shape fails closed until classified and reviewed.
type RolePrompts map[string]string

func LoadRolePrompts(repository string, mode memoryrecall.ReadMode) (RolePrompts, error) {
	result := RolePrompts{}
	for prompt, role := range renderedRolePrompts(agent.SystemPromptFor(mode)) {
		result[prompt] = role
	}
	specs := []struct{ path, name, role string }{
		{"agent/summarizer.go", "summarySystemPrompt", "summary"},
		{"agent/runner.go", "generalKnowledgeSystemPrompt", "answer"},
		{"agent/runner.go", "citationRepairSystemPrompt", "answer"},
		{"agent/runner.go", "memoryRepairSystemPrompt", "answer"},
		{"episode/generator.go", "episodeSystemPrompt", "background"},
		{"episode/support_verifier.go", "SupportSystemPrompt", "background"},
		{"memory/extractor.go", "extractorPrompt", "background"},
		{"memory/dream.go", "dreamPrompt", "background"},
		{"memory/global_relation.go", "globalRelationPrompt", "background"},
	}
	for _, spec := range specs {
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(repository, "v2", "service", spec.path), nil, 0)
		if err != nil {
			return nil, err
		}
		found := false
		ast.Inspect(f, func(n ast.Node) bool {
			v, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for i, name := range v.Names {
				if name.Name != spec.name || i >= len(v.Values) {
					continue
				}
				lit, ok := v.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				value, e := strconv.Unquote(lit.Value)
				if e == nil {
					result[value] = spec.role
					found = true
				}
			}
			return true
		})
		if !found {
			return nil, fmt.Errorf("role prompt is not a literal: %s", spec.name)
		}
	}
	return result, nil
}

// renderedRolePrompts returns the base prompt and every variant the product's
// own renderer can produce from it.
//
// The product renders the answer prompt per turn: a rule that names a tool
// this turn does not have is dropped, and what is left says only what the
// model can act on. So the prompt that reaches the model is rarely the base
// prompt byte for byte, and a role table that held only the base prompt
// refused the product's own request as unclassified.
//
// The variants are enumerated with the product's own renderer rather than
// with a copy of its trimming rule. The evaluator cannot hold a second
// implementation of "which paragraphs survive this turn's tool table": a copy
// would drift from the product the moment the markers change, and the drift
// would show up as a refused request in the middle of a measurement.
//
// The enumeration is over the capability sets the product can build. Every
// capability the product knows is named here, so a capability the product
// adds later is missing from this list until it is added here too - and the
// miss is loud, because the turn that needs the new capability produces a
// prompt that is not in the table and is refused.
func renderedRolePrompts(base string) map[string]string {
	capabilities := []agent.MemoryToolCapability{
		agent.CapabilityKnowledgeSearch,
		agent.CapabilityDocumentRead,
		agent.CapabilityHistoryRead,
		agent.CapabilityEpisodeSearch,
		agent.CapabilityTopicMemoryRead,
		agent.CapabilityMemoryDraft,
	}
	result := map[string]string{}
	for mask := 0; mask < 1<<len(capabilities); mask++ {
		available := map[agent.MemoryToolCapability]bool{}
		for i, capability := range capabilities {
			if mask&(1<<i) != 0 {
				available[capability] = true
			}
		}
		// The draft rule is the one place where the prompt depends on the
		// question rather than on the tool table, so both answers are
		// enumerated for every tool table.
		for _, intent := range []bool{false, true} {
			rendered := agent.RenderSystemPrompt(base, agent.PromptCapabilities{Available: available, MemoryIntent: intent})
			result[rendered] = "answer"
		}
	}
	return result
}

type ledgerModel struct {
	inner  modelcomponent.ToolCallingChatModel
	ledger *ModelLedger
	roles  RolePrompts
}

func WrapModel(inner modelcomponent.ToolCallingChatModel, ledger *ModelLedger, roles RolePrompts) (modelcomponent.ToolCallingChatModel, error) {
	if inner == nil || ledger == nil || len(roles) == 0 {
		return nil, fmt.Errorf("incomplete model telemetry")
	}
	copyRoles := RolePrompts{}
	for k, v := range roles {
		if v != "answer" && v != "summary" && v != "background" {
			return nil, fmt.Errorf("invalid model role")
		}
		copyRoles[k] = v
	}
	return &ledgerModel{inner, ledger, copyRoles}, nil
}
func (m *ledgerModel) WithTools(tools []*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	bound, err := m.inner.WithTools(tools)
	if err != nil {
		return nil, err
	}
	return &ledgerModel{bound, m.ledger, m.roles}, nil
}
func (m *ledgerModel) start(method string, input []*schema.Message) (int, error) {
	role := "unknown"
	prompt := ""
	if len(input) > 0 && input[0] != nil && input[0].Role == schema.System {
		prompt = input[0].Content
		if r, ok := m.roles[prompt]; ok {
			role = r
		}
	}
	id := m.ledger.begin(role, method, input, prompt)
	if role == "unknown" {
		err := fmt.Errorf("unclassified model call refused")
		m.ledger.finish(id, err)
		return id, err
	}
	return id, nil
}
func (m *ledgerModel) Generate(ctx context.Context, input []*schema.Message, opts ...modelcomponent.Option) (*schema.Message, error) {
	id, err := m.start("generate", input)
	if err != nil {
		return nil, err
	}
	out, err := m.inner.Generate(ctx, input, opts...)
	m.ledger.usage(id, out)
	m.ledger.finish(id, err)
	return out, err
}
func (m *ledgerModel) Stream(ctx context.Context, input []*schema.Message, opts ...modelcomponent.Option) (*schema.StreamReader[*schema.Message], error) {
	id, err := m.start("stream", input)
	if err != nil {
		return nil, err
	}
	upstream, err := m.inner.Stream(ctx, input, opts...)
	if err != nil {
		m.ledger.finish(id, err)
		return nil, err
	}
	if upstream == nil {
		err = fmt.Errorf("nil model stream")
		m.ledger.finish(id, err)
		return nil, err
	}
	reader, writer := schema.Pipe[*schema.Message](0)
	go func() {
		var terminal error
		defer func() { upstream.Close(); m.ledger.finish(id, terminal); writer.Close() }()
		for {
			msg, e := upstream.Recv()
			if e == io.EOF {
				return
			}
			if e != nil {
				terminal = e
				writer.Send(nil, e)
				return
			}
			m.ledger.usage(id, msg)
			if writer.Send(msg, nil) {
				terminal = fmt.Errorf("model stream closed before EOF")
				return
			}
		}
	}()
	return reader, nil
}
