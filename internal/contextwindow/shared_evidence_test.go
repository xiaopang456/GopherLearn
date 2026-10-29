package contextwindow_test

import (
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/agent"
	"github.com/xiaopang456/GopherLearn/internal/contextwindow"
)

func TestSharedEvidenceBudgetAndDisplayMarkersAreBidirectional(t *testing.T) {
	registry := agent.NewTurnEvidence(100)
	budget := contextwindow.NewEvidence(100)
	registry.AttachBudget(budget)
	if !registry.ChargeTokens(80) {
		t.Fatal("registry charge under shared ceiling was refused")
	}
	if budget.Used() != 80 || registry.UsedTokens() != 80 || budget.Remaining() != registry.RemainingTokens() {
		t.Fatalf("budget receipt diverged: budget=%d/%d registry=%d/%d", budget.Used(), budget.Remaining(), registry.UsedTokens(), registry.RemainingTokens())
	}
	if registry.ChargeTokens(30) {
		t.Fatal("registry accepted a shared over-budget charge")
	}
	if budget.Used() != 80 {
		t.Fatalf("rejected charge changed shared used=%d", budget.Used())
	}
	key := "document|x"
	budget.MarkBodySent(key)
	if !registry.BodySent(key) {
		t.Fatal("budget marker was not visible through registry")
	}
	registry.MarkBodySent("memory|y")
	if !budget.BodySent("memory|y") {
		t.Fatal("registry marker was not visible through budget")
	}
}
