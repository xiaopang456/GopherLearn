package contextwindow

import (
	"errors"
	"fmt"
)

// The declared capacity of a model is a deployment premise, not a fact this process
// can observe. These numbers are the budget the process holds itself to: a request
// this package admits is inside the declared window as far as a local estimate can
// tell, and no estimate here proves anything about a provider's real tokenizer,
// pricing, or maximum input.
const (
	DefaultWindowTokens        = 32768
	DefaultInputTargetTokens   = 28000
	DefaultOutputReserveTokens = 4000

	// These are the v2 production layer ceilings. They are explicit fields in
	// Config so a production receipt can distinguish them from experimental
	// helper defaults and so a missing provider does not silently delete a layer.
	DefaultGlobalMemoryMaxTokens = 800
	DefaultTopicMemoryMaxTokens  = 1200
	DefaultSummaryMaxTokens      = 2000
	DefaultMasteryMaxTokens      = 900
	DefaultEpisodeMaxTokens      = 1000
	DefaultHistoryMaxTokens      = 0
	DefaultHistoryFetchLimit     = 128
	DefaultEvidenceMaxTokens     = 6500

	// DefaultHistoryMaxMessages and DefaultHistoryMaxRunes are the two limits this
	// project already enforced on a recent window. They stay part of the budget rather
	// than being replaced by the token cap, and all three are applied together in the
	// one place that chooses history. A token cap alone would widen the door: six
	// thousand estimated tokens is about twenty-four thousand runes of Latin-script
	// text, so an agent turn made of long English answers would carry several times
	// what the same session carried before the migration.
	DefaultHistoryMaxMessages = 12
	DefaultHistoryMaxRunes    = 6000
)

// Ranges a resolved configuration must fall inside. They are wide enough for a real
// deployment to choose its own window and tight enough that a mistyped environment
// value stops startup instead of turning one question into an enormous bill.
const (
	MinWindowTokens        = 8192
	MaxWindowTokens        = 1048576
	MinInputTargetTokens   = 2048
	MinOutputReserveTokens = 256
	MinSummaryMaxTokens    = 128
	MaxSummaryMaxTokens    = 8000
	MinHistoryMaxTokens    = 256
	MaxHistoryMaxTokens    = 32000
	MinEvidenceMaxTokens   = 128
	MinHistoryFetchLimit   = 2
	MaxHistoryFetchLimit   = 512
	MinHistoryMaxMessages  = 2
	MaxHistoryMaxMessages  = 64
	MinHistoryMaxRunes     = 128
	MaxHistoryMaxRunes     = 200000

	// MinRequiredHeadroomTokens is what must stay unclaimed by the summary and the
	// verbatim exchanges after the evidence allowance is held back, because the system
	// prompt, the tool offers and the learner's own question are not optional and are
	// not capped.
	MinRequiredHeadroomTokens = 1024
)

