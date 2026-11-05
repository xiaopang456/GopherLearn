package agent

import (
	"context"
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

// The shadow mode assembles the write path but not the fallback. The
// fallback generates a segment before the answer goes out, which is a change
// to what the learner waits for, and in shadow the context is the old path's
// job.
func TestTheShadowModeWritesWithoutChangingWhatTheModelSees(t *testing.T) {
	settings, err := SummarySegmentAssemblyConfigFromSettings(segmentAssemblySettings("shadow"))
	if err != nil {
		t.Fatal(err)
	}
	assembly, err := NewSummarySegmentAssembly(settings, &stubSegmentRepository{},
		stubSegmentGeneratorWith(t, []model.SessionSummarySegmentPoint{{Text: "p", EvidenceMessageIDs: []uint{1}}}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if assembly == nil || !assembly.Enabled() {
		t.Fatalf("the shadow mode did not assemble a write path: %+v", assembly)
	}
	if assembly.Worker == nil || assembly.Trigger == nil || assembly.Producer == nil {
		t.Fatalf("the shadow mode did not assemble the write side: %+v", assembly)
	}
	if assembly.Fallback != nil {
		t.Fatal("the shadow mode assembled the fallback, which would change what the model sees")
	}
	if assembly.Applies() {
		t.Fatal("the shadow mode reports itself as applied")
	}
}

// The fallback in the shadow mode does nothing rather than fail: the caller
// asks for it on every turn, and in shadow there is no fallback to make.
func TestTheFallbackDoesNothingInTheShadowMode(t *testing.T) {
	settings, err := SummarySegmentAssemblyConfigFromSettings(segmentAssemblySettings("shadow"))
	if err != nil {
		t.Fatal(err)
	}
	assembly, err := NewSummarySegmentAssembly(settings, &stubSegmentRepository{},
		stubSegmentGeneratorWith(t, []model.SessionSummarySegmentPoint{{Text: "p", EvidenceMessageIDs: []uint{1}}}), nil)
	if err != nil {
		t.Fatal(err)
	}
	record, err := assembly.StartSummarySegmentFallback(context.Background(), 1, "s1", 100)
	if err != nil {
		t.Fatal(err)
	}
	if record.FellBack {
		t.Fatalf("the shadow mode ran a fallback: %+v", record)
	}
}

// The applied mode is the only one that reads the new state back.
func TestOnlyTheAppliedModeReadsTheNewStateBack(t *testing.T) {
	for mode, expected := range map[string]bool{"off": false, "shadow": false, "on": true} {
		settings, err := SummarySegmentAssemblyConfigFromSettings(segmentAssemblySettings(mode))
		if err != nil {
			t.Fatal(err)
		}
		assembly, err := NewSummarySegmentAssembly(settings, &stubSegmentRepository{},
			stubSegmentGeneratorWith(t, []model.SessionSummarySegmentPoint{{Text: "p", EvidenceMessageIDs: []uint{1}}}), nil)
		if err != nil {
			t.Fatal(err)
		}
		if assembly != nil && assembly.Applies() != expected {
			t.Fatalf("the %s mode applies = %v, want %v", mode, assembly.Applies(), expected)
		}
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
