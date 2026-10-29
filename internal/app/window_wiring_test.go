package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/agent"
	"github.com/xiaopang456/GopherLearn/internal/contextwindow"
	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/openai"
	"github.com/xiaopang456/GopherLearn/internal/responder"
)

// The window configuration is only worth having if the numbers an operator sets are the
// numbers the running process enforces. These tests follow one figure from the
// environment through the resolved configuration into the request a provider stand-in
// actually received, because "configured" and "consumed" are different claims.

// resolveWindow turns a hand-written budget into the one an assembly would enforce, and
// fails the test if the fixture itself is not a legal configuration.
func resolveWindow(t *testing.T, config contextwindow.Config) contextwindow.Config {
	t.Helper()
	resolved, err := config.Resolve()
	if err != nil {
		t.Fatalf("the test window is not a legal budget: %v", err)
	}
	return resolved
}

func TestWindowLimitsComeFromTheEnvironment(t *testing.T) {
	values := openAIEnvironment()
	values[EnvWindowTokens] = "16384"
	values[EnvInputTargetTokens] = "12000"
	values[EnvOutputReserveTokens] = "4096"
	values[EnvSummaryInjectTokens] = "1024"
	values[EnvHistoryBudgetTokens] = "3000"
	values[EnvHistoryFetchLimit] = "64"
	values[EnvEvidenceBudgetTokens] = "5000"
	config, err := ConfigFromEnvironment(lookup(values))
	if err != nil {
		t.Fatal(err)
	}
	want := contextwindow.Config{
		WindowTokens: 16384, InputTargetTokens: 12000, OutputReserveTokens: 4096,
		SummaryMaxTokens: 1024, GlobalMemoryMaxTokens: 800, TopicMemoryMaxTokens: 1200,
		MasteryMaxTokens: 900, EpisodeMaxTokens: 1000, HistoryMaxTokens: 3000,
		HistoryMaxMessages: 0, HistoryMaxRunes: 0, HistoryFetchLimit: 64, EvidenceMaxTokens: 5000,
	}
	if config.Context.Window != want {
		t.Fatalf("window = %+v, want %+v", config.Context.Window, want)
	}
	// The agent's history keeps the two ceilings the other strategies use even when the
	// environment only sets the token one, because a token cap alone is wider than the
	// rune cap it replaced for Latin-script text.
	tightened := openAIEnvironment()
	tightened[EnvContextMaxMessages] = "4"
	tightened[EnvContextMaxRunes] = "1500"
	tightenedConfig, err := ConfigFromEnvironment(lookup(tightened))
	if err != nil {
		t.Fatal(err)
	}
	if tightenedConfig.Context.Window.HistoryMaxMessages != 4 ||
		tightenedConfig.Context.Window.HistoryMaxRunes != 1500 {
		t.Fatalf("the shared ceilings did not reach the agent window: %+v",
			tightenedConfig.Context.Window)
	}
	if tightenedConfig.Context.MaxMessages != 4 || tightenedConfig.Context.MaxRunes != 1500 {
		t.Fatalf("the chat ceilings moved on their own: %+v", tightenedConfig.Context)
	}
	// The turn's clock is the agent's deadline, carried to the conversation service so
	// its summary refresh and the loop spend the same budget.
	if config.Context.AgentTurnDeadline != config.Agent.Deadline {
		t.Fatalf("turn deadline = %v, want the agent deadline %v",
			config.Context.AgentTurnDeadline, config.Agent.Deadline)
	}

	defaults, err := ConfigFromEnvironment(lookup(openAIEnvironment()))
	if err != nil {
		t.Fatal(err)
	}
	if defaults.Context.Window != contextwindow.DefaultConfig() {
		t.Fatalf("unset window = %+v, want the documented defaults", defaults.Context.Window)
	}
}

// Each illegal relationship stops startup by name. A budget that repaired itself would be
// reported in the startup line as if the operator had chosen it, and the two figures that
// disagree would then be invisible.
func TestWindowRefusesAnIllegalBudgetAtStartup(t *testing.T) {
	cases := []struct {
		name  string
		patch map[string]string
		want  error
	}{
		{"window below the floor", map[string]string{
			EnvWindowTokens: "4096", EnvInputTargetTokens: "2500", EnvOutputReserveTokens: "256",
			EnvEvidenceBudgetTokens: "128", EnvSummaryInjectTokens: "128", EnvHistoryBudgetTokens: "256",
		}, contextwindow.ErrWindowOutOfRange},
		{"input target plus reserve over the window", map[string]string{
			EnvWindowTokens: "16384", EnvInputTargetTokens: "14000", EnvOutputReserveTokens: "4096",
			EnvEvidenceBudgetTokens: "1000", EnvSummaryInjectTokens: "256", EnvHistoryBudgetTokens: "2048",
		}, contextwindow.ErrTargetExceedsWindow},
		{"evidence cap swallows the target", map[string]string{
			EnvWindowTokens: "32768", EnvInputTargetTokens: "8192", EnvOutputReserveTokens: "256",
			EnvEvidenceBudgetTokens: "8192", EnvSummaryInjectTokens: "256", EnvHistoryBudgetTokens: "256",
		}, contextwindow.ErrEvidenceCapOutOfRange},
		{"per-block caps leave no headroom", map[string]string{
			EnvWindowTokens: "32768", EnvInputTargetTokens: "8192", EnvOutputReserveTokens: "256",
			EnvEvidenceBudgetTokens: "1024", EnvSummaryInjectTokens: "4000", EnvHistoryBudgetTokens: "4000",
		}, contextwindow.ErrSubBudgetsExceedTarget},
		{"fetch limit cannot cover an exchange", map[string]string{
			EnvHistoryFetchLimit: "1",
		}, contextwindow.ErrHistoryFetchLimitOutOfRange},
		{"provider output above the reserve", map[string]string{
			EnvOutputReserveTokens:    "1024",
			openai.EnvMaxOutputTokens: "4096",
		}, contextwindow.ErrOutputCapExceedsReserve},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			values := openAIEnvironment()
			for name, value := range testCase.patch {
				values[name] = value
			}
			_, err := ConfigFromEnvironment(lookup(values))
			if !errors.Is(err, testCase.want) {
				t.Fatalf("config = %v, want %v", err, testCase.want)
			}
		})
	}
}

