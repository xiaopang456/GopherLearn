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
// file, the same loader, the same overlay, the same validation - and
// reports what the product will actually do. It touches no model, no
// database and no proxy, so a run can ask the question before its first
// paid call.
//
// The overlay matters as much as the entry file. A scaled run hands the
// product a second file, and that file can change the mode and every
// budget: the entry file alone is the configuration of a run that never
// happens. So the check takes the overlay the run uses, applies it in the
// run's order, and reports the configuration that is left.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ConfigurationCheck is what the check needs to know before it reads anything.
type ConfigurationCheck struct {
	// Repository is the checkout the configuration is read from.
	Repository string
	// ScaledConfig is the overlay the run hands the product. It is the same
	// file the run passes to -scaled-config; an empty string means the run
	// uses the product's defaults.
	ScaledConfig string
	// ChatModel is the model name the run would use. The check never calls a
	// model; the name is here because the product's own loader takes one.
	ChatModel string
}

// ConfigurationReport is what the check found.
type ConfigurationReport struct {
	// Repository is the checkout the configuration was read from.
	Repository string `json:"repository"`
	// ConfigurationPath is the file the product's isolated entry reads.
	ConfigurationPath string `json:"configuration_path"`
	// ConfigurationExists says whether that file is there at all.
	ConfigurationExists bool `json:"configuration_exists"`
	// ScaledConfigPath is the overlay the run hands the product.
	ScaledConfigPath string `json:"scaled_config_path,omitempty"`
	// ScaledConfigExists says whether that overlay is there.
	ScaledConfigExists bool `json:"scaled_config_exists"`
	// SummarySegmentMode is the layered summary's own switch, after the
	// overlay has been applied.
	SummarySegmentMode string `json:"summary_segment_mode"`
	// SummaryAssistantContent is the switch that says whether the summary
	// records what the assistant produced as well as what the learner said.
	SummaryAssistantContent string `json:"summary_assistant_content"`
	// SummarySegmentDebugDump is the switch that says whether the model's
	// own answer is kept beside the segment it produced - which is what
	// makes the per-batch evidence exist.
	SummarySegmentDebugDump bool `json:"summary_segment_debug_dump"`
	// Budgets are the budgets the window will really render with.
	Budgets EffectiveBudgets `json:"effective_budgets"`
	// Expected is what the run needs the configuration to say.
	Expected ExpectedConfiguration `json:"expected"`
	// Matches says whether the configuration says it.
	Matches bool `json:"matches"`
}

// EffectiveBudgets are the budgets the window will render with.
//
// They are the effective ones, not the configured ones: the product raises
// a segment's size and the segment blocks' budgets when the summary records
// the assistant's products, so the numbers the configuration holds are not
// the numbers the window gets. A report that listed the configured numbers
// would say what the file says and not what the product will do.
type EffectiveBudgets struct {
	// WhiteboardTokens is what the board block may cost.
	WhiteboardTokens int `json:"whiteboard_tokens"`
	// ResidentSegmentTokens is what the resident segment block may cost.
	ResidentSegmentTokens int `json:"resident_segment_tokens"`
	// RetrievedSegmentTokens is what the retrieved segment block may cost.
	RetrievedSegmentTokens int `json:"retrieved_segment_tokens"`
	// SegmentTokens is what one segment may cost.
	SegmentTokens int `json:"segment_tokens"`
	// InputTargetTokens is what the window may carry.
	InputTargetTokens int `json:"input_target_tokens"`
	// ModelWindowTokens is the model's own window.
	ModelWindowTokens int `json:"model_window_tokens"`
}

// ExpectedConfiguration is what a run needs the configuration to say.
type ExpectedConfiguration struct {
	// SummarySegmentMode is the layered summary's mode the run needs.
	SummarySegmentMode string `json:"summary_segment_mode"`
	// SummaryAssistantContent is the assistant-content switch the run needs.
	SummaryAssistantContent string `json:"summary_assistant_content"`
	// SummarySegmentDebugDump is the debug-dump switch the run needs.
	SummarySegmentDebugDump bool `json:"summary_segment_debug_dump"`
}

// CheckProductConfiguration loads the configuration of a checkout and reports
// what the product will do with it.
//
// The load order is the run's: the isolated entry first, then the
// environment the way the loader reads it, then the scaled overlay, and the
// product's own validation last. The check therefore reports the
// configuration the run would really measure with - the mode after the
// overlay, and the budgets after the product's own rules - and a
// configuration the product would refuse at startup is refused here too.
func CheckProductConfiguration(check ConfigurationCheck) (ConfigurationReport, error) {
	report := ConfigurationReport{Repository: check.Repository}
	entryPath := filepath.Join(check.Repository, "tutorial", "v2-entry", "isolated.example.toml")
	report.ConfigurationPath = entryPath
	if _, err := os.Stat(entryPath); err != nil {
		if os.IsNotExist(err) {
			report.ConfigurationExists = false
			return report, fmt.Errorf("the product's isolated entry is missing: %s", entryPath)
		}
		return report, err
	}
	report.ConfigurationExists = true

	if overlay := strings.TrimSpace(check.ScaledConfig); overlay != "" {
		report.ScaledConfigPath = overlay
		if _, err := os.Stat(overlay); err != nil {
			if os.IsNotExist(err) {
				report.ScaledConfigExists = false
				return report, fmt.Errorf("the scaled configuration is missing: %s", overlay)
			}
			return report, err
		}
		report.ScaledConfigExists = true
	}

	storage, err := os.MkdirTemp("", "gl-config-check-")
	if err != nil {
		return report, err
	}
	defer os.RemoveAll(storage)
	chatModel := strings.TrimSpace(check.ChatModel)
	if chatModel == "" {
		chatModel = "config-check-no-model"
	}
	// The run's own loader, in the run's own order: the isolated entry, the
	// environment, the overlay, and the product's validation.
	settings, err := summarySettings(check.Repository, storage, chatModel, strings.TrimSpace(check.ScaledConfig))
	if err != nil {
		return report, err
	}
	agent := settings.Config.AgentConfig
	report.SummarySegmentMode = strings.TrimSpace(agent.SummarySegmentMode)
	report.SummaryAssistantContent = strings.TrimSpace(agent.SummaryAssistantContent)
	report.SummarySegmentDebugDump = agent.SummarySegmentDebugDump
	report.Budgets = EffectiveBudgets{
		WhiteboardTokens:       agent.ContextMaxWhiteboardTokens,
		ResidentSegmentTokens:  agent.EffectiveResidentSegmentTokens(),
		RetrievedSegmentTokens: agent.EffectiveRetrievedSegmentTokens(),
		SegmentTokens:          agent.EffectiveSegmentTokens(),
		InputTargetTokens:      agent.ContextInputTargetTokens,
		ModelWindowTokens:      agent.ContextModelWindowTokens,
	}
	return report, nil
}

// MatchesExpected says whether the configuration says what the run needs.
//
// All three switches are checked together: a run that needs the layered
// summary on, the assistant's products recorded and the evidence exported
// is refused by any one of them being different, because each of them is
// the difference between a measurement and a different measurement.
func (r ConfigurationReport) MatchesExpected(expected ExpectedConfiguration) bool {
	return r.SummarySegmentMode == expected.SummarySegmentMode &&
		r.SummaryAssistantContent == expected.SummaryAssistantContent &&
		r.SummarySegmentDebugDump == expected.SummarySegmentDebugDump
}
