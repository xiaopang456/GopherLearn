package agent

import (
	"context"
	"fmt"
	"time"

	"GopherAI/config"

	modelcomponent "github.com/cloudwego/eino/components/model"
)

// The assembly of the layered summary's background path.
//
// It is kept apart from the rest of the service so that the parts that read
// the summary can be built without the parts that write it, and the other way
// round. The write path is assembled only when the mode says it should run:
// a segment that is written and never read costs a model call per batch and
// answers nothing.

// SummarySegmentAssembly is the assembled write path.
type SummarySegmentAssembly struct {
	// Worker processes the queue. Nil when the mode is off.
	Worker *SummarySegmentWorker
	// Trigger enqueues a segment when a turn leaves enough unsummarized
	// conversation behind. Nil when the mode is off.
	Trigger *SummarySegmentTrigger
	// Fallback generates a segment before the answer when the raw
	// conversation no longer fits. Nil when the mode is off or shadow.
	Fallback *SummarySegmentFallback
	// Producer is what both the worker and the fallback use.
	Producer *ModelSummarySegmentProducer
	// Mode is the mode this assembly runs in.
	Mode string
}

// Enabled says whether the write path is assembled.
//
// It is false only in the off mode. The shadow mode writes, so it is
// enabled.
func (a *SummarySegmentAssembly) Enabled() bool {
	return a != nil && a.Worker != nil
}

// Applies says whether the new state is read back.
//
// It is true only in the applied mode. The shadow mode writes without being
// read, which is the whole point of it.
func (a *SummarySegmentAssembly) Applies() bool {
	return a != nil && a.Mode == SummarySegmentModeOn
}

// SummarySegmentAssemblyConfig is what the assembly needs from the settings.
type SummarySegmentAssemblyConfig struct {
	// Mode is "off", "shadow" or "on".
	Mode string
	// WorkerConfig is the worker's timing and limits.
	WorkerConfig SummarySegmentWorkerConfig
	// TriggerConfig is the threshold at which a segment is wanted.
	TriggerConfig SummarySegmentTriggerConfig
	// ProducerConfig bounds one attempt.
	ProducerConfig SummarySegmentProducerConfig
	// FallbackConfig bounds the fallback.
	FallbackConfig SummarySegmentFallbackConfig
}

// The layered summary's modes.
const (
	// SummarySegmentModeOff writes nothing.
	SummarySegmentModeOff = "off"
	// SummarySegmentModeShadow writes the segments and the board, but the
	// context still comes from the old path.
	//
	// It is the mode for watching the write side: the segments and the board
	// are produced and stored exactly as they would be in the applied mode,
	// and their success rate, failure reasons, cost and latency can be read
	// off the counters. What the model sees is unchanged, so a shadow run
	// cannot affect a learner.
	SummarySegmentModeShadow = "shadow"
	// SummarySegmentModeOn reads the new state back.
	SummarySegmentModeOn = "on"
)

// EpisodeReadsLegacySummary reports whether the episode projection may read
// the old rolling summary in this mode.
//
// The applied mode may not. The old rolling summary is a frozen picture of an
// earlier part of the session, taken before the layered summary existed: a
// value that was replaced afterwards is still in it, and reading it would let
// a stale statement become an assertion about the learner. The projection
// then reads the raw messages and nothing else.
//
// The other two modes keep reading it. The off mode has no new state at all,
// and the shadow mode is about watching the write side; neither changes what
// the projection sees.
//
// Reading the summary segments instead of the raw messages is a later change.
func EpisodeReadsLegacySummary(mode string) bool {
	return mode != SummarySegmentModeOn
}

