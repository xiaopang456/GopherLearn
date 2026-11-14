package conversation

// What the product will do with the configuration a run hands it.
//
// A run that measured the wrong arm is not caught by a clean git tree or by
// a hash of the configuration file: both of those say what the file is, not
// what the product will do with it. The run of 2026-11-14 measured the
// closed arm for exactly that reason - the isolated entry of the checkout
// had no [agentConfig] block, so the summary did not record what the
// assistant produced and the evidence export was off, and nothing said so
// until the log was read.
//
// So the check loads the configuration the way the run does - the same
// file, the same loader, the same overrides the environment would carry -
// and reports what the product will actually do. It touches no model, no
// database and no proxy, so a run can ask the question before its first paid
// call.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ConfigurationReport is what the check found.
type ConfigurationReport struct {
	// Repository is the checkout the configuration was read from.
	Repository string `json:"repository"`
	// ConfigurationPath is the file the product's isolated entry reads.
	ConfigurationPath string `json:"configuration_path"`
	// ConfigurationExists says whether that file is there at all.
	ConfigurationExists bool `json:"configuration_exists"`
	// SummarySegmentMode is the layered summary's own switch.
	SummarySegmentMode string `json:"summary_segment_mode"`
	// SummaryAssistantContent is the switch that says whether the summary
	// records what the assistant produced as well as what the learner said.
	SummaryAssistantContent string `json:"summary_assistant_content"`
	// SummarySegmentDebugDump is the switch that says whether the model's
	// own answer is kept beside the segment it produced - which is what
	// makes the per-batch evidence exist.
	SummarySegmentDebugDump bool `json:"summary_segment_debug_dump"`
	// Expected is what the run needs the configuration to say.
	Expected ExpectedConfiguration `json:"expected"`
	// Matches says whether the configuration says it.
	Matches bool `json:"matches"`
}

// ExpectedConfiguration is what a run needs the configuration to say.
type ExpectedConfiguration struct {
	SummaryAssistantContent string `json:"summary_assistant_content"`
	SummarySegmentDebugDump bool   `json:"summary_segment_debug_dump"`
}

// CheckProductConfiguration loads the configuration of a checkout and reports
// what the product will do with it.
//
// The loader is the product's own isolated entry, so the report is the
// product's own reading of its own file - not a guess about what the file
// means.
func CheckProductConfiguration(repository string) (ConfigurationReport, error) {
	report := ConfigurationReport{Repository: repository}
	path := filepath.Join(repository, "tutorial", "v2-entry", "isolated.example.toml")
	report.ConfigurationPath = path
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			report.ConfigurationExists = false
			return report, fmt.Errorf("the product's isolated entry is missing: %s", path)
		}
		return report, err
	}
	report.ConfigurationExists = true

	storage, err := os.MkdirTemp("", "gl-config-check-")
	if err != nil {
		return report, err
	}
	defer os.RemoveAll(storage)
	settings, err := offlineSettings(repository, storage)
	if err != nil {
		return report, err
	}
	report.SummarySegmentMode = strings.TrimSpace(settings.Config.AgentConfig.SummarySegmentMode)
	report.SummaryAssistantContent = strings.TrimSpace(settings.Config.AgentConfig.SummaryAssistantContent)
	report.SummarySegmentDebugDump = settings.Config.AgentConfig.SummarySegmentDebugDump
	return report, nil
}
