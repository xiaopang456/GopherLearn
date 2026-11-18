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
	"strings"
	"sync"
	"time"

	"GopherAI/common/memoryrecall"
	agent "GopherAI/service/agent"
	modelcomponent "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// CardExchange is one card generation's raw request and response.
//
// The bodies are the model's own words and the card it wrote. They carry no
// credential: the handler that reads them never reads a header, and the key
// the harness uses is a placeholder.
type CardExchange struct {
	// SessionID is the conversation the card was written for, when the
	// caller knew it.
	SessionID string `json:"session_id,omitempty"`
	// TargetMessageID is how far the card was asked to reach.
	//
	// It is the one number in the request that names a conversation: the
	// payload carries the id the card may cite to, and that id belongs to
	// exactly one conversation. So a refused card can be pointed at the
	// conversation it was written for.
	TargetMessageID uint `json:"target_message_id,omitempty"`
	// Request is the body the model was sent.
	Request string `json:"request"`
	// Response is the body the model answered.
	Response string `json:"response"`
	// HTTPStatus is the status the answer carried.
	HTTPStatus int `json:"http_status,omitempty"`
	// At is when the exchange happened.
	At time.Time `json:"at"`
}

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
	// CardSessions names the source conversations of the session cards the
	// request carried.
	//
	// The request body is the record of what the model was shown, and the
	// product does not persist the cards it showed. The names are read out of
	// that body, so an attempt can say what the answer was given without
	// asking the product to remember it.
	CardSessions []string `json:"card_sessions,omitempty"`
	// ToolTable names the tools the request offered the model.
	//
	// The two arms do not carry the same table, and a comparison that does
	// not say so would be comparing something the reader cannot see.
	ToolTable []string `json:"tool_table,omitempty"`
	// ShownMemoryKind says which cross-session memory the request carried:
	// "card", "episode" or "none".
	//
	// It is read out of the request body, because the product keeps the ids
	// of the memories it showed in the window but does not persist them, and
	// the request body is the only place left that says what the model was
	// actually shown.
	ShownMemoryKind string `json:"shown_memory_kind,omitempty"`
	// InjectedText is the cross-session memory block itself, truncated. It is
	// the difference between "the memory was there" and "the memory was there
	// and said this".
	InjectedText string `json:"injected_text,omitempty"`
	// InjectedMessageIDs are the messages the injected block cited. They are
	// the block's own citations, so they say which conversation the memory
	// was written from.
	InjectedMessageIDs []uint `json:"injected_message_ids,omitempty"`
	// InjectedSegmentSeqs are the summary segment numbers the injected block
	// cited.
	//
	// A card written from the compressed state cites the segment it read
	// rather than the messages under it, and the segment belongs to one
	// conversation - which is what says where the card came from.
	InjectedSegmentSeqs []int  `json:"injected_segment_seqs,omitempty"`
	Error               string `json:"error,omitempty"`
}
type ModelLedger struct {
	mu    sync.Mutex
	calls []ModelCall
	// cardExchanges are the card generation's raw request and response
	// bodies.
	//
	// They are kept because the card's own failure is the one failure a
	// reader cannot reconstruct: the ledger holds hashes, and a hash says
	// that two bodies differ, not what either of them said. When the server
	// refuses a card, the question is what the model actually wrote - and
	// that is in the response body.
	//
	// They live in the harness and not in the sandbox: the sandbox holds the
	// database, and the evidence outlives it. The attempt writes them to its
	// own private directory before it seals its outputs, so that a failed
	// export cannot take them away.
	cardExchanges []CardExchange
	// pendingTools is the tool table the model was bound to, read out of the
	// binding rather than out of the request. It is kept between the binding
	// and the call, because the product binds the tools once and then calls.
	pendingTools []string
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

// CardExchanges returns the card generations' raw bodies.
//
// They are copied out, because the ledger keeps writing to them while the
// preparation is still running.
func (l *ModelLedger) CardExchanges() []CardExchange {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]CardExchange, len(l.cardExchanges))
	copy(out, l.cardExchanges)
	return out
}

