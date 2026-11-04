package conversation

import (
	"errors"
	"github.com/xiaopang456/GopherLearn/internal/openai"
	"github.com/xiaopang456/GopherLearn/internal/ordinaryconfig"
)

type ClientConfig = openai.Config

// OrdinaryConfigKeys returns only the retained ordinary-client/context/summary
// settings. No clean Agent assembly or environment lookup is performed here.
func OrdinaryConfigKeys() []string {
	return []string{ordinaryconfig.EnvProvider, openai.EnvBaseURL, openai.EnvAPIKey, openai.EnvModel,
		openai.EnvEnableThinking, openai.EnvThinkingDialect,
		openai.EnvTemperature, openai.EnvMaxOutputTokens, openai.EnvTimeout,
		ordinaryconfig.EnvContextMaxMessages, ordinaryconfig.EnvContextMaxRunes,
		ordinaryconfig.EnvSummaryTriggerMessages, ordinaryconfig.EnvSummaryTriggerTokens,
		ordinaryconfig.EnvSummaryKeepRecentMessages, ordinaryconfig.EnvSummaryMaxInputTokens,
		ordinaryconfig.EnvSummaryMaxOutputTokens, ordinaryconfig.EnvSummaryTimeout,
		ordinaryconfig.EnvSummaryStageBudget, ordinaryconfig.EnvSummaryFailureCooldown}
}

// ConfigureClient reads the settings a chat turn uses, through the same explicit
// allow-list it always had: a key outside OrdinaryConfigKeys is handed to the
// parsers as empty, so an Agent budget can never reach Chat.
//
// The three families are read here rather than through one combined environment
// parse, because the settings that used to sit between them - the learning agent's
// context window, its call budgets, the scripted quiz model - are not in the
// allow-list. They were always read as empty strings from this path and could only
// default, so dropping them changes nothing a caller could observe, and it is what
// keeps the legacy agent assembly out of the integrated binary.
func ConfigureClient(lookup func(string) string) (ClientConfig, Options, error) {
	allowed := map[string]bool{}
	for _, key := range OrdinaryConfigKeys() {
		allowed[key] = true
	}
	filtered := func(key string) string {
		if lookup != nil && allowed[key] {
			return lookup(key)
		}
		return ""
	}
	provider, model, err := ordinaryconfig.ProviderAndModel(filtered)
	if err != nil {
		return ClientConfig{}, Options{}, err
	}
	messages, runes, err := ordinaryconfig.ContextLimits(filtered)
	if err != nil {
		return ClientConfig{}, Options{}, err
	}
	policy, err := ordinaryconfig.SummaryPolicy(filtered)
	if err != nil {
		return ClientConfig{}, Options{}, err
	}
	// Checked last, exactly as before: a configuration that names both a non
	// openai_compatible provider and an unusable summary figure reports the summary
	// figure, because that was the order the combined parse resolved them in.
	if provider != ordinaryconfig.ProviderOpenAI {
		return ClientConfig{}, Options{}, errors.New("explicit openai_compatible ordinary client required")
	}
	budget := ordinaryconfig.Budget(messages, runes)
	return model, Options{Budget: Budget{MaxMessages: budget.MaxMessages, MaxRunes: budget.MaxRunes},
		SummaryPolicy: policy, SummaryEnabled: true}, nil
}

func NewClient(config ClientConfig) (Model, error) { return openai.New(config) }
