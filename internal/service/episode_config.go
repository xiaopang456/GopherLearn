package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// Episode configuration. Everything is an environment variable with a working
// default, and the whole subsystem is off unless GOPHERLEARN_EPISODES says on:
// a deployment that never asked for episodic memory must not grow a background
// model caller, and every existing behaviour stays byte-identical when the
// switch is absent.

// Environment variable names owned by the episodic memory system.
const (
	EnvEpisodes                = "GOPHERLEARN_EPISODES"
	EnvEpisodeQuietWindow      = "GOPHERLEARN_EPISODE_QUIET_WINDOW"
	EnvEpisodeScanInterval     = "GOPHERLEARN_EPISODE_SCAN_INTERVAL"
	EnvEpisodeReconcileSeconds = "GOPHERLEARN_EPISODE_RECONCILE_INTERVAL"
	EnvEpisodeTaskTimeout      = "GOPHERLEARN_EPISODE_TASK_TIMEOUT"
	EnvEpisodeLease            = "GOPHERLEARN_EPISODE_LEASE"
	EnvEpisodeRecoveryInterval = "GOPHERLEARN_EPISODE_RECOVERY_INTERVAL"
	EnvEpisodeRetryDelay       = "GOPHERLEARN_EPISODE_RETRY_DELAY"
	EnvEpisodeMaxAttempts      = "GOPHERLEARN_EPISODE_MAX_ATTEMPTS"
	EnvEpisodeMaxMessages      = "GOPHERLEARN_EPISODE_MAX_MESSAGES"
	EnvEpisodeMaxInputTokens   = "GOPHERLEARN_EPISODE_MAX_INPUT_TOKENS"
	EnvEpisodeMaxOutputTokens  = "GOPHERLEARN_EPISODE_MAX_OUTPUT_TOKENS"
	EnvEpisodeWorkerCount      = "GOPHERLEARN_EPISODE_WORKER_COUNT"
	EnvEpisodeQueueSize        = "GOPHERLEARN_EPISODE_QUEUE_SIZE"
	// EnvEpisodeRecallMode selects the context-supply recall mode: disabled,
	// shadow_only or inject_enabled. It governs the supply the context builder
	// may consume, not the episode_search tool, which a model calls on its own.
	EnvEpisodeRecallMode = "GOPHERLEARN_EPISODE_RECALL_MODE"
	// EnvEpisodeAdmissionMode selects the fusion admission rule: legacy (the
	// shipped rule) or v3 (the experiment's corroboration bound).
	EnvEpisodeAdmissionMode = "GOPHERLEARN_EPISODE_ADMISSION_MODE"
)

// EpisodeRecallMode is the three-state switch the source project runs its
// episodic context supply with. It exists because "only observe, never inject"
// is the only way to evaluate the supply on live sessions without clearing the
// store: shadow recalls exactly what injection would have served and records
// it, while the window itself stays untouched.
type EpisodeRecallMode string

const (
	// EpisodeRecallDisabled recalls nothing for the context builder.
	EpisodeRecallDisabled EpisodeRecallMode = "disabled"
	// EpisodeRecallShadow is the default: recall runs and its outcome is
	// observable, but nothing is handed to the window.
	EpisodeRecallShadow EpisodeRecallMode = "shadow_only"
	// EpisodeRecallInject hands the recalled cards to the context builder for
	// injection. It is the deployment state the source project's production
	// configuration runs.
	EpisodeRecallInject EpisodeRecallMode = "inject_enabled"
)

// DefaultEpisodeRecallMode is the shipped default: observe without injecting.
// It mirrors the source project's own default and keeps an unconfigured
// deployment from changing what its window carries.
func DefaultEpisodeRecallMode() EpisodeRecallMode { return EpisodeRecallShadow }

// NormalizeEpisodeRecallMode parses a configured mode. An unknown value is an
// error rather than a fallback: a silent fallback is how a mode nobody chose
// ends up running in production.
func NormalizeEpisodeRecallMode(value string) (EpisodeRecallMode, error) {
	switch strings.TrimSpace(value) {
	case "", string(EpisodeRecallShadow):
		return EpisodeRecallShadow, nil
	case string(EpisodeRecallDisabled):
		return EpisodeRecallDisabled, nil
	case string(EpisodeRecallInject):
		return EpisodeRecallInject, nil
	default:
		return "", fmt.Errorf("episode recall mode %q must be disabled, shadow_only or inject_enabled", value)
	}
}