// SummarySegmentAssemblyConfigFromSettings reads the assembly's settings.
//
// The mode is the one thing that decides whether the write path exists at
// all. Everything else is read and validated even when the mode is off, so
// that a deployment which turns the mode on does not fail on a setting that
// was never checked.
func SummarySegmentAssemblyConfigFromSettings(conf config.AgentConfig) (SummarySegmentAssemblyConfig, error) {
	fallbackBudget := time.Duration(conf.SummarySegmentFallbackBudgetSeconds) * time.Second
	fallbackLease := time.Duration(conf.SummarySegmentLeaseSeconds) * time.Second
	if fallbackLease <= fallbackBudget {
		// The fallback holds the session while it generates. Its lease has to
		// outlast its own budget, or the background worker would take the
		// session back while the fallback is still working on it.
		fallbackLease = fallbackBudget + 5*time.Second
	}
	assembly := SummarySegmentAssemblyConfig{
		Mode: conf.SummarySegmentMode,
		WorkerConfig: SummarySegmentWorkerConfig{
			WorkerCount:       conf.SummarySegmentWorkerCount,
			QueueSize:         conf.SummarySegmentQueueSize,
			ScanInterval:      time.Duration(conf.SummarySegmentScanIntervalSeconds) * time.Second,
			ReconcileInterval: time.Duration(conf.SummarySegmentReconcileIntervalSeconds) * time.Second,
			RecoveryInterval:  time.Duration(conf.SummarySegmentRecoveryIntervalSeconds) * time.Second,
			TaskTimeout:       time.Duration(conf.SummarySegmentTaskTimeoutSeconds) * time.Second,
			Lease:             time.Duration(conf.SummarySegmentLeaseSeconds) * time.Second,
			RetryDelay:        time.Duration(conf.SummarySegmentRetryDelaySeconds) * time.Second,
			MaxAttempts:       conf.SummarySegmentMaxAttempts,
		},
		TriggerConfig: SummarySegmentTriggerConfig{
			TriggerMessages: conf.SummarySegmentTriggerMessages,
			TriggerTokens:   conf.SummarySegmentTriggerTokens,
			FetchLimit:      conf.SummarySegmentFetchLimit,
		},
		ProducerConfig: SummarySegmentProducerConfig{
			MaxMessages:     conf.SummarySegmentMaxMessages,
			MaxInputTokens:  conf.SummarySegmentMaxInputTokens,
			MaxOutputTokens: conf.SummarySegmentMaxOutputTokens,
			FetchLimit:      conf.SummarySegmentFetchLimit,
		},
		FallbackConfig: SummarySegmentFallbackConfig{
			RawMessageTokenLimit: conf.SummarySegmentFallbackTokens,
			StageBudget:          fallbackBudget,
			Lease:                fallbackLease,
			MaxAttempts:          conf.SummarySegmentMaxAttempts,
		},
	}
	if err := assembly.WorkerConfig.Validate(); err != nil {
		return SummarySegmentAssemblyConfig{}, err
	}
	if err := assembly.TriggerConfig.Validate(); err != nil {
		return SummarySegmentAssemblyConfig{}, err
	}
	if err := assembly.ProducerConfig.Validate(); err != nil {
		return SummarySegmentAssemblyConfig{}, err
	}
	if err := assembly.FallbackConfig.Validate(); err != nil {
		return SummarySegmentAssemblyConfig{}, err
	}
	switch assembly.Mode {
	case SummarySegmentModeOff, SummarySegmentModeShadow, SummarySegmentModeOn:
	default:
		return SummarySegmentAssemblyConfig{}, fmt.Errorf("summary segment mode must be off, shadow or on")
	}
	return assembly, nil
}

