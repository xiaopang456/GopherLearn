package config

import "testing"

// The pre-retrieval's threshold is a deployment setting, and the value it
// ships with is the one the reranker's own calibration produced: above the
// highest score a question the material does not answer reached, below the
// median of the ones it does.
//
// A default that moved would move every answer the product gives, so it is
// pinned here rather than left to the code that happens to read it.
func TestKnowledgeRelevanceThresholdShipsAtTheCalibratedValue(t *testing.T) {
	cfg := defaultConfig()
	if cfg.AgentConfig.KnowledgeRelevanceThreshold != 0.80 {
		t.Fatalf("default threshold = %v, want 0.80", cfg.AgentConfig.KnowledgeRelevanceThreshold)
	}
	if err := cfg.AgentConfig.Validate(); err != nil {
		t.Fatal(err)
	}
}

// Zero means the pre-retrieval does not run. It is the one value outside the
// "a rerank score" reading that still has to be allowed, because it is what a
// deployment without a reranker gets.
func TestKnowledgeRelevanceThresholdAcceptsZeroAndRefusesWhatNoScoreCanReach(t *testing.T) {
	cfg := defaultConfig()
	cfg.AgentConfig.KnowledgeRelevanceThreshold = 0
	if err := cfg.AgentConfig.Validate(); err != nil {
		t.Fatalf("zero was refused: %v", err)
	}
	for _, invalid := range []float64{-0.1, 1.1} {
		broken := defaultConfig()
		broken.AgentConfig.KnowledgeRelevanceThreshold = invalid
		if err := broken.AgentConfig.Validate(); err == nil {
			t.Fatalf("threshold %v was accepted", invalid)
		}
	}
}
