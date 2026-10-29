package gormmysql

import (
	"path/filepath"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/repository/repositorytest"
)

// TestEpisodeContract runs the shared episodic memory suite against the SQL
// store, so the production engine is held to exactly the rules the development
// store follows: one versioned card per session, durable jobs a crashed worker
// cannot duplicate, and a candidate reload that refuses anything the authority
// no longer backs.
func TestEpisodeContract(t *testing.T) {
	repositorytest.EpisodeSuite(t, func(*testing.T) repository.Repository { return NewRepository(newTestDB(t)) })
}

// TestEpisodeSurvivesReopeningTheDatabase is the restart case: a projected card
// and its job state are only worth writing if a new process reads back the same
// version, the same watermark and the same index status.
func TestEpisodeSurvivesReopeningTheDatabase(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "episode-reopen.db")
	first := NewRepository(openTestDB(t, path))
	repositorytest.SeedEpisodeScenario(t, first)

	user, err := first.FindUserByEmail(ctx, "episode-restart@example.com")
	if err != nil {
		t.Fatalf("find seeded user: %v", err)
	}
	sessions, err := first.ListSessions(ctx, user.ID)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	card, err := first.GetEpisodeBySession(ctx, user.ID, sessions[0].ID)
	if err != nil {
		t.Fatalf("read card from the first process: %v", err)
	}

	// A second "process" reopens the same file. Everything the first process
	// wrote must still be there, including the index status the indexer
	// recorded for that exact version.
	second := NewRepository(openTestDB(t, path))
	reopened, err := second.GetEpisodeBySession(ctx, user.ID, sessions[0].ID)
	if err != nil {
		t.Fatalf("read card from the second process: %v", err)
	}
	if reopened.ID != card.ID || reopened.Version != card.Version ||
		reopened.SourceLastMessageID != card.SourceLastMessageID ||
		reopened.IndexStatus != card.IndexStatus ||
		reopened.Document.Synopsis != card.Document.Synopsis {
		t.Fatalf("reopened card drifted: %#v vs %#v", reopened, card)
	}
	// And the store refuses to let the reopened process rewrite history: the
	// optimistic version the first process saw is still the live one.
	bumped := *reopened
	bumped.SearchText += " rewrote"
	if err := second.SaveEpisode(ctx, &bumped, reopened.Version-1); err == nil {
		t.Fatal("a stale expected version was accepted after reopen")
	}
}