// CaptureCardExchange records one card generation's raw bodies.
func (l *ModelLedger) CaptureCardExchange(exchange CardExchange) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cardExchanges = append(l.cardExchanges, exchange)
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
		out[i].CardSessions = append([]string(nil), out[i].CardSessions...)
		out[i].ToolTable = append([]string(nil), out[i].ToolTable...)
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
	// The answer prompt is not one string. The product renders it per turn
	// from the turn's tool table, and a deployment that reads the layered
	// summary appends that summary's own contract to it. Both forms are
	// enumerated, because both reach the model and a role table that held
	// only one of them would refuse the product's own request as
	// unclassified - and the turn would end before the answer existed.
	for _, prompt := range renderedAnswerPrompts(agent.SystemPromptFor(mode)) {
		for variant, role := range renderedRolePrompts(prompt) {
			result[variant] = role
		}
	}
	specs := []struct{ path, name, role string }{
		{"agent/summarizer.go", "summarySystemPrompt", "summary"},
		// The layered summary compresses a stretch of turns the same way the
		// rolling summary does, so it carries the same role. The arm decides
		// which of the two a run used, and the prompt hash tells them apart
		// inside one ledger.
		{"agent/segment_generator.go", "segmentSystemPrompt", "summary"},
		// The changes-only call is the same role: it is the same batch,
		// answered in the shape that fits when the board no longer does.
		{"agent/segment_generator.go", "segmentDeltaSystemPrompt", "summary"},
		// The summary that also records what the assistant produced answers
		// under its own prompts, one for the rewrite call and one for the
		// changes-only call. They are separate literals rather than one
		// prompt with a switch in it, so the mode that does not record the
		// assistant's products sends exactly the bytes it was measured
		// against. A deployment that turns the mode on sends the other two,
		// and a table that held only the first pair refused them as
		// unclassified: the summary call was refused and the turn ended
		// before the answer existed.
		{"agent/segment_generator.go", "segmentAssistantSystemPrompt", "summary"},
		{"agent/segment_generator.go", "segmentDeltaAssistantSystemPrompt", "summary"},
		{"agent/runner.go", "generalKnowledgeSystemPrompt", "answer"},
		{"agent/runner.go", "citationRepairSystemPrompt", "answer"},
		{"agent/runner.go", "memoryRepairSystemPrompt", "answer"},
		{"episode/generator.go", "episodeSystemPrompt", "background"},
		// The session card is written by the same background worker, in the
		// same place an Episode is, so it carries the same role. A table that
		// held only the Episode prompt refused the card's request as
		// unclassified, and the card was never written: the arm then measured
		// a window without a card and called it a result.
		{"episode/card_generator.go", "cardSystemPrompt", "background"},
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

// renderedAnswerPrompts returns the base prompt and every form of it the
// product can put in front of the model.
//
// Two shapes exist. A deployment that does not read the layered summary sends
// the mode's base prompt. A deployment that does sends the base prompt with
// the layered summary's own contract appended, because the board and the
// segments are only trustworthy under that contract. Both reach the model, so
// both are classified; a table that held only the first would refuse the
// second as unclassified and end the turn before the answer existed.
func renderedAnswerPrompts(base string) []string {
	return []string{base, base + agent.LayeredSummaryPrompt()}
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
		// The layered summary's browse tool. Its marker is in the product's
		// own marker table, so a turn that carries the tool renders a prompt
		// that names it; leaving the capability out would leave that prompt
		// out of the table as well.
		agent.CapabilityBrowseSummarySegments,
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

// trustOrderChunks are the trust orders the product can write into the answer
// prompt. The product writes one per turn, naming only the sources that
// turn's window carries, and it writes it between the base prompt and the
// layered summary's own contract.
//
// The list comes from the product's own renderer, so the evaluator does not
// hold a second implementation of the rule: a copy would drift from the
// product the moment the order changes, and the drift would show up as a
// refused request in the middle of a measurement.
var trustOrderChunks = agent.TrustOrderPrompts()

// classifyPrompt reads the role of a model call from its first system prompt.
//
// The table holds the prompt byte for byte, so an unknown shape is refused
// rather than guessed at. One shape is not in the table: the product writes
// this turn's trust order into the prompt, and the order depends on what the
// window carried, so the table cannot hold every form of it.
//
// That form is recognised by taking the product's own trust order out of the
// prompt and looking up what is left. What is left is the base prompt and
// the layered contract, which the table does hold, so the lookup stays exact:
// a prompt that is neither a known rendering nor a known rendering with a
// known trust order inside it is still refused.
func classifyPrompt(roles RolePrompts, prompt string) (string, bool) {
	if role, ok := roles[prompt]; ok {
		return role, true
	}
	for _, chunk := range trustOrderChunks {
		if chunk == "" {
			continue
		}
		index := strings.Index(prompt, chunk)
		if index < 0 {
			continue
		}
		// Several trust orders are prefixes of the longer ones, so a chunk
		// that is not this turn's leaves something behind that the table
		// does not hold; the loop keeps looking rather than stopping at the
		// first match.
		stripped := prompt[:index] + prompt[index+len(chunk):]
		if role, ok := roles[stripped]; ok {
			return role, true
		}
	}
	return "", false
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
	// The tool table is what the model is offered, and it is bound before the
	// call. Recording it here says what the answer could have done, which is
	// not the same question as what it did - and the two arms differ in it.
	if names := ToolNamesFromInfos(tools); len(names) > 0 {
		m.ledger.mu.Lock()
		m.ledger.pendingTools = names
		m.ledger.mu.Unlock()
	}
	return &ledgerModel{bound, m.ledger, m.roles}, nil
}
func (m *ledgerModel) start(method string, input []*schema.Message) (int, error) {
	role := "unknown"
	prompt := ""
	if len(input) > 0 && input[0] != nil && input[0].Role == schema.System {
		prompt = input[0].Content
		if r, ok := classifyPrompt(m.roles, prompt); ok {
			role = r
		}
	}
	id := m.ledger.begin(role, method, input, prompt)
	// The prompt is the record of what the model was shown, and the cards it
	// carried are read out of it here. The names are kept with the call, so
	// an attempt does not have to hold the whole prompt to say what the
	// answer was given.
	if shown, sessions := ReadShownCards(input); shown {
		m.ledger.mu.Lock()
		m.ledger.calls[id-1].CardSessions = sessions
		m.ledger.mu.Unlock()
	}
	// The cross-session memory the prompt carried is read out of the same
	// prompt, so the in-process path says what the model was given as well.
	memory := ReadShownMemory(input)
	m.ledger.mu.Lock()
	m.ledger.calls[id-1].ShownMemoryKind = memory.Kind
	m.ledger.calls[id-1].InjectedText = memory.Text
	m.ledger.calls[id-1].InjectedMessageIDs = memory.MessageIDs
	m.ledger.calls[id-1].InjectedSegmentSeqs = memory.SegmentSeqs
	m.ledger.mu.Unlock()
	m.ledger.mu.Lock()
	if len(m.ledger.pendingTools) > 0 {
		m.ledger.calls[id-1].ToolTable = append([]string(nil), m.ledger.pendingTools...)
	}
	m.ledger.mu.Unlock()
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