var (
	// ErrWindowOutOfRange reports a declared capacity outside the supported range.
	ErrWindowOutOfRange = errors.New("contextwindow: declared window is out of range")
	// ErrInputTargetOutOfRange reports an input target below the smallest usable one.
	ErrInputTargetOutOfRange = errors.New("contextwindow: input target is out of range")
	// ErrOutputReserveOutOfRange reports an output reserve below the smallest usable one.
	ErrOutputReserveOutOfRange = errors.New("contextwindow: output reserve is out of range")
	// ErrTargetExceedsWindow reports the input target plus the output reserve not
	// fitting inside the declared window: the two halves of a completion cannot both
	// claim more than the model is declared to hold.
	ErrTargetExceedsWindow = errors.New("contextwindow: input target plus output reserve exceeds the declared window")
	// ErrSummaryCapOutOfRange reports an injected-summary cap outside its range.
	ErrSummaryCapOutOfRange = errors.New("contextwindow: summary cap is out of range")
	// ErrHistoryCapOutOfRange reports a recent-history cap outside its range.
	ErrHistoryCapOutOfRange = errors.New("contextwindow: history cap is out of range")
	// ErrHistoryCountOutOfRange reports a recent-history message ceiling outside its
	// range.
	ErrHistoryCountOutOfRange = errors.New("contextwindow: history message ceiling is out of range")
	// ErrHistoryRunesOutOfRange reports a recent-history rune ceiling outside its range.
	ErrHistoryRunesOutOfRange = errors.New("contextwindow: history rune ceiling is out of range")
	// ErrHistoryFetchLimitOutOfRange reports a candidate read limit that cannot cover
	// a complete exchange or is larger than one turn may read.
	ErrHistoryFetchLimitOutOfRange = errors.New("contextwindow: history fetch limit is out of range")
	// ErrEvidenceCapOutOfRange reports a run-level evidence cap outside its range.
	ErrEvidenceCapOutOfRange = errors.New("contextwindow: evidence cap is out of range")
	// ErrSubBudgetsExceedTarget reports the optional caps claiming everything the input
	// target holds, which would leave nothing for a system prompt or a question and
	// make the headroom rule unenforceable no matter what a turn contains.
	ErrSubBudgetsExceedTarget = errors.New("contextwindow: the per-block caps leave no headroom inside the input target")
	// ErrOutputCapExceedsReserve reports a provider output limit larger than the
	// reserve set aside for it. The reserve is the only reason the input target is
	// smaller than the window, so an override that ignores it cancels the check.
	ErrOutputCapExceedsReserve = errors.New("contextwindow: the configured output limit exceeds the output reserve")
)

// Config is one deployment's window budget.
//
// A zero field means "not configured" and resolves to its default; it never means
// "send nothing" or "unlimited". A negative or out-of-range field is a configuration
// fault and is refused by name, because a silently corrected budget is a number that
// the startup log then reports as if the operator had chosen it.
type Config struct {
	// WindowTokens declares the context capacity this deployment holds itself to.
	// It is used to check the other figures add up and to refuse a request whose
	// estimated input plus its output limit would not fit; it says nothing about
	// what the model behind the endpoint really accepts.
	WindowTokens int

	// InputTargetTokens is the ceiling on the estimated input of one request.
	InputTargetTokens int

	// OutputReserveTokens is what the window keeps for the reply.
	OutputReserveTokens int

	// SummaryMaxTokens bounds the summary block injected into a turn.
	SummaryMaxTokens int
	// GlobalMemoryMaxTokens, TopicMemoryMaxTokens, MasteryMaxTokens and
	// EpisodeMaxTokens are separate v2 production layer ceilings. Providers may
	// leave a layer empty, but the budget still names the layer rather than
	// silently folding it into system or history.
	GlobalMemoryMaxTokens int
	TopicMemoryMaxTokens  int
	MasteryMaxTokens      int
	EpisodeMaxTokens      int

	// HistoryMaxTokens bounds the verbatim recent exchanges of a turn.
	HistoryMaxTokens int
	// HistoryMaxMessages and HistoryMaxRunes are optional compatibility caps.
	// Zero means the v2 production path does not impose that clean-only cap.
	HistoryMaxMessages int
	HistoryMaxRunes    int

	// HistoryFetchLimit is how many stored messages are read as candidates before
	// the budget picks among them. It bounds the read, not the send.
	HistoryFetchLimit int

	// EvidenceMaxTokens is the whole turn's shared allowance for tool evidence
	// bodies: one budget across every tool call of the turn, not one budget per
	// tool. A per-tool cap alone lets a model that calls three tools send three
	// caps' worth of retrieved text.
	EvidenceMaxTokens int
}

// DefaultConfig is the budget a deployment runs with when it configures nothing.
func DefaultConfig() Config {
	return Config{
		WindowTokens:          DefaultWindowTokens,
		InputTargetTokens:     DefaultInputTargetTokens,
		OutputReserveTokens:   DefaultOutputReserveTokens,
		SummaryMaxTokens:      DefaultSummaryMaxTokens,
		GlobalMemoryMaxTokens: DefaultGlobalMemoryMaxTokens,
		TopicMemoryMaxTokens:  DefaultTopicMemoryMaxTokens,
		MasteryMaxTokens:      DefaultMasteryMaxTokens,
		EpisodeMaxTokens:      DefaultEpisodeMaxTokens,
		HistoryMaxTokens:      DefaultHistoryMaxTokens,
		HistoryMaxMessages:    0,
		HistoryMaxRunes:       0,
		HistoryFetchLimit:     DefaultHistoryFetchLimit,
		EvidenceMaxTokens:     DefaultEvidenceMaxTokens,
	}
}

