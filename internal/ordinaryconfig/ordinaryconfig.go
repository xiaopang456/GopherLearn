// Package ordinaryconfig parses the settings the ordinary conversation path
// reads: which provider answers, how much of the conversation a turn may carry,
// and when the rolling summary is refreshed.
//
// It exists because those settings have two consumers with different reach. The
// baseline entry reads them as part of one large environment parse that also
// covers the agent's context window and its call budgets. The integrated entry
// needs only this package - and must not be dragged through the baseline
// assembly to get it, because that assembly is what builds the legacy agent
// harness it never runs.
//
// So each family is parsed here once, and each consumer sequences the calls
// itself. That keeps a deployment's error precedence unchanged: a configuration
// that names both a bad context ceiling and a bad window ceiling still reports
// the context ceiling first, because the caller decides the order, not a shared
// god-function.
package ordinaryconfig

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/openai"
	"github.com/xiaopang456/GopherLearn/internal/service"
	"github.com/xiaopang456/GopherLearn/internal/summary"
)

// Provider selects which assistant a turn gets. Echo keeps a process free of any
// external call, which is the right default for development and the only safe
// default when nobody configured anything.
type Provider string

const (
	ProviderEcho   Provider = "echo"
	ProviderOpenAI Provider = "openai_compatible"
)

// The environment names ordinary conversation reads. Names stay exactly as
// deployments have always written them.
const (
	EnvProvider           = "GOPHERLEARN_AI_PROVIDER"
	EnvContextMaxMessages = "GOPHERLEARN_CONTEXT_MAX_MESSAGES"
	EnvContextMaxRunes    = "GOPHERLEARN_CONTEXT_MAX_RUNES"

	EnvSummaryTriggerMessages    = "GOPHERLEARN_SUMMARY_TRIGGER_MESSAGES"
	EnvSummaryTriggerTokens      = "GOPHERLEARN_SUMMARY_TRIGGER_TOKENS"
	EnvSummaryKeepRecentMessages = "GOPHERLEARN_SUMMARY_KEEP_RECENT_MESSAGES"
	EnvSummaryMaxInputTokens     = "GOPHERLEARN_SUMMARY_MAX_INPUT_TOKENS"
	EnvSummaryMaxOutputTokens    = "GOPHERLEARN_SUMMARY_MAX_OUTPUT_TOKENS"
	EnvSummaryTimeout            = "GOPHERLEARN_SUMMARY_TIMEOUT"
	EnvSummaryStageBudget        = "GOPHERLEARN_SUMMARY_STAGE_BUDGET"
	EnvSummaryFailureCooldown    = "GOPHERLEARN_SUMMARY_FAILURE_COOLDOWN"
)

var (
	ErrUnsupportedProvider    = errors.New("unsupported " + EnvProvider + " value")
	ErrInvalidContextLimit    = errors.New(EnvContextMaxMessages + " and " + EnvContextMaxRunes + " must be positive integers")
	ErrInvalidSummaryLimit    = errors.New("the summary trigger and input limits must be positive integers")
	ErrInvalidSummaryDuration = errors.New(EnvSummaryTimeout + ", " + EnvSummaryStageBudget + " and " +
		EnvSummaryFailureCooldown + " must be Go durations such as 20s, and positive")
)

// ProviderAndModel reads the provider choice and, once an OpenAI-compatible one is
// named, its client settings. An unset provider keeps a keyless local run working,
// so the model settings are only required in that one branch.
func ProviderAndModel(lookup func(string) string) (Provider, openai.Config, error) {
	if lookup == nil {
		lookup = func(string) string { return "" }
	}
	switch provider := Provider(strings.ToLower(strings.TrimSpace(lookup(EnvProvider)))); provider {
	case "", ProviderEcho:
		return ProviderEcho, openai.Config{}, nil
	case ProviderOpenAI:
		model, err := openai.ConfigFromEnvironment(lookup)
		if err != nil {
			return "", openai.Config{}, err
		}
		return ProviderOpenAI, model, nil
	default:
		return "", openai.Config{}, fmt.Errorf("%w: %q", ErrUnsupportedProvider, string(provider))
	}
}

