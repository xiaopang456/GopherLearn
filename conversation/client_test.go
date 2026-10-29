package conversation

import (
	"github.com/xiaopang456/GopherLearn/internal/app"
	"github.com/xiaopang456/GopherLearn/internal/openai"
	"reflect"
	"testing"
)

func TestOrdinaryClientForwardsRetainedConfigAndNoAgentBudget(t *testing.T) {
	values := map[string]string{app.EnvProvider: "openai_compatible", openai.EnvAPIKey: "synthetic-never-sent", openai.EnvBaseURL: "https://ordinary.invalid/v1", openai.EnvModel: "ordinary", openai.EnvEnableThinking: "true", openai.EnvTemperature: "0.4", openai.EnvMaxOutputTokens: "1500", openai.EnvTimeout: "42s", app.EnvContextMaxMessages: "18", app.EnvContextMaxRunes: "8000", app.EnvSummaryTimeout: "7s"}
	lookup := func(key string) string { return values[key] }
	original, err := app.ConfigFromEnvironment(lookup)
	if err != nil {
		t.Fatal(err)
	}
	c, options, err := ConfigureClient(lookup)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c, original.Model) || !reflect.DeepEqual(options.SummaryPolicy, original.Summary) || options.Budget.MaxMessages != original.Context.MaxMessages || options.Budget.MaxRunes != original.Context.MaxRunes || options.Budget.AgentTurnDeadline != 0 || !reflect.DeepEqual(options.Budget.Window, Budget{}.Window) || !options.SummaryEnabled {
		t.Fatal("retained ordinary configuration drifted")
	}
	if _, err := NewClient(c); err != nil {
		t.Fatal(err)
	}
	values[app.EnvAgentMaxModelCalls] = "invalid-unused"
	if _, _, err := ConfigureClient(lookup); err != nil {
		t.Fatal("clean Agent setting leaked into ordinary parser")
	}
}
