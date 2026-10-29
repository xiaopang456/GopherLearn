package contextwindow

import (
	"errors"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/responder"
)

func validConfig(t *testing.T) Config {
	t.Helper()
	// The production default follows v2 and leaves clean-only history caps unset.
	// These focused window tests explicitly exercise the legacy compatibility
	// envelope so their clipping assertions remain deterministic.
	config := DefaultConfig()
	config.HistoryMaxTokens = 6000
	config.HistoryMaxMessages = 12
	config.HistoryMaxRunes = 6000
	config, err := config.Resolve()
	if err != nil {
		t.Fatalf("the shipped defaults do not validate: %v", err)
	}
	return config
}

func TestResolveFillsUnsetFieldsOnly(t *testing.T) {
	resolved, err := Config{}.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if resolved != DefaultConfig() {
		t.Fatalf("an unset configuration resolved to something other than the defaults: %+v", resolved)
	}
	// Zero means "not configured" and never "send nothing": a deployment that forgets
	// one field must not end up with a zero-length window.
	if resolved.InputTargetTokens == 0 || resolved.EvidenceMaxTokens == 0 {
		t.Fatal("a resolved budget holds a zero limit")
	}
}

// Each illegal figure is refused by name rather than clamped, so the startup log can
// say which one an operator got wrong. A budget that silently fixed itself would be
// reported as the operator's own choice.
func TestValidateRefusesEachIllegalBudgetByName(t *testing.T) {
	cases := []struct {
		name  string
		patch func(*Config)
		want  error
	}{
		{"window below the floor", func(c *Config) { c.WindowTokens = 4096 }, ErrWindowOutOfRange},
		{"window above the ceiling", func(c *Config) { c.WindowTokens = 2_000_000 }, ErrWindowOutOfRange},
		{"negative window", func(c *Config) { c.WindowTokens = -1 }, ErrWindowOutOfRange},
		{"input target below the floor", func(c *Config) { c.InputTargetTokens = 1024 }, ErrInputTargetOutOfRange},
		{"input target above the window", func(c *Config) {
			c.WindowTokens = 32768
			c.InputTargetTokens = 40000
			c.OutputReserveTokens = 256
		}, ErrTargetExceedsWindow},
		{"target plus reserve overflows the window", func(c *Config) {
			c.WindowTokens = 24576
			c.InputTargetTokens = 20480
			c.OutputReserveTokens = 4097
		}, ErrTargetExceedsWindow},
		{"reserve below the floor", func(c *Config) { c.OutputReserveTokens = 64 }, ErrOutputReserveOutOfRange},
		{"summary cap below the floor", func(c *Config) { c.SummaryMaxTokens = 16 }, ErrSummaryCapOutOfRange},
		{"summary cap above the ceiling", func(c *Config) { c.SummaryMaxTokens = 9000 }, ErrSummaryCapOutOfRange},
		{"history cap below the floor", func(c *Config) { c.HistoryMaxTokens = 8 }, ErrHistoryCapOutOfRange},
		{"history cap above the ceiling", func(c *Config) { c.HistoryMaxTokens = 40000 }, ErrHistoryCapOutOfRange},
		{"fetch limit cannot hold a pair", func(c *Config) { c.HistoryFetchLimit = 1 }, ErrHistoryFetchLimitOutOfRange},
		{"fetch limit above the ceiling", func(c *Config) { c.HistoryFetchLimit = 513 }, ErrHistoryFetchLimitOutOfRange},
		{"evidence cap below the floor", func(c *Config) { c.EvidenceMaxTokens = 8 }, ErrEvidenceCapOutOfRange},
		{"evidence cap swallows the target", func(c *Config) {
			c.InputTargetTokens = 8192
			c.OutputReserveTokens = 256
			c.SummaryMaxTokens = 128
			c.HistoryMaxTokens = 256
			c.EvidenceMaxTokens = 8192
		}, ErrEvidenceCapOutOfRange},
		{"per-block caps leave no headroom", func(c *Config) {
			c.WindowTokens = 32768
			c.InputTargetTokens = 8192
			c.OutputReserveTokens = 256
			c.SummaryMaxTokens = 2048
			c.HistoryMaxTokens = 3000
			c.EvidenceMaxTokens = 3000
		}, ErrSubBudgetsExceedTarget},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config := DefaultConfig()
			testCase.patch(&config)
			_, err := config.Resolve()
			if !errors.Is(err, testCase.want) {
				t.Fatalf("Resolve() = %v, want %v", err, testCase.want)
			}
		})
	}
}