// ContextLimits reads the two ceilings a conversation turn carries: complete
// message pairs, and the runes they may occupy. Zero means "unset", which
// service.ContextBudget's own Normalize fills with the project default.
func ContextLimits(lookup func(string) string) (messages, runes int, err error) {
	if lookup == nil {
		lookup = func(string) string { return "" }
	}
	if messages, err = PositiveInt(lookup(EnvContextMaxMessages)); err != nil {
		return 0, 0, err
	}
	if runes, err = PositiveInt(lookup(EnvContextMaxRunes)); err != nil {
		return 0, 0, err
	}
	return messages, runes, nil
}

// SummaryPolicy reads when the rolling summary refreshes and how much a refresh may
// show the model. Every value is read as an integer or a duration or startup stops:
// a silently ignored trigger would leave a conversation whose summary no longer
// describes it.
func SummaryPolicy(lookup func(string) string) (summary.Policy, error) {
	if lookup == nil {
		lookup = func(string) string { return "" }
	}
	trigger, err := PositiveIntIn(lookup(EnvSummaryTriggerMessages), ErrInvalidSummaryLimit)
	if err != nil {
		return summary.Policy{}, err
	}
	triggerTokens, err := PositiveIntIn(lookup(EnvSummaryTriggerTokens), ErrInvalidSummaryLimit)
	if err != nil {
		return summary.Policy{}, err
	}
	keepRecent, err := PositiveIntIn(lookup(EnvSummaryKeepRecentMessages), ErrInvalidSummaryLimit)
	if err != nil {
		return summary.Policy{}, err
	}
	inputTokens, err := PositiveIntIn(lookup(EnvSummaryMaxInputTokens), ErrInvalidSummaryLimit)
	if err != nil {
		return summary.Policy{}, err
	}
	outputTokens, err := PositiveIntIn(lookup(EnvSummaryMaxOutputTokens), ErrInvalidSummaryLimit)
	if err != nil {
		return summary.Policy{}, err
	}
	timeout, err := SummaryDuration(lookup(EnvSummaryTimeout))
	if err != nil {
		return summary.Policy{}, err
	}
	stageBudget, err := SummaryDuration(lookup(EnvSummaryStageBudget))
	if err != nil {
		return summary.Policy{}, err
	}
	cooldown, err := SummaryDuration(lookup(EnvSummaryFailureCooldown))
	if err != nil {
		return summary.Policy{}, err
	}
	return summary.Policy{
		TriggerMessages:    trigger,
		TriggerTokens:      triggerTokens,
		KeepRecentMessages: keepRecent,
		MaxInputTokens:     inputTokens,
		MaxOutputTokens:    outputTokens,
		Timeout:            timeout,
		StageBudget:        stageBudget,
		FailureCooldown:    cooldown,
	}.Normalize(), nil
}

// Budget applies two context ceilings over the project defaults, so a caller that
// only has the ceilings never re-implements what "unset" means.
func Budget(messages, runes int) service.ContextBudget {
	budget := service.ContextBudget{}.Normalize()
	if messages > 0 {
		budget.MaxMessages = messages
	}
	if runes > 0 {
		budget.MaxRunes = runes
	}
	return budget
}

// PositiveInt reads one optional positive integer. Absent means unset.
func PositiveInt(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, ErrInvalidContextLimit
	}
	return value, nil
}

// PositiveIntIn reads one optional positive integer and reports it under the
// family's own error, so a bad window figure is refused as a window problem rather
// than as the generic context-limit one. Absent means unset.
func PositiveIntIn(raw string, refusal error) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%w: got %q", refusal, strings.TrimSpace(raw))
	}
	return value, nil
}

// SummaryDuration reads one optional duration. An unparsable value stops startup: a
// silently ignored timeout is the one budget whose absence cannot be spotted in a
// line about token counts.
func SummaryDuration(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%w: got %q", ErrInvalidSummaryDuration, raw)
	}
	return value, nil
}
