package conversation

import (
	"fmt"
	"os"

	"GopherAI/config"

	"github.com/BurntSushi/toml"
)

// scaledAgentConfig is the part of [agentConfig] a scaled run is allowed to set.
//
// Every field is a pointer so a key that is absent keeps the product default and
// a key that is present as zero means zero. Without that distinction a file that
// says nothing about a budget would silently set it to nothing, which is the
// failure this whole configuration exists to make visible.
//
// It is deliberately a closed list, not the whole AgentConfig: a measurement
// that could set any field could also set the ones it is measuring against.
type scaledAgentConfig struct {
	AgentConfig scaledAgentSection `toml:"agentConfig"`
}

type scaledAgentSection struct {
	ContextModelWindowTokens         *int    `toml:"contextModelWindowTokens"`
	ContextInputTargetTokens         *int    `toml:"contextInputTargetTokens"`
	ContextOutputReserveTokens       *int    `toml:"contextOutputReserveTokens"`
	KnowledgeMaxRunTokens            *int    `toml:"knowledgeMaxRunTokens"`
	KnowledgeMaxObservationTokens    *int    `toml:"knowledgeMaxObservationTokens"`
	ContextMaxGlobalUserMemoryTokens *int    `toml:"contextMaxGlobalUserMemoryTokens"`
	ContextMaxTopicUserMemoryTokens  *int    `toml:"contextMaxTopicUserMemoryTokens"`
	ContextMaxSummaryTokens          *int    `toml:"contextMaxSummaryTokens"`
	ContextMaxMasteryTokens          *int    `toml:"contextMaxMasteryTokens"`
	ContextMaxEpisodeTokens          *int    `toml:"contextMaxEpisodeTokens"`
	ContextMaxWhiteboardTokens       *int    `toml:"contextMaxWhiteboardTokens"`
	ContextMaxResidentSegmentTokens  *int    `toml:"contextMaxResidentSegmentTokens"`
	ContextMaxRetrievedSegmentTokens *int    `toml:"contextMaxRetrievedSegmentTokens"`
	ContextMaxSegmentTokens          *int    `toml:"contextMaxSegmentTokens"`
	ContextMinRecentMessages         *int    `toml:"contextMinRecentMessages"`
	ContextHistoryFetchLimit         *int    `toml:"contextHistoryFetchLimit"`
	EpisodeRecallMode                *string `toml:"episodeRecallMode"`
	SummarySegmentMode               *string `toml:"summarySegmentMode"`
}

// applyScaledAgentConfig overlays a scaled run's configuration on the settings.
//
// The settings a run starts from are the product defaults, and they are the
// right start: a run that only wanted to change the mode should not have to
// restate every budget. The overlay says which keys it changes, the rest keep
// their defaults, and the settings are validated afterwards so a configuration
// the product would refuse at startup is refused here too.
func applyScaledAgentConfig(target *config.AgentConfig, path string) error {
	if path == "" {
		return nil
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read scaled configuration: %w", err)
	}
	var overlay scaledAgentConfig
	meta, err := toml.Decode(string(encoded), &overlay)
	if err != nil {
		return fmt.Errorf("parse scaled configuration: %w", err)
	}
	// A misspelled key is a run that measures something other than what its
	// file says, so the undecoded keys are refused rather than ignored.
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		return fmt.Errorf("scaled configuration names %q, which this run does not set", undecoded)
	}

	overlay.AgentConfig.applyTo(target)
	return nil
}

func (o *scaledAgentSection) applyTo(target *config.AgentConfig) {
	set := func(dst *int, value *int) {
		if value != nil {
			*dst = *value
		}
	}
	set(&target.ContextModelWindowTokens, o.ContextModelWindowTokens)
	set(&target.ContextInputTargetTokens, o.ContextInputTargetTokens)
	set(&target.ContextOutputReserveTokens, o.ContextOutputReserveTokens)
	set(&target.KnowledgeMaxRunTokens, o.KnowledgeMaxRunTokens)
	set(&target.KnowledgeMaxObservationTokens, o.KnowledgeMaxObservationTokens)
	set(&target.ContextMaxGlobalUserMemoryTokens, o.ContextMaxGlobalUserMemoryTokens)
	set(&target.ContextMaxTopicUserMemoryTokens, o.ContextMaxTopicUserMemoryTokens)
	set(&target.ContextMaxSummaryTokens, o.ContextMaxSummaryTokens)
	set(&target.ContextMaxMasteryTokens, o.ContextMaxMasteryTokens)
	set(&target.ContextMaxEpisodeTokens, o.ContextMaxEpisodeTokens)
	set(&target.ContextMaxWhiteboardTokens, o.ContextMaxWhiteboardTokens)
	set(&target.ContextMaxResidentSegmentTokens, o.ContextMaxResidentSegmentTokens)
	set(&target.ContextMaxRetrievedSegmentTokens, o.ContextMaxRetrievedSegmentTokens)
	set(&target.ContextMaxSegmentTokens, o.ContextMaxSegmentTokens)
	set(&target.ContextMinRecentMessages, o.ContextMinRecentMessages)
	set(&target.ContextHistoryFetchLimit, o.ContextHistoryFetchLimit)
	if value := o.EpisodeRecallMode; value != nil {
		target.EpisodeRecallMode = *value
	}
	if value := o.SummarySegmentMode; value != nil {
		target.SummarySegmentMode = *value
	}
}
