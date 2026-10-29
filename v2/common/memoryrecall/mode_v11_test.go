package memoryrecall

import "testing"

// STABILIZE-058 v1.2: a mode that exists but is missing from the enumeration is
// a mode a deployment cannot select and an operator cannot discover from the
// error message. Both were true of the v1.1 identity when it was added.
func TestEveryValidModeIsEnumeratedAndParseable(t *testing.T) {
	listed := map[ReadMode]bool{}
	for _, mode := range ModeNames() {
		listed[mode] = true
		if !mode.Valid() {
			t.Errorf("%s is enumerated but not valid", mode)
		}
		parsed, err := ParseReadMode(string(mode))
		if err != nil || parsed != mode {
			t.Errorf("%s did not parse back: %v", mode, err)
		}
	}
	for _, mode := range []ReadMode{
		ModeLegacyV2, ModeEpisodeV3, ModeEpisodeV3Lexical, ModeEpisodeV3Semantic,
		ModeModelPlannedV1, ModeAgentMemoryToolV1, ModeAgentMemoryToolV2,
		ModeAgentMemoryToolV3, ModeAgentMemoryToolV4, ModeAgentMemoryUserConfirmedV1,
		ModeEpisodeAssertionGovernedV11,
	} {
		if !listed[mode] {
			t.Errorf("%s is valid but ModeNames() omits it", mode)
		}
	}
}

// The refusal message must name every option, or an operator who mistypes gets
// a list that silently excludes the mode they wanted.
func TestParseErrorNamesEveryMode(t *testing.T) {
	_, err := ParseReadMode("nonsense")
	if err == nil {
		t.Fatal("an unknown mode was accepted")
	}
	for _, mode := range ModeNames() {
		if !contains(err.Error(), string(mode)) {
			t.Errorf("the refusal message omits %s", mode)
		}
	}
}

func contains(haystack, needle string) bool {
	for index := 0; index+len(needle) <= len(haystack); index++ {
		if haystack[index:index+len(needle)] == needle {
			return true
		}
	}
	return false
}

// The production default is unchanged, and the new identity is not it.
func TestNewIdentityIsNotTheDefault(t *testing.T) {
	if DefaultReadMode() != ModeLegacyV2 {
		t.Fatal("the production default moved")
	}
	if DefaultReadMode() == ModeEpisodeAssertionGovernedV11 {
		t.Fatal("the v1.1 identity became the default")
	}
}
