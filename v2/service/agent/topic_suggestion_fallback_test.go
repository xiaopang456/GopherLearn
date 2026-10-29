package agent

import (
	"context"
	"testing"
	"time"

	"GopherAI/service/agent/topicconsent"
)

func TestCatalogueSelectionsAcrossSearchesShareOneFinalTicket(t *testing.T) {
	topics := []topicconsent.Topic{
		{Key: "jwt-鉴权", Name: "JWT 鉴权", Graded: true},
		{Key: "docker-容器编排", Name: "Docker 容器编排", Notes: 1},
	}
	suggester, err := topicconsent.NewSuggester(topicconsent.TopicListerFunc(
		func(_ context.Context, userID int64) ([]topicconsent.Topic, error) {
			if userID != 7 {
				return nil, nil
			}
			return topics, nil
		}), topicconsent.MaxCandidates)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := topicconsent.NewSigner("fallback-test-secret-that-is-long-enough", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewTopicSuggestionService(suggester, signer)
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"JWT", "Docker"} {
		page, err := service.BrowseCatalogue(context.Background(), 7, query, 0, 20)
		if err != nil || len(page.Candidates) != 1 {
			t.Fatalf("query %q page=%+v err=%v", query, page, err)
		}
	}

	issued, err := service.IssueSelection(context.Background(), 7, "session-1", "request-1", "比较两种做法",
		[]string{"jwt-鉴权", "docker-容器编排"})
	if err != nil {
		t.Fatal(err)
	}
	verified, err := signer.Verify(topicconsent.VerifyRequest{
		Ticket: issued.Ticket, Selected: []string{"docker-容器编排", "jwt-鉴权"},
		UserID: 7, SessionID: "session-1", RequestID: "request-1", Question: "比较两种做法",
	})
	if err != nil || len(verified) != 2 {
		t.Fatalf("verified=%v err=%v", verified, err)
	}
}
