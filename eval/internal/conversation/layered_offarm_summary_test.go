package conversation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	sessiondao "GopherAI/dao/session"
	"GopherAI/model"
	agent "GopherAI/service/agent"
)

// The truncated arm's assembly, without the container.
//
// The arm's real run drives the product's rolling summary over a MySQL
// sandbox; what has to be tested is the assembly itself - that the off window
// is built on the product's summary provider and carries what it wrote - and
// that is a question about interfaces the sandbox only implements. The fakes
// below are those interfaces in memory: the store the summary is saved to, the
// conversation both sides read, and a generator whose answer is derived from
// the messages it is handed, so a summary that reaches the window can only
// have come from the replayed conversation.

// memorySummaryStore is the summary row, with the optimistic version the
// product's save path relies on.
type memorySummaryStore struct {
	mu  sync.Mutex
	row *model.SessionSummary
}

func (s *memorySummaryStore) GetSummary(context.Context, int64, string) (*model.SessionSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.row == nil {
		return nil, sessiondao.ErrSummaryNotFound
	}
	stored := *s.row
	return &stored, nil
}

func (s *memorySummaryStore) SaveSummary(_ context.Context, next *model.SessionSummary, expected uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if (s.row == nil && expected != 0) || (s.row != nil && s.row.Version != expected) {
		return sessiondao.ErrSummaryConflict
	}
	stored := *next
	stored.Version = expected + 1
	s.row = &stored
	return nil
}

// replayConversation is the stored conversation both the summarizer and the
// window read.
type replayConversation struct {
	messages []model.Message
}

func (c *replayConversation) ListAfter(_ context.Context, _ string, afterID uint, limit int) ([]model.Message, error) {
	out := make([]model.Message, 0, len(c.messages))
	for _, message := range c.messages {
		if message.ID > afterID {
			out = append(out, message)
		}
	}
	return out, nil
}

func (c *replayConversation) ListRecentAfter(ctx context.Context, sessionID string, afterID uint, limit int) ([]model.Message, error) {
	return c.ListAfter(ctx, sessionID, afterID, limit)
}

// replaySummaryGenerator derives its answer from the increment it is given.
//
// The derivation is the point: a summary in the window that names this text
// can only have come from the replayed conversation, through the provider, and
// not from anywhere else the assembly might have invented it.
type replaySummaryGenerator struct{ calls int }

func (g *replaySummaryGenerator) GenerateSummary(_ context.Context, _ string, messages []model.Message) (agent.SummaryGeneration, error) {
	g.calls++
	var firstUser model.Message
	for _, message := range messages {
		if message.IsUser {
			firstUser = message
			break
		}
	}
	document := model.SessionSummaryDocument{
		SchemaVersion: model.SessionSummarySchemaV2,
		CurrentGoal:   "回放目标：把每天的读量定下来",
		ConfirmedDecisions: model.SessionSummaryEvidenceList{{
			Text: "回放决定：" + strings.TrimSpace(firstUser.Content), EvidenceMessageIDs: []uint{firstUser.ID},
		}},
	}
	content, err := json.Marshal(document)
	if err != nil {
		return agent.SummaryGeneration{}, err
	}
	return agent.SummaryGeneration{Content: string(content)}, nil
}

