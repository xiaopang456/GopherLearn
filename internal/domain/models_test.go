package domain

import "testing"

func TestSessionModeIsValidAcceptsOnlyFinalModes(t *testing.T) {
	valid := []SessionMode{SessionModeChat, SessionModeLearningAgent}
	for _, mode := range valid {
		if !mode.IsValid() {
			t.Errorf("mode %q should be valid", mode)
		}
	}

	invalid := []SessionMode{
		"",
		"fast_chat",
		"knowledge_qa",
		"agent",
		"direct_chat",
		"Chat",
		"chat ",
		" learning_agent",
	}
	for _, mode := range invalid {
		if mode.IsValid() {
			t.Errorf("mode %q should be invalid", mode)
		}
	}
}

func TestAnswerStrategyIsValidAcceptsOnlyThreeStrategies(t *testing.T) {
	valid := []AnswerStrategy{AnswerStrategyDirectChat, AnswerStrategyKnowledgeQA, AnswerStrategyAgent}
	for _, strategy := range valid {
		if !strategy.IsValid() {
			t.Errorf("strategy %q should be valid", strategy)
		}
	}

	invalid := []AnswerStrategy{"", "chat", "learning_agent", "fast_chat", "rag", "AGENT", "knowledge_qa "}
	for _, strategy := range invalid {
		if strategy.IsValid() {
			t.Errorf("strategy %q should be invalid", strategy)
		}
	}
}