// EpisodeConfig bounds the background projection system. Every duration and
// count has a ceiling: a mistyped value must fail startup, not unbound a loop.
type EpisodeConfig struct {
	WorkerCount       int
	QueueSize         int
	ScanInterval      time.Duration
	ReconcileInterval time.Duration
	TaskTimeout       time.Duration
	// Lease must be strictly longer than TaskTimeout: a lease that expires
	// while the task it covers is still running invites a second worker to
	// start the same projection.
	Lease            time.Duration
	RecoveryInterval time.Duration
	RetryDelay       time.Duration
	// QuietWindow is the debounce: how long after a completed turn a session's
	// projection waits, so an active conversation is not projected on every
	// turn only to be superseded by the next message.
	QuietWindow time.Duration
	MaxAttempts int
	// MaxMessages caps how many complete messages one projection may read.
	MaxMessages int
	// MaxInputTokens and MaxOutputTokens are estimate-based model budgets. The
	// estimator counts CJK characters individually and ASCII words at four per
	// token; it is not a model tokenizer.
	MaxInputTokens  int
	MaxOutputTokens int
	// RecallMode governs the context-supply recall (ContextRecall): disabled,
	// shadow_only or inject_enabled. It does not touch the episode_search tool,
	// which is the model's own choice in every mode.
	RecallMode EpisodeRecallMode
	// AdmissionMode selects the fusion admission rule: legacy (shipped) or v3
	// (the experiment's corroboration bound).
	AdmissionMode EpisodeAdmissionMode
}

// DefaultEpisodeConfig mirrors the original project's production defaults.
func DefaultEpisodeConfig() EpisodeConfig {
	return EpisodeConfig{
		WorkerCount: 1, QueueSize: 32, ScanInterval: 3 * time.Second,
		ReconcileInterval: time.Minute, TaskTimeout: 30 * time.Second, Lease: time.Minute,
		RecoveryInterval: 30 * time.Second, RetryDelay: 15 * time.Second, QuietWindow: 10 * time.Minute,
		MaxAttempts: 3, MaxMessages: 4096, MaxInputTokens: 12000, MaxOutputTokens: 1800,
	}
}

// Validate refuses a configuration the worker cannot honour.
func (c EpisodeConfig) Validate() error {
	if c.WorkerCount < 1 || c.WorkerCount > 16 || c.QueueSize < c.WorkerCount || c.QueueSize > 1000 ||
		c.ScanInterval <= 0 || c.ReconcileInterval <= 0 || c.TaskTimeout <= 0 || c.Lease <= c.TaskTimeout ||
		c.RecoveryInterval <= 0 || c.RetryDelay <= 0 || c.QuietWindow < 0 || c.QuietWindow > 24*time.Hour ||
		c.MaxAttempts < 1 || c.MaxAttempts > 10 || c.MaxMessages < 32 || c.MaxMessages > 20000 ||
		c.MaxInputTokens < 1024 || c.MaxInputTokens > 100000 || c.MaxOutputTokens < 256 || c.MaxOutputTokens > 8000 {
		return fmt.Errorf("invalid episode configuration")
	}
	return nil
}