// The off arm's window carries the rolling summary the replay wrote, and the
// summary's content is the conversation's own text.
//
// The second half is what makes the first half mean anything: Budget.Summary
// > 0 alone could be satisfied by any block, and a window assembled against
// the empty provider - the mistake this test exists to catch - renders no
// summary at all and fails both assertions.
func TestTheTruncatedArmSendsTheRollingSummaryTheReplayWrote(t *testing.T) {
	settings, err := offlineSettings("../../..", t.TempDir())
	if err != nil {
		t.Fatalf("load the product's own settings: %v", err)
	}
	conversation := &replayConversation{messages: make([]model.Message, 0, 40)}
	for index := 1; index <= 40; index++ {
		conversation.messages = append(conversation.messages,
			model.Message{ID: uint(2*index - 1), IsUser: true, Content: fmt.Sprintf("第 %d 轮：每天读 %d 页", index, 10+index)},
			model.Message{ID: uint(2 * index), IsUser: false, Content: fmt.Sprintf("好，记下了 %d 页", 10+index)},
		)
	}
	summaryStore := &memorySummaryStore{}
	generator := &replaySummaryGenerator{}
	summaryConfig, err := rollingSummaryConfig(settings.Config)
	if err != nil {
		t.Fatalf("map the summary config: %v", err)
	}
	// The counting wrapper has to be the generator the provider calls, or the
	// count it reports is a second counter nobody increments.
	counting := &countingSummaryGenerator{inner: generator}
	provider, err := agent.NewRollingSummaryProvider(summaryStore, conversation, counting, summaryConfig)
	if err != nil {
		t.Fatalf("build the product's provider: %v", err)
	}

	// The write phase, at the same cadence the driver replays it. The run
	// context names the question here because the read phase reuses it, and
	// the builder refuses a turn without one.
	runContext := agent.RunContext{UserID: 7, SessionID: "off-arm", Question: "我每天读多少页"}
	replay := rollingReplay{provider: provider}
	replayRollingSummaries(context.Background(), provider, runContext, len(conversation.messages), &replay, counting)
	if replay.compactions == 0 {
		t.Fatal("the replay never compacted, so the conversation never crossed the trigger")
	}

	// The read phase: the off arm's own assembly. The summary source is the
	// arm's rule, exactly as the driver passes it.
	builder, err := agent.NewContextBuilder(windowContextConfig(settings),
		conversation, nil, nil, nil, armSummarySource(LayeredArmTruncated, provider))
	if err != nil {
		t.Fatalf("build the off builder: %v", err)
	}
	window, err := builder.Build(context.Background(), runContext)
	if err != nil {
		t.Fatalf("build the off window: %v", err)
	}
	var rendered strings.Builder
	for _, message := range window.Messages {
		if message != nil {
			rendered.WriteString(message.Content)
			rendered.WriteString("\n")
		}
	}
	if window.Budget.Summary == 0 {
		t.Fatalf("the off window charged no summary budget - the arm compared against a product that does not exist:\n%s", rendered.String())
	}
	// The content: the generator's answer names the first user message of the
	// increment it was handed, and that text exists nowhere else.
	if !strings.Contains(rendered.String(), "回放决定：第 1 轮：每天读 11 页") {
		t.Fatalf("the summary in the window did not come from the replayed conversation:\n%s", rendered.String())
	}
}

// The same assembly against the empty source carries no summary - the mistake
// stated as a measurement, and the reason the assertions above are load-bearing.
func TestTheSameAssemblyAgainstTheEmptySourceCarriesNoSummary(t *testing.T) {
	settings, err := offlineSettings("../../..", t.TempDir())
	if err != nil {
		t.Fatalf("load the product's own settings: %v", err)
	}
	conversation := &replayConversation{messages: []model.Message{
		{ID: 1, IsUser: true, Content: "每天读 15 页"},
		{ID: 2, IsUser: false, Content: "记下了"},
	}}
	offBuilder, err := agent.NewContextBuilder(windowContextConfig(settings),
		conversation, nil, nil, nil, agent.EmptySummaryProvider{})
	if err != nil {
		t.Fatalf("build the builder: %v", err)
	}
	window, err := offBuilder.Build(context.Background(), agent.RunContext{UserID: 7, SessionID: "off-arm-empty", Question: "读多少页"})
	if err != nil {
		t.Fatalf("build the window: %v", err)
	}
	if window.Budget.Summary != 0 {
		t.Fatalf("the empty source was charged %d summary tokens", window.Budget.Summary)
	}
}