// NewSummarySegmentAssembly builds the write path.
//
// It returns nil when the mode is off, so the caller does not have to decide
// whether the path exists: a nil assembly is the off state.
//
// The shadow mode assembles the worker and the trigger but not the fallback.
// The fallback generates a segment before the answer goes out, which is a
// change to what the learner waits for; in shadow the context is the old
// path's job, so the fallback has nothing to do.
func NewSummarySegmentAssembly(
	settings SummarySegmentAssemblyConfig,
	repository SummarySegmentRepository,
	generator SegmentGenerator,
	chatModel modelcomponent.ToolCallingChatModel,
) (*SummarySegmentAssembly, error) {
	if settings.Mode == SummarySegmentModeOff {
		return nil, nil
	}
	if repository == nil {
		return nil, fmt.Errorf("summary segment repository is required")
	}
	segmentGenerator := generator
	if segmentGenerator == nil {
		if chatModel == nil {
			return nil, fmt.Errorf("summary segment generator or chat model is required")
		}
		built, err := NewModelSegmentGenerator(chatModel, settings.ProducerConfig.MaxInputTokens, settings.ProducerConfig.MaxOutputTokens)
		if err != nil {
			return nil, err
		}
		segmentGenerator = built
	}
	producer, err := NewModelSummarySegmentProducer(repository, repository, segmentGenerator, settings.ProducerConfig)
	if err != nil {
		return nil, err
	}
	reconciler, err := NewSummarySegmentReconciler(repository, settings.WorkerConfig.QueueSize)
	if err != nil {
		return nil, err
	}
	worker, err := NewSummarySegmentWorker(repository, producer, reconciler, settings.WorkerConfig)
	if err != nil {
		return nil, err
	}
	trigger, err := NewSummarySegmentTrigger(repository, settings.TriggerConfig)
	if err != nil {
		return nil, err
	}
	assembly := &SummarySegmentAssembly{
		Worker:   worker,
		Trigger:  trigger,
		Producer: producer,
		Mode:     settings.Mode,
	}
	if settings.Mode == SummarySegmentModeOn {
		fallback, err := NewSummarySegmentFallback(repository, producer, settings.FallbackConfig)
		if err != nil {
			return nil, err
		}
		assembly.Fallback = fallback
	}
	return assembly, nil
}

// Enqueuer returns the part of the write path the service calls after an
// answer, or nil when the write path is off.
//
// It is a method rather than a field so that a caller can ask an assembly
// that does not exist: the off mode has no assembly at all, and a field
// access on it would panic.
func (a *SummarySegmentAssembly) Enqueuer() SummarySegmentEnqueuer {
	if a == nil || a.Trigger == nil {
		return nil
	}
	return a.Trigger
}

// SummarySegmentRepository is the storage the write path needs.
//
// The read and the write sides are one interface because they are one
// transaction's worth of state: the board that is read is the board that is
// compared against the version being written.
type SummarySegmentRepository interface {
	SummarySegmentSource
	SummarySegmentSink
	SummarySegmentJobRepository
	SummarySegmentReconcileRepository
	SummarySegmentTriggerRepository
	SummarySegmentFallbackRepository
}

// StartSummarySegmentFallback generates a segment before the answer when the
// raw conversation no longer fits.
//
// It is a no-op when the write path is off: a deployment that has not turned
// the mode on keeps the rolling summary's behaviour, and a fallback that ran
// anyway would write segments nothing reads.
func (a *SummarySegmentAssembly) StartSummarySegmentFallback(ctx context.Context, userID int64, sessionID string, latestMessageID uint) (SummarySegmentFallbackRecord, error) {
	if a == nil || a.Fallback == nil {
		return SummarySegmentFallbackRecord{}, nil
	}
	return a.Fallback.Decide(ctx, userID, sessionID, latestMessageID)
}

// ConsiderSummarySegment enqueues a segment when the conversation grew past
// what the raw messages can carry.
//
// It is a no-op when the write path is off.
func (a *SummarySegmentAssembly) ConsiderSummarySegment(ctx context.Context, userID int64, sessionID string, latestMessageID uint) (TriggerOutcome, error) {
	if a == nil || a.Trigger == nil {
		return TriggerOutcome{}, nil
	}
	return a.Trigger.Consider(ctx, userID, sessionID, latestMessageID)
}

// Start starts the worker.
func (a *SummarySegmentAssembly) Start(ctx context.Context) error {
	if a == nil || a.Worker == nil {
		return nil
	}
	return a.Worker.Start(ctx)
}

// Stop stops the worker.
func (a *SummarySegmentAssembly) Stop(ctx context.Context) error {
	if a == nil || a.Worker == nil {
		return nil
	}
	return a.Worker.Stop(ctx)
}