func TestCheckOutputCapComparesAgainstTheReserve(t *testing.T) {
	config := validConfig(t)
	if err := config.CheckOutputCap(config.OutputReserveTokens); err != nil {
		t.Fatalf("an output limit equal to the reserve was refused: %v", err)
	}
	if err := config.CheckOutputCap(config.OutputReserveTokens + 1); !errors.Is(err, ErrOutputCapExceedsReserve) {
		t.Fatalf("CheckOutputCap above the reserve = %v, want %v", err, ErrOutputCapExceedsReserve)
	}
	// Zero means the provider's default is not stated here, which is not the same as
	// "no limit"; the assembly point compares the real figure somewhere else.
	if err := config.CheckOutputCap(0); err != nil {
		t.Fatalf("an unset output limit was refused: %v", err)
	}
}

func parts(systemPrompt, summary string, history []responder.Turn, question string,
	tools []model.ToolDefinition,
) Parts {
	// Every pre-existing case in this file describes a turn whose evidence
	// tools are reachable - the ordinary agent turn - so the helper pins the
	// flag that makes the window hold the evidence allowance back. A test that
	// exercises the unreachable branch sets the field itself.
	return Parts{SystemPrompt: systemPrompt, Summary: summary, History: history,
		Question: question, Tools: tools, DocumentKnowledgeReachable: true}
}

func exchange(index int, runes int) []responder.Turn {
	body := strings.Repeat("记", runes)
	return []responder.Turn{
		{Role: model.RoleUser, Content: body + "问" + string(rune('A'+index))},
		{Role: model.RoleAssistant, Content: body + "答" + string(rune('A'+index))},
	}
}

func TestBuildRefusesRatherThanDroppingTheQuestion(t *testing.T) {
	config := validConfig(t)
	// One token past what is left once the evidence allowance is held back. The
	// question is the largest of the mandatory parts and the only one a learner
	// controls, so it is the one that can reach this.
	huge := strings.Repeat("超", config.InputTargetTokens-config.EvidenceMaxTokens-MessageTokens("规则")+1)
	_, _, err := Build(parts("规则", "", nil, huge, nil), config)
	if !errors.Is(err, ErrRequiredExceedsBudget) {
		t.Fatalf("a question of %d runes = %v, want a required-parts refusal", len([]rune(huge)), err)
	}
}

// The window keeps whole exchanges. The reference project could cut its newest message
// in half to squeeze it in, which shows the model a question whose answer went away.
func TestBuildKeepsWholeExchangesAndStopsAtTheFirstIllFit(t *testing.T) {
	config := validConfig(t)
	history := []responder.Turn{}
	history = append(history, exchange(0, 5)...)
	history = append(history, exchange(1, 5)...)
	// One enormous exchange in the middle: newer exchanges must not be pulled in
	// across the hole, and the enormous one must not be halved.
	history = append(history, exchange(2, 3000)...)
	history = append(history, exchange(3, 5)...)

	messages, breakdown, err := Build(parts("规则", "", history, "本问", nil), config)
	if err != nil {
		t.Fatal(err)
	}
	if breakdown.HistoryKept != 1 || breakdown.HistoryDropped != 3 {
		t.Fatalf("kept %d exchanges and dropped %d, want the newest one kept and the rest left out",
			breakdown.HistoryKept, breakdown.HistoryDropped)
	}
	// messages: system, the kept pair, the current question.
	if len(messages) != 4 {
		t.Fatalf("%d messages, want system plus one exchange plus the question:\n%s",
			len(messages), shape(messages))
	}
	if messages[1].Content == "" || messages[2].Content == "" {
		t.Fatal("an exchange reached the model with only half of it")
	}
	if messages[3].Role != model.RoleUser || messages[3].Content != "本问" {
		t.Fatalf("the last message is not the current question: %q", messages[3].Content)
	}
	if strings.Count(join(messages), "本问") != 1 {
		t.Fatal("the current question reached the model more than once")
	}
}

