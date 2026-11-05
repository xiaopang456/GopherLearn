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
	// conversation no longer fits. Nil when the mode is off.
	Fallback *SummarySegmentFallback
	// Producer is what both the worker and the fallback use.
	Producer *ModelSummarySegmentProducer
}

// Enabled says whether the write path is assembled.
func (a *SummarySegmentAssembly) Enabled() bool {
	return a != nil && a.Worker != nil
}

// SummarySegmentAssemblyConfig is what the assembly needs from the settings.
type SummarySegmentAssemblyConfig struct {
	// Mode is "off" or "on".
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

// SummarySegmentAssemblyConfigFromSettings reads the assembly's settings.
//
// The mode is the one thing that decides whether the write path exists at
// all. Everything else is read and validated even when the mode is off, so
// that a deployment which turns the mode on does not fail on a setting that
// was never checked.
func SummarySegmentAssemblyConfigFromSettings(conf config.AgentConfig) (SummarySegmentAssemblyConfig, error) {
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
			StageBudget:          time.Duration(conf.SummarySegmentFallbackBudgetSeconds) * time.Second,
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
	case "off", "on":
	default:
		return SummarySegmentAssemblyConfig{}, fmt.Errorf("summary segment mode must be off or on")
	}
	return assembly, nil
}

// NewSummarySegmentAssembly builds the write path.
//
// It returns nil when the mode is off, so the caller does not have to decide
// whether the path exists: a nil assembly is the off state.
func NewSummarySegmentAssembly(
	settings SummarySegmentAssemblyConfig,
	repository SummarySegmentRepository,
	generator SegmentGenerator,
	chatModel modelcomponent.ToolCallingChatModel,
) (*SummarySegmentAssembly, error) {
	if settings.Mode == "off" {
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
	fallback, err := NewSummarySegmentFallback(repository, producer, settings.FallbackConfig)
	if err != nil {
		return nil, err
	}
	return &SummarySegmentAssembly{
		Worker:   worker,
		Trigger:  trigger,
		Fallback: fallback,
		Producer: producer,
	}, nil
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
