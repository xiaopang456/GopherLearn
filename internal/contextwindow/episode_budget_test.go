package contextwindow

import (
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/responder"
)

func TestEpisodeUsesOnlyRemainingAfterSummaryAndHistory(t *testing.T) {
	config := validConfig(t)
	history := []responder.Turn{
		{Role: model.RoleUser, Content: strings.Repeat("历史", 120)},
		{Role: model.RoleAssistant, Content: strings.Repeat("回答", 120)},
	}
	base := parts("规则", strings.Repeat("摘要", 160), history, "问题", nil)
	without := base
	without.EpisodeMemory = ""
	with, withoutBreakdown, err := Build(without, config)
	if err != nil {
		t.Fatal(err)
	}
	withParts := base
	withParts.EpisodeMemory = strings.Repeat("Episode", 500)
	withMessages, withBreakdown, err := Build(withParts, config)
	if err != nil {
		t.Fatal(err)
	}
	if withBreakdown.SummaryTokens != withoutBreakdown.SummaryTokens ||
		withBreakdown.HistoryTokens != withoutBreakdown.HistoryTokens ||
		withBreakdown.HistoryKept != withoutBreakdown.HistoryKept {
		t.Fatalf("Episode displaced summary/history: without=%+v with=%+v", withoutBreakdown, withBreakdown)
	}
	if withBreakdown.EpisodeMemoryTokens == 0 || len(withMessages) <= len(with) {
		t.Fatalf("Episode did not enter when remaining budget existed: %+v", withBreakdown)
	}
	if withBreakdown.AssembledTokens > config.InputTargetTokens-config.EvidenceMaxTokens {
		t.Fatalf("Episode pushed the request over the available target: %+v", withBreakdown)
	}
}

func TestEpisodeIsOmittedWhenSummaryAndHistoryLeaveNoRoom(t *testing.T) {
	config := validConfig(t)
	config.InputTargetTokens = 4096
	config.EvidenceMaxTokens = 128
	config.SummaryMaxTokens = 2000
	config.HistoryMaxTokens = 800
	config, err := config.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	history := []responder.Turn{{Role: model.RoleUser, Content: strings.Repeat("历史", 180)}, {Role: model.RoleAssistant, Content: strings.Repeat("回答", 180)}}
	parts := parts("规则", strings.Repeat("摘要", 250), history, strings.Repeat("问题", 1200), nil)
	parts.EpisodeMemory = strings.Repeat("Episode", 500)
	messages, breakdown, err := Build(parts, config)
	if err != nil {
		t.Fatal(err)
	}
	if breakdown.EpisodeMemoryTokens != 0 {
		t.Fatalf("Episode was not omitted: %+v", breakdown)
	}
	if strings.Contains(joinMessages(messages), "Episode") {
		t.Fatal("omitted Episode reached the prompt")
	}
	if breakdown.AssembledTokens > config.InputTargetTokens-config.EvidenceMaxTokens {
		t.Fatalf("request exceeded budget: %+v", breakdown)
	}
}

func joinMessages(messages []model.Message) string {
	var out strings.Builder
	for _, message := range messages {
		out.WriteString(message.Content)
	}
	return out.String()
}