func TestBuildTruncatesAnOversizedSummaryAndSaysSo(t *testing.T) {
	config := validConfig(t)
	long := strings.Repeat("摘", config.SummaryMaxTokens*2)
	messages, breakdown, err := Build(parts("规则", long, nil, "本问", nil), config)
	if err != nil {
		t.Fatal(err)
	}
	if !breakdown.SummaryTruncated || breakdown.SummaryDropped {
		t.Fatalf("truncated=%v dropped=%v, want a shortened summary reported as shortened",
			breakdown.SummaryTruncated, breakdown.SummaryDropped)
	}
	if breakdown.SummaryTokens > config.SummaryMaxTokens {
		t.Fatalf("the injected summary cost %d tokens against a cap of %d",
			breakdown.SummaryTokens, config.SummaryMaxTokens)
	}
	block := messages[1].Content
	if !strings.HasPrefix(block, responder.SummaryLabel) {
		t.Fatalf("the summary block lost its label, so a compressed past reads like a present instruction:\n%s", block)
	}
	if !strings.HasSuffix(block, "…") {
		t.Fatalf("a cut summary does not say it was cut:\n%s", block)
	}
}

// A summary that cannot fit at all is reported as absent, not as an empty summary: the
// two read the same in the prompt and mean different things to whoever is debugging.
func TestBuildDropsTheSummaryWhenThereIsNoRoomForItsEnvelope(t *testing.T) {
	config := validConfig(t)
	// Leave a handful of tokens of room - enough for the required parts to pass, not
	// enough to carry the summary's own label and preamble.
	available := config.InputTargetTokens - config.EvidenceMaxTokens
	prompt := strings.Repeat("规", available-MessageTokens("问")-10)
	_, breakdown, err := Build(parts(prompt, "摘要", nil, "问", nil), config)
	if err != nil {
		t.Fatalf("the turn should still be answerable from its required parts: %v", err)
	}
	if !breakdown.SummaryDropped || breakdown.SummaryTokens != 0 {
		t.Fatalf("dropped=%v tokens=%d, want the summary absent and costed at zero",
			breakdown.SummaryDropped, breakdown.SummaryTokens)
	}
}

func TestBuildPricesTheToolOffersEveryCallCarries(t *testing.T) {
	config := validConfig(t)
	tools := []model.ToolDefinition{{
		Name:        "knowledge_search",
		Description: "在用户自己的知识库里检索资料",
		Parameters:  []byte(`{"type":"object","properties":{"query":{"type":"string"}}}`),
	}}
	_, withTools, err := Build(parts("规则", "", nil, "本问", tools), config)
	if err != nil {
		t.Fatal(err)
	}
	_, withoutTools, err := Build(parts("规则", "", nil, "本问", nil), config)
	if err != nil {
		t.Fatal(err)
	}
	if withTools.ToolTokens == 0 {
		t.Fatal("the tool offers were not costed at all")
	}
	if withTools.AssembledTokens <= withoutTools.AssembledTokens {
		t.Fatalf("identical prompts cost the same although one carries schemas: %d vs %d",
			withTools.AssembledTokens, withoutTools.AssembledTokens)
	}
	if withTools.RequiredTokens-withoutTools.RequiredTokens != withTools.ToolTokens {
		t.Fatalf("the schemas are outside the required parts: %d - %d != %d",
			withTools.RequiredTokens, withoutTools.RequiredTokens, withTools.ToolTokens)
	}
}

// The evidence allowance is held back before anything is chosen, so a turn that later
// decides to search does not discover that its history already used the room.
func TestBuildHoldsTheEvidenceAllowanceBack(t *testing.T) {
	config := validConfig(t)
	_, breakdown, err := Build(parts("规则", "", nil, "本问", nil), config)
	if err != nil {
		t.Fatal(err)
	}
	if breakdown.EvidenceReserved != config.EvidenceMaxTokens {
		t.Fatalf("reserved %d, want the whole configured allowance %d",
			breakdown.EvidenceReserved, config.EvidenceMaxTokens)
	}
	if breakdown.AssembledTokens > config.InputTargetTokens-config.EvidenceMaxTokens {
		t.Fatal("the opening prompt ate into the evidence allowance")
	}
}

