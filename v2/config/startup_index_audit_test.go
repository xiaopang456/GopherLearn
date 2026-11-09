package config

import "testing"

// The startup index audit ships in the reporting mode. The first upgrade of
// a deployment should only show what is inconsistent; a deployment that wants
// the documents rebuilt says so, and then pays for the embeddings.
func TestStartupIndexAuditShipsInAuditMode(t *testing.T) {
	cfg := defaultConfig()
	if cfg.KnowledgeConfig.StartupIndexAudit.Mode != StartupIndexAuditModeAudit {
		t.Fatalf("default startup index audit mode = %q, want %q", cfg.KnowledgeConfig.StartupIndexAudit.Mode, StartupIndexAuditModeAudit)
	}
	if cfg.KnowledgeConfig.StartupIndexAudit.RebuildLimit != DefaultStartupIndexAuditRebuildLimit {
		t.Fatalf("default rebuild limit = %d, want %d", cfg.KnowledgeConfig.StartupIndexAudit.RebuildLimit, DefaultStartupIndexAuditRebuildLimit)
	}
	if err := cfg.KnowledgeConfig.Validate(); err != nil {
		t.Fatal(err)
	}
}

// An empty mode means the default, not "nothing": a deployment that never
// heard of the audit still gets the report, because an index that is thinner
// than the record says is silent.
func TestStartupIndexAuditAcceptsTheThreeModesAndRefusesTheOthers(t *testing.T) {
	for _, mode := range []string{"", StartupIndexAuditModeOff, StartupIndexAuditModeAudit, StartupIndexAuditModeRebuild, " REBUILD "} {
		cfg := defaultConfig()
		cfg.KnowledgeConfig.StartupIndexAudit.Mode = mode
		if err := cfg.KnowledgeConfig.Validate(); err != nil {
			t.Fatalf("mode %q was refused: %v", mode, err)
		}
	}
	for _, mode := range []string{"delete", "rebuild-everything", "true"} {
		cfg := defaultConfig()
		cfg.KnowledgeConfig.StartupIndexAudit.Mode = mode
		if err := cfg.KnowledgeConfig.Validate(); err == nil {
			t.Fatalf("mode %q was accepted", mode)
		}
	}
}

// The rebuild limit is a number of documents, not a number of bytes: a
// negative one cannot be read as "none".
func TestStartupIndexAuditRefusesANegativeRebuildLimit(t *testing.T) {
	cfg := defaultConfig()
	cfg.KnowledgeConfig.StartupIndexAudit.RebuildLimit = -1
	if err := cfg.KnowledgeConfig.Validate(); err == nil {
		t.Fatal("a negative rebuild limit was accepted")
	}
}

// Zero means the default, and the default is the bounded one.
func TestStartupIndexAuditRebuildLimitFallsBackToTheDefault(t *testing.T) {
	cfg := StartupIndexAuditConfig{}
	if got := cfg.StartupIndexAuditRebuildLimit(); got != DefaultStartupIndexAuditRebuildLimit {
		t.Fatalf("rebuild limit = %d, want %d", got, DefaultStartupIndexAuditRebuildLimit)
	}
	if got := (StartupIndexAuditConfig{RebuildLimit: 5}).StartupIndexAuditRebuildLimit(); got != 5 {
		t.Fatalf("rebuild limit = %d, want 5", got)
	}
}