// Resolve fills unset fields and refuses illegal ones. It is the only entry a
// deployment should call, so a configuration that reaches the assembly point has
// been both completed and checked.
func (c Config) Resolve() (Config, error) {
	defaults := DefaultConfig()
	if c.WindowTokens == 0 {
		c.WindowTokens = defaults.WindowTokens
	}
	if c.InputTargetTokens == 0 {
		c.InputTargetTokens = defaults.InputTargetTokens
	}
	if c.OutputReserveTokens == 0 {
		c.OutputReserveTokens = defaults.OutputReserveTokens
	}
	if c.SummaryMaxTokens == 0 {
		c.SummaryMaxTokens = defaults.SummaryMaxTokens
	}
	if c.GlobalMemoryMaxTokens == 0 {
		c.GlobalMemoryMaxTokens = defaults.GlobalMemoryMaxTokens
	}
	if c.TopicMemoryMaxTokens == 0 {
		c.TopicMemoryMaxTokens = defaults.TopicMemoryMaxTokens
	}
	if c.MasteryMaxTokens == 0 {
		c.MasteryMaxTokens = defaults.MasteryMaxTokens
	}
	if c.EpisodeMaxTokens == 0 {
		c.EpisodeMaxTokens = defaults.EpisodeMaxTokens
	}
	if c.HistoryMaxTokens == 0 {
		c.HistoryMaxTokens = defaults.HistoryMaxTokens
	}
	if c.HistoryMaxMessages == 0 {
		c.HistoryMaxMessages = defaults.HistoryMaxMessages
	}
	if c.HistoryMaxRunes == 0 {
		c.HistoryMaxRunes = defaults.HistoryMaxRunes
	}
	if c.HistoryFetchLimit == 0 {
		c.HistoryFetchLimit = defaults.HistoryFetchLimit
	}
	if c.EvidenceMaxTokens == 0 {
		c.EvidenceMaxTokens = defaults.EvidenceMaxTokens
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Validate reports every relationship that must hold before a turn is built. It does
// not fill anything: an unresolved Config whose fields are zero is refused rather
// than quietly completed, which is what makes it usable as a direct check.
func (c Config) Validate() error {
	switch {
	case c.WindowTokens < MinWindowTokens || c.WindowTokens > MaxWindowTokens:
		return fmt.Errorf("%w: %d, want %d to %d", ErrWindowOutOfRange,
			c.WindowTokens, MinWindowTokens, MaxWindowTokens)
	case c.InputTargetTokens < MinInputTargetTokens:
		return fmt.Errorf("%w: %d, want at least %d", ErrInputTargetOutOfRange,
			c.InputTargetTokens, MinInputTargetTokens)
	case c.OutputReserveTokens < MinOutputReserveTokens:
		return fmt.Errorf("%w: %d, want at least %d", ErrOutputReserveOutOfRange,
			c.OutputReserveTokens, MinOutputReserveTokens)
	case c.InputTargetTokens > c.WindowTokens:
		return fmt.Errorf("%w: target %d exceeds window %d", ErrTargetExceedsWindow,
			c.InputTargetTokens, c.WindowTokens)
	case c.InputTargetTokens+c.OutputReserveTokens > c.WindowTokens:
		return fmt.Errorf("%w: target %d plus reserve %d exceeds window %d", ErrTargetExceedsWindow,
			c.InputTargetTokens, c.OutputReserveTokens, c.WindowTokens)
	case c.SummaryMaxTokens < MinSummaryMaxTokens || c.SummaryMaxTokens > MaxSummaryMaxTokens:
		return fmt.Errorf("%w: %d, want %d to %d", ErrSummaryCapOutOfRange,
			c.SummaryMaxTokens, MinSummaryMaxTokens, MaxSummaryMaxTokens)
	case c.HistoryMaxTokens != 0 && (c.HistoryMaxTokens < MinHistoryMaxTokens || c.HistoryMaxTokens > MaxHistoryMaxTokens):
		return fmt.Errorf("%w: %d, want 0 or %d to %d", ErrHistoryCapOutOfRange,
			c.HistoryMaxTokens, MinHistoryMaxTokens, MaxHistoryMaxTokens)
	case c.HistoryFetchLimit < MinHistoryFetchLimit || c.HistoryFetchLimit > MaxHistoryFetchLimit:
		return fmt.Errorf("%w: %d, want %d to %d", ErrHistoryFetchLimitOutOfRange,
			c.HistoryFetchLimit, MinHistoryFetchLimit, MaxHistoryFetchLimit)
	case c.EvidenceMaxTokens < MinEvidenceMaxTokens:
		return fmt.Errorf("%w: %d, want at least %d", ErrEvidenceCapOutOfRange,
			c.EvidenceMaxTokens, MinEvidenceMaxTokens)
	case c.EvidenceMaxTokens >= c.InputTargetTokens:
		return fmt.Errorf("%w: evidence %d leaves no room inside target %d", ErrEvidenceCapOutOfRange,
			c.EvidenceMaxTokens, c.InputTargetTokens)
	}
	if c.HistoryMaxMessages != 0 && (c.HistoryMaxMessages < MinHistoryMaxMessages || c.HistoryMaxMessages > MaxHistoryMaxMessages) {
		return fmt.Errorf("%w: %d, want 0 or %d to %d", ErrHistoryCountOutOfRange,
			c.HistoryMaxMessages, MinHistoryMaxMessages, MaxHistoryMaxMessages)
	}
	if c.HistoryMaxRunes != 0 && (c.HistoryMaxRunes < MinHistoryMaxRunes || c.HistoryMaxRunes > MaxHistoryMaxRunes) {
		return fmt.Errorf("%w: %d, want 0 or %d to %d", ErrHistoryRunesOutOfRange,
			c.HistoryMaxRunes, MinHistoryMaxRunes, MaxHistoryMaxRunes)
	}
	// The production layers are independently named even when a provider returns
	// an empty block. A negative layer is a configuration fault; zero means the
	// provider is absent and does not reserve a hidden slice.
	for name, value := range map[string]int{
		"global memory": c.GlobalMemoryMaxTokens, "topic memory": c.TopicMemoryMaxTokens,
		"mastery": c.MasteryMaxTokens, "episode": c.EpisodeMaxTokens,
	} {
		if value < 0 {
			return fmt.Errorf("%s budget must not be negative", name)
		}
	}
	return c.validateRelationships()
}

func (c Config) validateRelationships() error {
	// The arithmetic check is separate from the range switch because it is a
	// relationship between otherwise legal figures. Optional clean-only history
	// caps are ignored when zero, so production v2 does not inherit them.
	if c.SummaryMaxTokens+c.HistoryMaxTokens >
		c.InputTargetTokens-c.EvidenceMaxTokens-MinRequiredHeadroomTokens {
		return fmt.Errorf("%w: summary %d plus history %d against target %d minus evidence %d",
			ErrSubBudgetsExceedTarget, c.SummaryMaxTokens, c.HistoryMaxTokens,
			c.InputTargetTokens, c.EvidenceMaxTokens)
	}
	return nil
}

// CheckOutputCap reports a provider output limit that does not fit the reserve. It is
// a separate call because the output limit lives with the provider configuration, and
// the assembly point is the only place that holds both.
func (c Config) CheckOutputCap(maxOutputTokens int) error {
	if maxOutputTokens <= 0 {
		// Zero means the request defers to the configured default, and a negative
		// figure is not a limit at all. Neither can be compared with the reserve.
		return nil
	}
	if maxOutputTokens > c.OutputReserveTokens {
		return fmt.Errorf("%w: %d against a reserve of %d", ErrOutputCapExceedsReserve,
			maxOutputTokens, c.OutputReserveTokens)
	}
	return nil
}