// EpisodeConfigFromEnvironment reads the subsystem's variables. An absent
// switch means off; a value that is neither on nor off stops startup, because a
// typo left to default quietly to off would be a deployment that believes it is
// remembering something while it remembers nothing.
func EpisodeConfigFromEnvironment(lookup func(string) string) (bool, EpisodeConfig, error) {
	if lookup == nil {
		lookup = func(string) string { return "" }
	}
	enabled := false
	switch strings.ToLower(strings.TrimSpace(lookup(EnvEpisodes))) {
	case "":
	case "on", "true", "1":
		enabled = true
	case "off", "false", "0":
	default:
		return false, EpisodeConfig{}, fmt.Errorf("unsupported %s value %q (want on or off)",
			EnvEpisodes, strings.TrimSpace(lookup(EnvEpisodes)))
	}
	config := DefaultEpisodeConfig()
	var err error
	// Every override is applied only when its variable is actually set: an
	// unset variable keeps the default, and an unparsable one stops startup.
	applyDuration := func(target *time.Duration, name string) error {
		value, err := episodeDuration(lookup, name)
		if err != nil {
			return err
		}
		if value > 0 {
			*target = value
		}
		return nil
	}
	applyInt := func(target *int, name string) error {
		value, err := episodeInt(lookup, name)
		if err != nil {
			return err
		}
		if value > 0 {
			*target = value
		}
		return nil
	}
	if err = applyDuration(&config.ScanInterval, EnvEpisodeScanInterval); err != nil {
		return enabled, EpisodeConfig{}, err
	}
	if err = applyDuration(&config.ReconcileInterval, EnvEpisodeReconcileSeconds); err != nil {
		return enabled, EpisodeConfig{}, err
	}
	if err = applyDuration(&config.TaskTimeout, EnvEpisodeTaskTimeout); err != nil {
		return enabled, EpisodeConfig{}, err
	}
	if err = applyDuration(&config.Lease, EnvEpisodeLease); err != nil {
		return enabled, EpisodeConfig{}, err
	}
	if err = applyDuration(&config.RecoveryInterval, EnvEpisodeRecoveryInterval); err != nil {
		return enabled, EpisodeConfig{}, err
	}
	if err = applyDuration(&config.RetryDelay, EnvEpisodeRetryDelay); err != nil {
		return enabled, EpisodeConfig{}, err
	}
	if err = applyDuration(&config.QuietWindow, EnvEpisodeQuietWindow); err != nil {
		return enabled, EpisodeConfig{}, err
	}
	if err = applyInt(&config.MaxAttempts, EnvEpisodeMaxAttempts); err != nil {
		return enabled, EpisodeConfig{}, err
	}
	if err = applyInt(&config.MaxMessages, EnvEpisodeMaxMessages); err != nil {
		return enabled, EpisodeConfig{}, err
	}
	if err = applyInt(&config.MaxInputTokens, EnvEpisodeMaxInputTokens); err != nil {
		return enabled, EpisodeConfig{}, err
	}
	if err = applyInt(&config.MaxOutputTokens, EnvEpisodeMaxOutputTokens); err != nil {
		return enabled, EpisodeConfig{}, err
	}
	if err = applyInt(&config.WorkerCount, EnvEpisodeWorkerCount); err != nil {
		return enabled, EpisodeConfig{}, err
	}
	if err = applyInt(&config.QueueSize, EnvEpisodeQueueSize); err != nil {
		return enabled, EpisodeConfig{}, err
	}
	// Both modes are parsed rather than defaulted through: an unknown value must
	// stop startup, not quietly pick a rule nobody selected.
	recallMode, err := NormalizeEpisodeRecallMode(lookup(EnvEpisodeRecallMode))
	if err != nil {
		return enabled, EpisodeConfig{}, err
	}
	config.RecallMode = recallMode
	admissionMode, err := NormalizeEpisodeAdmissionMode(lookup(EnvEpisodeAdmissionMode))
	if err != nil {
		return enabled, EpisodeConfig{}, err
	}
	config.AdmissionMode = admissionMode
	if err := config.Validate(); err != nil {
		return enabled, EpisodeConfig{}, err
	}
	return enabled, config, nil
}

func episodeDuration(lookup func(string) string, name string) (time.Duration, error) {
	raw := strings.TrimSpace(lookup(name))
	if raw == "" {
		return 0, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("%s must be a non-negative Go duration such as 30s", name)
	}
	return value, nil
}

func episodeInt(lookup func(string) string, name string) (int, error) {
	raw := strings.TrimSpace(lookup(name))
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return value, nil
}

// Episode recall bounds, mirroring the original project's retriever defaults.
const (
	episodeCandidateDepth = 20
	episodeMaxResults     = 5
)

// episodeProjectionErrorCodes is the closed vocabulary a projection job's
// error_code may carry. The code is persisted and read by operators; each value
// names the stage that failed so an investigation starts in the right place.
const (
	episodeCodeTimeout        = "projection_timeout"
	episodeCodeCancelled      = "projection_cancelled"
	episodeCodeSensitive      = "sensitive_content"
	episodeCodeSourceBoundary = "source_boundary"
	episodeCodeInvalidOutput  = "invalid_model_output"
	episodeCodeSourceMissing  = "source_not_found"
	episodeCodeFailed         = "projection_failed"
	episodeCodeIndexFailed    = "index_failed"
)

// projectionErrorCode maps a projection failure onto the closed vocabulary.
func projectionErrorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded):
		return episodeCodeTimeout
	case errors.Is(err, context.Canceled):
		return episodeCodeCancelled
	case errors.Is(err, errEpisodeSensitive):
		return episodeCodeSensitive
	case errors.Is(err, errEpisodeSourceBoundary):
		return episodeCodeSourceBoundary
	case errors.Is(err, errEpisodeToolProtocol), errors.Is(err, errEmptyEpisode),
		errors.Is(err, errEpisodeInputTooLarge):
		return episodeCodeInvalidOutput
	case errors.Is(err, errProjectionSourceNotFound), errors.Is(err, errProjectionTargetMissing):
		return episodeCodeSourceMissing
	default:
		return episodeCodeFailed
	}
}

// ensureEpisodeErrorCodeValid is a compile-time statement that the codes above
// stay within the persisted vocabulary.
func ensureEpisodeErrorCodeValid() {
	for _, code := range []string{episodeCodeTimeout, episodeCodeCancelled, episodeCodeSensitive,
		episodeCodeSourceBoundary, episodeCodeInvalidOutput, episodeCodeSourceMissing,
		episodeCodeFailed, episodeCodeIndexFailed} {
		if !domain.ValidEpisodeErrorCode(code) {
			panic("service: episode error code outside the persisted vocabulary: " + code)
		}
	}
}
