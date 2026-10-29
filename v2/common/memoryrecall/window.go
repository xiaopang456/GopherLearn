package memoryrecall

// WindowPolicy names how one memory_search observation is bounded.
//
// It sits beside ReadMode rather than inside the tool because it is chosen the
// same way a read mode is - once, in configuration, for a whole deployment -
// and because configuration validation must be able to reject an unknown value
// without importing the Agent.
//
// It is deliberately not a sixth ReadMode. A mode is a complete read path with
// its own measurements; this changes only how much of an already-selected
// candidate set one observation carries, and making it a mode would have
// implied a second path to keep measured.
type WindowPolicy string

const (
	// WindowFixed is the shipped behaviour: the merged window and nothing else.
	// It is the zero value, so a deployment that never heard of this setting
	// gets the method its measurements were taken under.
	WindowFixed WindowPolicy = "w0_fixed"
	// WindowTokenBounded is STABILIZE-036. On a mixed-kind call where both
	// kinds had eligible records, the observation may carry whole cards the
	// final merge dropped, up to eight cards and up to
	// min(2800, floor(base x 1.35)) tokens.
	//
	// It is an experiment, not a default. STABILIZE-035 showed that widening
	// every search buys a little recall at half again the input cost; whether
	// widening only the squeezed searches is worth it is what the paired run is
	// for, and until that answers, production reads legacy_v2.
	WindowTokenBounded WindowPolicy = "token_bounded_mixed_v1"
)

func (p WindowPolicy) Valid() bool {
	switch p {
	case "", WindowFixed, WindowTokenBounded:
		return true
	}
	return false
}

// WidensSqueezedCalls is the single place that answers "does this policy
// append". Every caller asks it rather than comparing strings, so adding a
// policy cannot leave one call site reading the old set of values.
func (p WindowPolicy) WidensSqueezedCalls() bool { return p == WindowTokenBounded }
