package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"GopherAI/model"
)

// The card path reaches the window: the cards the provider returns are
// rendered, they are charged to the budget, and their citations become
// read-back authorisations.
func TestContextBuilderInjectsSessionCardsAndGrantsTheirCitations(t *testing.T) {
	config := DefaultContextConfig()
	config.MaxEpisodeTokens = 1000
	builder, err := NewContextBuilder(config, &fakeHistoryReader{}, nil, nil, nil, staticSummary("current-session summary"))
	if err != nil {
		t.Fatal(err)
	}
	card := model.EpisodeMemory{
		ID: "card-1", UserID: 7, SessionID: "source-session",
		ContentKind: model.EpisodeContentSessionCard,
		Title:       "Go 并发练习",
		Card: model.SessionCardDocument{
			Version:  model.SessionCardContentVersion,
			Title:    "Go 并发练习",
			Synopsis: "讨论了每轮题量。",
			Arrangements: []model.SessionCardEntry{
				{Text: "每轮9道题", Status: model.SessionCardStatusStated, MessageIDs: []uint{12}},
			},
		},
		SearchText: "Go 并发练习", SourceLastMessageID: 144,
		LastMessageAt: time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC),
		Version:       2, Status: model.EpisodeStatusActive, IndexStatus: model.EpisodeIndexIndexed,
	}
	builder.UseSessionCardProvider(SessionCardProviderFunc(func(context.Context, RunContext) ([]model.EpisodeMemory, error) {
		return []model.EpisodeMemory{card}, nil
	}))
	window, err := builder.Build(context.Background(), RunContext{UserID: 7, SessionID: "current", Question: "上次题量怎么定的？", EpisodeRecallReady: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(window.SessionCardIDs) != 1 || window.SessionCardIDs[0] != "card-1" {
		t.Fatalf("the card was not shown: %v", window.SessionCardIDs)
	}
	if window.Budget.SessionCardCount != 1 || window.Budget.SessionCardMemory == 0 {
		t.Fatalf("the card was not charged to the budget: %+v", window.Budget)
	}
	joined := ""
	for _, message := range window.Messages {
		joined += message.Content + "\n"
	}
	if !strings.Contains(joined, "source_session: source-session") {
		t.Fatalf("the card block is not in the window:\n%s", joined)
	}
	if len(window.SessionCardHistoryGrants) != 1 {
		t.Fatalf("the card granted %d ids", len(window.SessionCardHistoryGrants))
	}
	grant := window.SessionCardHistoryGrants[0]
	if grant.MessageID != 12 || grant.SourceKind != HistorySourceSessionCard || grant.CardID != "card-1" {
		t.Fatalf("the grant is %+v", grant)
	}
}

// The card path replaces the Episode path: attaching the card provider
// detaches the Episode provider, so a window never carries two accounts of
// the same conversation.
func TestContextBuilderDoesNotCarryCardsAndEpisodesAtOnce(t *testing.T) {
	config := DefaultContextConfig()
	config.EpisodeRecallMode = EpisodeRecallInject
	builder, err := NewContextBuilder(config, &fakeHistoryReader{}, nil, nil, nil, staticSummary("current-session summary"))
	if err != nil {
		t.Fatal(err)
	}
	builder.UseEpisodeProvider(EpisodeProviderFunc(func(context.Context, RunContext) ([]model.EpisodeMemory, error) {
		return []model.EpisodeMemory{testEpisode("episode-1", "一次讨论", time.Now())}, nil
	}))
	builder.UseSessionCardProvider(SessionCardProviderFunc(func(context.Context, RunContext) ([]model.EpisodeMemory, error) {
		return []model.EpisodeMemory{cardRow("card-1", "source-session", nil, nil)}, nil
	}))
	window, err := builder.Build(context.Background(), RunContext{UserID: 7, SessionID: "current", Question: "上次呢？", EpisodeRecallReady: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(window.EpisodeIDs) != 0 {
		t.Fatalf("the window carries episodes as well: %v", window.EpisodeIDs)
	}
	if len(window.SessionCardIDs) != 1 {
		t.Fatalf("the window carries %d cards", len(window.SessionCardIDs))
	}
}
