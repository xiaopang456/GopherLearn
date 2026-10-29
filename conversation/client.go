package conversation

import (
	"errors"
	"github.com/xiaopang456/GopherLearn/internal/app"
	"github.com/xiaopang456/GopherLearn/internal/openai"
)

type ClientConfig = openai.Config

// OrdinaryConfigKeys returns only the retained ordinary-client/context/summary
// settings. No clean Agent assembly or environment lookup is performed here.
func OrdinaryConfigKeys() []string {
	return []string{app.EnvProvider, openai.EnvBaseURL, openai.EnvAPIKey, openai.EnvModel,
		openai.EnvEnableThinking, openai.EnvTemperature, openai.EnvMaxOutputTokens, openai.EnvTimeout,
		app.EnvContextMaxMessages, app.EnvContextMaxRunes, app.EnvSummaryTriggerMessages,
		app.EnvSummaryTriggerTokens, app.EnvSummaryKeepRecentMessages, app.EnvSummaryMaxInputTokens,
		app.EnvSummaryMaxOutputTokens, app.EnvSummaryTimeout, app.EnvSummaryStageBudget, app.EnvSummaryFailureCooldown}
}

// ConfigureClient forwards the original configuration parser with an explicit
// lookup restricted to ordinary fields; Agent budgets are never passed to Chat.
func ConfigureClient(lookup func(string) string) (ClientConfig, Options, error) {
	allowed := map[string]bool{}
	for _, key := range OrdinaryConfigKeys() {
		allowed[key] = true
	}
	c, err := app.ConfigFromEnvironment(func(key string) string {
		if lookup != nil && allowed[key] {
			return lookup(key)
		}
		return ""
	})
	if err != nil {
		return ClientConfig{}, Options{}, err
	}
	if c.Provider != app.ProviderOpenAI {
		return ClientConfig{}, Options{}, errors.New("explicit openai_compatible ordinary client required")
	}
	return c.Model, Options{Budget: Budget{MaxMessages: c.Context.MaxMessages, MaxRunes: c.Context.MaxRunes}, SummaryPolicy: c.Summary, SummaryEnabled: true}, nil
}

func NewClient(config ClientConfig) (Model, error) { return openai.New(config) }