// The reserve is conditional, not decorative: a turn whose evidence tools are
// not reachable does not pay for them, and the tokens win back to the
// conversation the question actually is about. This is the same conditional the
// reference project drew, and it holds only while the caller keeps the tool
// offers in step - which is the pairing the harness guards.
func TestBuildSkipsTheEvidenceReserveWhenKnowledgeIsUnreachable(t *testing.T) {
	config := validConfig(t)
	unreachable := parts("规则", "", nil, "本问", nil)
	unreachable.DocumentKnowledgeReachable = false
	_, breakdown, err := Build(unreachable, config)
	if err != nil {
		t.Fatal(err)
	}
	if breakdown.EvidenceReserved != 0 {
		t.Fatalf("reserved %d, want nothing held back when no evidence tool is reachable",
			breakdown.EvidenceReserved)
	}
	// The same turn with the flag on reserves the allowance, so the two lines
	// differ by exactly the reserve in what they may spend on history.
	reachable := parts("规则", "", nil, "本问", nil)
	_, reachableBreakdown, err := Build(reachable, config)
	if err != nil {
		t.Fatal(err)
	}
	if reachableBreakdown.EvidenceReserved != config.EvidenceMaxTokens {
		t.Fatalf("reachable reserve %d, want %d",
			reachableBreakdown.EvidenceReserved, config.EvidenceMaxTokens)
	}
}

// The task state is required context, not an optional block: it is priced into
// the required parts, placed between the policy and everything else, and a turn
// whose task state cannot fit is refused rather than sent half-said.
func TestBuildCarriesTheTaskStateAsRequiredContext(t *testing.T) {
	config := validConfig(t)
	taskState := "Active learning task (structured server state; ordinary chat is not an answer):\n" +
		"- topic: TCP 粘包\n- status: waiting_answer\n- progress: 2/5"
	p := parts("规则", "", nil, "本问", nil)
	p.TaskState = taskState
	messages, breakdown, err := Build(p, config)
	if err != nil {
		t.Fatal(err)
	}
	if breakdown.TaskStateTokens == 0 {
		t.Fatal("the task state was not costed at all")
	}
	if len(messages) < 2 || messages[1].Role != model.RoleSystem || messages[1].Content != taskState {
		t.Fatalf("the task state is not the message right after the policy:\n%+v", messages[:2])
	}
	if breakdown.RequiredTokens != MessageTokens("规则")+MessageTokens("本问")+
		MessageTokens(taskState) {
		t.Fatalf("required %d, want policy + question + task state", breakdown.RequiredTokens)
	}
	// A task state too large for the target is refused, the same way an
	// oversized question is: required parts do not trim.
	huge := parts("规则", "", nil, "本问", nil)
	huge.TaskState = strings.Repeat("题", config.InputTargetTokens)
	if _, _, err := Build(huge, config); !errors.Is(err, ErrRequiredExceedsBudget) {
		t.Fatalf("an oversized task state = %v, want a required-parts refusal", err)
	}
}

// An empty task state changes nothing: no message, no cost, no shift in the
// layout. A session with no open quiz must look exactly like it did before the
// field existed.
func TestBuildLeavesAnEmptyTaskStateOut(t *testing.T) {
	config := validConfig(t)
	without := parts("规则", "", nil, "本问", nil)
	messages, breakdown, err := Build(without, config)
	if err != nil {
		t.Fatal(err)
	}
	if breakdown.TaskStateTokens != 0 {
		t.Fatalf("an empty task state cost %d tokens", breakdown.TaskStateTokens)
	}
	with := parts("规则", "", nil, "本问", nil)
	with.TaskState = "state"
	withMessages, _, err := Build(with, config)
	if err != nil {
		t.Fatal(err)
	}
	if len(withMessages) != len(messages)+1 {
		t.Fatalf("an empty task state produced %d messages and a set one %d",
			len(messages), len(withMessages))
	}
}