// The arm's rule is the product's rule: off reads the rolling summary, the
// applied mode reads neither.
func TestArmSummarySourceFollowsTheProductRule(t *testing.T) {
	settings, err := offlineSettings("../../..", t.TempDir())
	if err != nil {
		t.Fatalf("load the product's own settings: %v", err)
	}
	// One stored summary row: the arm rule decides whether the window reads
	// it, and the budget is the oracle - an unread row charges nothing under
	// either arm, a read one charges the block.
	document := model.SessionSummaryDocument{
		SchemaVersion:      model.SessionSummarySchemaV2,
		CurrentGoal:        "回放目标：把每天的读量定下来",
		ConfirmedDecisions: model.SessionSummaryEvidenceList{{Text: "回放决定：每天读 15 页", EvidenceMessageIDs: []uint{1}}},
	}
	content, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("encode the stored summary: %v", err)
	}
	summaryStore := &memorySummaryStore{}
	if err := summaryStore.SaveSummary(context.Background(), &model.SessionSummary{
		UserID: 7, SessionID: "arm", Content: string(content), CoveredMessageID: 1, TokenEstimate: 40,
	}, 0); err != nil {
		t.Fatalf("store the summary: %v", err)
	}
	conversation := &replayConversation{messages: []model.Message{
		{ID: 1, IsUser: true, Content: "每天读 15 页"},
		{ID: 2, IsUser: false, Content: "记下了"},
	}}
	summaryConfig, err := rollingSummaryConfig(settings.Config)
	if err != nil {
		t.Fatalf("map the summary config: %v", err)
	}
	rolling, err := agent.NewRollingSummaryProvider(summaryStore, conversation, &replaySummaryGenerator{}, summaryConfig)
	if err != nil {
		t.Fatalf("build the rolling provider: %v", err)
	}
	charged := func(arm string) int {
		builder, err := agent.NewContextBuilder(windowContextConfig(settings), conversation, nil, nil, nil,
			armSummarySource(arm, rolling))
		if err != nil {
			t.Fatalf("build the %s builder: %v", arm, err)
		}
		window, err := builder.Build(context.Background(), agent.RunContext{UserID: 7, SessionID: "arm", Question: "读多少页"})
		if err != nil {
			t.Fatalf("build the %s window: %v", arm, err)
		}
		return window.Budget.Summary
	}
	if budget := charged(LayeredArmTruncated); budget == 0 {
		t.Fatal("the truncated arm's window charged no summary, so the arm did not keep the rolling source")
	}
	if budget := charged(LayeredArmOn); budget != 0 {
		t.Fatalf("the layered arm's summary source cost %d tokens; the product sends no rolling summary in that mode", budget)
	}
}

// The summary configuration the driver maps is the product's own mapping, and
// the one number the arm's fairness depends on - the 1100 cap - is the same
// key the window renders with.
func TestRollingSummaryConfigMirrorsTheProductMapping(t *testing.T) {
	settings, err := offlineSettings("../../..", t.TempDir())
	if err != nil {
		t.Fatalf("load the product's own settings: %v", err)
	}
	summaryConfig, err := rollingSummaryConfig(settings.Config)
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	agentConfig := settings.Config.AgentConfig
	if summaryConfig.MaxOutputTokens != agentConfig.ContextMaxSummaryTokens {
		t.Fatalf("the generation cap is %d and the render cap is %d; the product maps one key to both",
			summaryConfig.MaxOutputTokens, agentConfig.ContextMaxSummaryTokens)
	}
	if summaryConfig.TriggerMessages != agentConfig.SummaryTriggerMessages ||
		summaryConfig.TriggerTokens != agentConfig.SummaryTriggerTokens ||
		summaryConfig.RetainMessages != agentConfig.SummaryRetainMessages ||
		summaryConfig.FetchLimit != agentConfig.SummaryFetchLimit {
		t.Fatalf("the trigger numbers are not the settings': %+v", summaryConfig)
	}
}
