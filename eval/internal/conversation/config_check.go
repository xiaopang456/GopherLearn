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
	// SessionCardMode is the cross-session memory path the run asks for:
	// "disabled" is the old Episode, "enabled" is the session card.
	//
	// The check reports what the product will do with it, so that a run that
	// asked for one path and booted with the other is caught before the
	// first paid call rather than in the results.
	SessionCardMode string
	// SessionCardIndexName and SessionCardIndexKeyPrefix are the namespace
	// the card arm writes into. They are checked as well: two arms that
	// wrote into one index would read each other's rows.
	SessionCardIndexName      string
	SessionCardIndexKeyPrefix string
	// MemoryReadMode is the product's read path the run asks for, and
	// EpisodeRecallMode is its Episode recall switch. They are checked for
	// the same reason the card mode is: a run that booted with a different
	// read path than it meant to measure produces results about something
	// else, and the results would look like results.
	MemoryReadMode    string
	EpisodeRecallMode string
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
	// SessionCardMode is the cross-session memory path the product will run.
	SessionCardMode string `json:"session_card_mode"`
	// SessionCardIndexName and SessionCardIndexPrefix are the namespace
	// the card arm will write into.
	//
	// The prefix is reported under a name that does not say "key": the
	// report is printed where the run's output is, and a field named after a
	// secret is a place a secret could hide.
	SessionCardIndexName   string `json:"session_card_index_name,omitempty"`
	SessionCardIndexPrefix string `json:"session_card_index_prefix,omitempty"`
	// MemoryReadMode and EpisodeRecallMode are the read path and the recall
	// switch the product will run with.
	MemoryReadMode    string `json:"memory_read_mode,omitempty"`
	EpisodeRecallMode string `json:"episode_recall_mode,omitempty"`
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
	// SessionCardMode is the cross-session memory path the run needs. It is
	// empty when the run does not say, and then it is not compared: a run
	// that measures the old path must not fail over a field it never set.
	SessionCardMode string `json:"session_card_mode,omitempty"`
	// SessionCardIndexName is the namespace the run needs the card arm to
	// write into.
	SessionCardIndexName string `json:"session_card_index_name,omitempty"`
	// SessionCardIndexKeyPrefix is the key prefix of that namespace.
	//
	// The index name and the key prefix are two halves of one namespace, and
	// the prefix is the half that decides which rows a search can see: two
	// arms that agreed on the index name but not on the prefix would write
	// into one index and read each other's rows. So a check that compares the
	// name and ignores the prefix says the namespace matches when it does not.
	SessionCardIndexKeyPrefix string `json:"session_card_index_key_prefix,omitempty"`
	// MemoryReadMode is the read path the run needs. It is empty when the run
	// does not say, and then it is not compared.
	MemoryReadMode string `json:"memory_read_mode,omitempty"`
	// EpisodeRecallMode is the Episode recall switch the run needs. It is
	// empty when the run does not say, and then it is not compared.
	EpisodeRecallMode string `json:"episode_recall_mode,omitempty"`
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
	// The card arm is applied the way the run applies it, so that the report
	// says what the product will do rather than what the file holds.
	arm := &CardArm{Mode: strings.TrimSpace(check.SessionCardMode), IndexName: check.SessionCardIndexName,
		KeyPrefix: check.SessionCardIndexKeyPrefix, MemoryReadMode: check.MemoryReadMode,
		EpisodeRecallMode: check.EpisodeRecallMode}
	if arm.Mode != "" {
		if err = ApplyCardArm(settings, arm); err != nil {
			return report, err
		}
		if err = settings.Validate(); err != nil {
			return report, err
		}
	}
	agent := settings.Config.AgentConfig
	report.SummarySegmentMode = strings.TrimSpace(agent.SummarySegmentMode)
	report.SummaryAssistantContent = strings.TrimSpace(agent.SummaryAssistantContent)
	report.SummarySegmentDebugDump = agent.SummarySegmentDebugDump
	report.SessionCardMode = strings.TrimSpace(agent.SessionCardMode)
	report.SessionCardIndexName = strings.TrimSpace(agent.SessionCardIndexName)
	report.SessionCardIndexPrefix = strings.TrimSpace(agent.SessionCardIndexKeyPrefix)
	report.MemoryReadMode = strings.TrimSpace(agent.MemoryReadMode)
	report.EpisodeRecallMode = strings.TrimSpace(agent.EpisodeRecallMode)
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
	if r.SummarySegmentMode != expected.SummarySegmentMode ||
		r.SummaryAssistantContent != expected.SummaryAssistantContent ||
		r.SummarySegmentDebugDump != expected.SummarySegmentDebugDump {
		return false
	}
	// The card fields are compared only when the run named them. A run that
	// measures the old path does not set them, and a run that never named a
	// namespace must not fail over one.
	if expected.SessionCardMode != "" && r.SessionCardMode != expected.SessionCardMode {
		return false
	}
	if expected.SessionCardIndexName != "" && r.SessionCardIndexName != expected.SessionCardIndexName {
		return false
	}
	// The key prefix is compared with the index name, for the same reason:
	// it is the other half of the namespace the card arm writes into.
	if expected.SessionCardIndexKeyPrefix != "" && r.SessionCardIndexPrefix != expected.SessionCardIndexKeyPrefix {
		return false
	}
	// The read path and the recall switch are compared only when the run
	// named them, for the same reason the card fields are.
	if expected.MemoryReadMode != "" && r.MemoryReadMode != expected.MemoryReadMode {
		return false
	}
	if expected.EpisodeRecallMode != "" && r.EpisodeRecallMode != expected.EpisodeRecallMode {
		return false
	}
	return true
}