func TestCheckRequestGrowsWithTheTurn(t *testing.T) {
	config := Config{
		WindowTokens:        MinWindowTokens,
		InputTargetTokens:   4096,
		OutputReserveTokens: MinOutputReserveTokens,
		SummaryMaxTokens:    MinSummaryMaxTokens,
		HistoryMaxTokens:    MinHistoryMaxTokens,
		HistoryFetchLimit:   MinHistoryFetchLimit,
		EvidenceMaxTokens:   MinEvidenceMaxTokens,
	}
	config, err := config.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	request := model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: "问"}}}
	if err := CheckRequest(request, config, 256); err != nil {
		t.Fatalf("a one-message request was refused: %v", err)
	}
	// The same turn after three tool observations: each one stays in the message list,
	// so the next call carries all of them.
	for index := 0; index < 3; index++ {
		request.Messages = append(request.Messages,
			model.Message{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{
				{ID: "c", Name: "knowledge_search", Arguments: `{"query":"问"}`},
			}},
			model.Message{Role: model.RoleTool, ToolCallID: "c", Content: strings.Repeat("资", 1500)},
		)
	}
	if err := CheckRequest(request, config, 256); !errors.Is(err, ErrAssembledExceedsTarget) {
		t.Fatalf("a grown request = %v, want an input-target refusal", err)
	}
}

func TestCheckRequestChargesTheOutputLimitItWillActuallyUse(t *testing.T) {
	config := validConfig(t)
	small := model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: "问"}}}
	// The provider default is what a request that names no limit will use, and it is
	// still compared: an override can only ever be applied to a request that asked.
	if err := CheckRequest(small, config, config.WindowTokens+1); !errors.Is(err, ErrWindowExceeded) {
		t.Fatalf("a default output limit past the window = %v, want a window refusal", err)
	}
	oversized := small
	oversized.MaxOutputTokens = config.WindowTokens + 1
	if err := CheckRequest(oversized, config, 1024); !errors.Is(err, ErrWindowExceeded) {
		t.Fatalf("a request-set output limit past the window = %v, want a window refusal", err)
	}
}

// An orphan in the middle must not shift every later exchange by one message: an answer
// presented as somebody's question, with the next real question as its answer, is a
// fabricated turn rather than a trimmed one. The transcript here is question→answer, then
// a question nobody answered, then a complete exchange.
func TestBuildDoesNotShiftPairsAroundAnOrphan(t *testing.T) {
	config := validConfig(t)
	history := []responder.Turn{
		{Role: model.RoleUser, Content: "甲问"},
		{Role: model.RoleAssistant, Content: "甲答"},
		{Role: model.RoleUser, Content: "乙问没人答"},
		{Role: model.RoleUser, Content: "丙问"},
		{Role: model.RoleAssistant, Content: "丙答"},
	}
	messages, breakdown, err := Build(parts("规则", "", history, "本问", nil), config)
	if err != nil {
		t.Fatal(err)
	}
	if breakdown.HistoryKept != 2 {
		t.Fatalf("kept %d exchanges, want the two that were answered", breakdown.HistoryKept)
	}
	text := join(messages)
	if strings.Contains(text, "乙问没人答") {
		t.Fatal("an unanswered question reached the model, which invites it to answer the same thing again")
	}
	if !strings.Contains(text, "甲问") || !strings.Contains(text, "丙答") {
		t.Fatalf("a complete exchange went missing:\n%s", text)
	}
	// Roles are what keeps the two surviving exchanges from reading as one invented turn:
	// the orphan's removal must not turn 丙问 into an answer or 甲答 into a question.
	roles := make([]string, 0, len(messages))
	for _, message := range messages {
		roles = append(roles, message.Role+"|"+message.Content)
	}
	want := []string{"system|规则", "user|甲问", "assistant|甲答", "user|丙问", "assistant|丙答", "user|本问"}
	if strings.Join(roles, " ") != strings.Join(want, " ") {
		t.Fatalf("the window is not the two answered exchanges plus the question:\n%v", roles)
	}
}

func join(messages []model.Message) string {
	var builder strings.Builder
	for _, message := range messages {
		builder.WriteString(message.Content)
		builder.WriteString("\n")
	}
	return builder.String()
}

func shape(messages []model.Message) string {
	parts := make([]string, 0, len(messages))
	for _, message := range messages {
		parts = append(parts, message.Role+":"+string([]rune(message.Content)[0]))
	}
	return strings.Join(parts, " ")
}
