package agent

import (
	"testing"

	"GopherAI/config"
	"GopherAI/model"
)

// The write path is assembled only when the mode says it should run. A
// segment that is written and never read costs a model call per batch and
// answers nothing, so the off state has to be the shipped default.

func segmentAssemblySettings(mode string) config.AgentConfig {
	conf := config.AgentConfig{}
	conf.SummarySegmentMode = mode
	conf.SummarySegmentWorkerCount = 1
	conf.SummarySegmentQueueSize = 32
	conf.SummarySegmentScanIntervalSeconds = 3
	conf.SummarySegmentReconcileIntervalSeconds = 60
	conf.SummarySegmentTaskTimeoutSeconds = 30
	conf.SummarySegmentLeaseSeconds = 60
	conf.SummarySegmentRecoveryIntervalSeconds = 30
	conf.SummarySegmentRetryDelaySeconds = 15
	conf.SummarySegmentMaxAttempts = 3
	conf.SummarySegmentTriggerMessages = 10
	conf.SummarySegmentTriggerTokens = 6000
	conf.SummarySegmentMaxMessages = 64
	conf.SummarySegmentMaxInputTokens = 8000
	conf.SummarySegmentMaxOutputTokens = 2000
	conf.SummarySegmentFetchLimit = 256
	conf.SummarySegmentFallbackTokens = 6000
	conf.SummarySegmentFallbackBudgetSeconds = 8
	return conf
}

// The off mode assembles nothing, and the methods a caller may still call
// do nothing rather than fail.
func TestTheOffModeAssemblesNothing(t *testing.T) {
	settings, err := SummarySegmentAssemblyConfigFromSettings(segmentAssemblySettings("off"))
	if err != nil {
		t.Fatal(err)
	}
	assembly, err := NewSummarySegmentAssembly(settings, &stubSegmentRepository{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if assembly != nil {
		t.Fatalf("the off mode assembled a write path: %+v", assembly)
	}
	if assembly.Enabled() {
		t.Fatal("the off mode reports itself as enabled")
	}
}

// A mode that is not one of the two is refused rather than guessed at.
func TestAnUnknownModeIsRefused(t *testing.T) {
	if _, err := SummarySegmentAssemblyConfigFromSettings(segmentAssemblySettings("sometimes")); err == nil {
		t.Fatal("an unknown mode must be refused")
	}
}

// The settings are checked even when the mode is off, so that turning the
// mode on does not fail on a setting that was never looked at.
func TestTheSettingsAreCheckedEvenWhenTheModeIsOff(t *testing.T) {
	conf := segmentAssemblySettings("off")
	conf.SummarySegmentLeaseSeconds = 10
	conf.SummarySegmentTaskTimeoutSeconds = 30
	if _, err := SummarySegmentAssemblyConfigFromSettings(conf); err == nil {
		t.Fatal("a lease shorter than the task timeout must be refused")
	}
}

// The on mode assembles the whole write path, and the fallback and the
// trigger are usable.
func TestTheOnModeAssemblesTheWholeWritePath(t *testing.T) {
	settings, err := SummarySegmentAssemblyConfigFromSettings(segmentAssemblySettings("on"))
	if err != nil {
		t.Fatal(err)
	}
	assembly, err := NewSummarySegmentAssembly(settings, &stubSegmentRepository{},
		stubSegmentGeneratorWith(t, []model.SessionSummarySegmentPoint{{Text: "p", EvidenceMessageIDs: []uint{1}}}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if assembly == nil || !assembly.Enabled() {
		t.Fatalf("the on mode did not assemble a write path: %+v", assembly)
	}
	if assembly.Worker == nil || assembly.Trigger == nil || assembly.Fallback == nil || assembly.Producer == nil {
		t.Fatalf("the assembly is incomplete: %+v", assembly)
	}
}
