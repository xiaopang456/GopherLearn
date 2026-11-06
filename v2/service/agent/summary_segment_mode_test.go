package agent

import "testing"

// The applied mode does not read the old rolling summary.
//
// The episode projection says what happened in the session, and the old
// rolling summary is a compression of a part of it that was frozen before the
// layered summary existed. A value that was replaced afterwards is still in
// there, so reading it would let a stale statement become an assertion about
// the learner.
func TestTheAppliedModeDoesNotReadTheOldRollingSummary(t *testing.T) {
	if EpisodeReadsLegacySummary(SummarySegmentModeOn) {
		t.Fatal("the applied mode reads the old rolling summary")
	}
	if !EpisodeReadsLegacySummary(SummarySegmentModeShadow) {
		t.Fatal("the shadow mode stopped reading the old rolling summary")
	}
	if !EpisodeReadsLegacySummary(SummarySegmentModeOff) {
		t.Fatal("the off mode stopped reading the old rolling summary")
	}
}
