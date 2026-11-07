package conversation

import (
	"testing"

	"GopherAI/common/memoryrecall"
	"GopherAI/config"
	"GopherAI/entry"
)

// The window is built under the deployment's own read path, and the harness
// says which one it was.
//
// The mode decides whether a compressed card's evidence ids may be read back,
// so it decides what each arm's authorisation set contains. A harness that
// quietly chose a mode would measure a product nobody ships, and an arm whose
// authorisation set came out empty would have no way to say whether the window
// was empty or the mode forbade it.
func TestTheWindowIsBuiltUnderTheDeploymentsOwnReadMode(t *testing.T) {
	for _, mode := range []memoryrecall.ReadMode{
		memoryrecall.ModeLegacyV2,
		memoryrecall.ModeAgentMemorySourcePriorityV2,
		memoryrecall.ModeAgentMemorySourcePriorityV3,
	} {
		settings := &entry.Settings{Config: &config.Config{}}
		settings.Config.MemoryReadMode = string(mode)

		window := windowContextConfig(settings)
		if window.MemoryReadMode != mode {
			t.Fatalf("the window's read mode is %q, want %q", window.MemoryReadMode, mode)
		}
		// The mode's own answer to "may a compressed card's ids be read back"
		// is what the report has to carry beside the authorisation set.
		if window.MemoryReadMode.ReadsEvidenceBack() != mode.ReadsEvidenceBack() {
			t.Fatalf("the mode %q answered the read-back rule differently from the harness", mode)
		}
	}
}