func TestWindowRefusesAnUnparsableFigureAtStartup(t *testing.T) {
	for _, name := range []string{
		EnvWindowTokens, EnvInputTargetTokens, EnvOutputReserveTokens, EnvSummaryInjectTokens,
		EnvHistoryBudgetTokens, EnvHistoryFetchLimit, EnvEvidenceBudgetTokens,
	} {
		for _, bad := range []string{"0", "-1", "many"} {
			values := openAIEnvironment()
			values[name] = bad
			if _, err := ConfigFromEnvironment(lookup(values)); !errors.Is(err, ErrInvalidWindowLimit) {
				t.Fatalf("%s=%q was accepted: %v", name, bad, err)
			}
		}
	}
}

// TestAssembledAgentHoldsEveryCallInsideTheConfiguredWindow is the reachability proof: the
// window is set through the same configuration path a deployment uses, the assembly is the
// production one, and the assertion is on the bytes a provider stand-in was handed.
func TestAssembledAgentHoldsEveryCallInsideTheConfiguredWindow(t *testing.T) {
	store, _, _, _ := agentFixture(t)
	config, err := ConfigFromEnvironment(lookup(openAIEnvironment()))
	if err != nil {
		t.Fatal(err)
	}
	config.Context.Window = resolveWindow(t, contextwindow.Config{
		WindowTokens: 8192, InputTargetTokens: 4096, OutputReserveTokens: 1024,
		SummaryMaxTokens: 512, HistoryMaxTokens: 1024, HistoryFetchLimit: 32,
		EvidenceMaxTokens: 1024,
	})
	config.Model.MaxOutputTokens = 1024
	config.Agent = agent.Budget{MaxModelCalls: 3, MaxToolCalls: 2, MaxSteps: 6}

	chat := &toolChat{replies: []model.Completion{
		searchReply("AOF fsync"),
		{Content: "可以选 always、everysec 或 no。", FinishReason: model.FinishStop},
	}}
	assembly, err := Assemble(store, config, Options{NewModel: func(openai.Config) (model.ChatModel, error) {
		return chat, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"window_tokens=8192", "input_target_tokens=4096",
		"evidence_tokens=1024", "summary_stage_budget="} {
		if !strings.Contains(assembly.StartupLine, marker) {
			t.Fatalf("the startup line does not report %s:\n%s", marker, assembly.StartupLine)
		}
	}

	// A prompt that starts far larger than the window: a long summary and long
	// exchanges, both of which the assembly has to bound before anything is sent.
	question := seededRequest(t, store, domain.AnswerStrategyAgent, "那 mycnf 里怎么配？",
		strings.Repeat("摘要", 4000))
	question.History = nil
	for index := 0; index < 8; index++ {
		body := strings.Repeat("经", 600)
		question.History = append(question.History,
			responder.Turn{Role: model.RoleUser, Content: body},
			responder.Turn{Role: model.RoleAssistant, Content: body},
		)
	}
	if _, err := assembly.Assistant.Respond(context.Background(), question); err != nil {
		t.Fatal(err)
	}
	if len(chat.toolRequest) == 0 {
		t.Fatal("no agent call was captured")
	}
	available := config.Context.Window.InputTargetTokens - config.Context.Window.EvidenceMaxTokens
	for index, request := range chat.toolRequest {
		if got := contextwindow.RequestTokens(request); got > available {
			t.Fatalf("call %d estimated %d tokens against the %d the window leaves for it",
				index, got, available)
		}
		if len(request.Tools) == 0 {
			t.Fatalf("call %d carried no tool offers, so the measurement would be incomplete", index)
		}
	}
	// The turn still answers, and the summary still arrives labelled: bounding a
	// window must not mean dropping the guard that says the summary is data.
	if !strings.Contains(chat.toolRequest[0].Messages[1].Content, responder.SummaryLabel) {
		t.Fatalf("the summary block lost its label while being bounded:\n%+v", chat.toolRequest[0].Messages[1])
	}
}
