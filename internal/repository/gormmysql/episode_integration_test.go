package gormmysql

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/repository/repositorytest"
)

// The episodic-memory rules that only a real MySQL engine can answer. The
// shared behavioural contract (repositorytest.EpisodeSuite) runs against this
// engine inside TestRealMySQLContract; the scenarios below are the engine
// specific ones - conditional-update races, lease recovery across a second
// connection, and the same-transaction delete cascade - stated on the engine
// production runs, never on the embedded stand-in.
//
// Every scenario opens its own throwaway schema through openIntegrationSchema:
// an unreachable server fails at connect, and no SQLite fallback exists on
// this path.

// TestRealMySQLEpisodePersistenceAndRaces covers four things the embedded
// engine cannot fully prove: a concurrent claim race decided by the
// conditional update, lease recovery across a restarted "process", optimistic
// version control seen through a second connection, and the same-transaction
// session-delete cascade beating an in-flight projection. Each subscenario
// truncates the schema and seeds its own world, so none of them inherits
// another's version or job state.
func TestRealMySQLEpisodePersistenceAndRaces(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv(EnvRealMySQLDSN))
	if dsn == "" {
		t.Skipf("%s is unset; this scenario needs a real MySQL server", EnvRealMySQLDSN)
	}
	db := openIntegrationSchema(t, dsn)
	if got := db.Dialector.Name(); got != "mysql" {
		t.Fatalf("the integration connection is %q, want the real mysql engine", got)
	}
	ctx := context.Background()

	// episodeWorld is one scenario's private data: a user, a session ending
	// on an assistant turn, its projected card, and a published job. Called
	// after truncateAll, it is the subtest's whole universe.
	episodeWorld := func(t *testing.T) (repository.Repository, string, string, string) {
		t.Helper()
		store := NewRepository(db)
		user, err := store.CreateUser(ctx, &domain.User{Email: "episode-real@example.com", DisplayName: "Real"})
		if err != nil {
			t.Fatal(err)
		}
		session, err := store.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "Real", Mode: domain.SessionModeChat})
		if err != nil {
			t.Fatal(err)
		}
		question, err := store.AppendMessage(ctx, user.ID, session.ID, &domain.Message{Role: "user", Content: "我决定用 JSON 编解码器。"})
		if err != nil {
			t.Fatal(err)
		}
		answer, err := store.AppendMessage(ctx, user.ID, session.ID, &domain.Message{Role: "assistant", Content: "好的。"})
		if err != nil {
			t.Fatal(err)
		}
		card := repositorytest.EpisodeFixtureFor(user.ID, session.ID, question.ID)
		if err := store.SaveEpisode(ctx, card, 0); err != nil {
			t.Fatalf("save card: %v", err)
		}
		if err := store.CommitEpisodeIndexStatus(ctx, user.ID, card.ID, card.Version, domain.EpisodeIndexIndexed); err != nil {
			t.Fatalf("mark indexed: %v", err)
		}
		return store, user.ID, session.ID, answer.ID
	}

	t.Run("concurrent claims decide exactly one winner", func(t *testing.T) {
		truncateAll(t, db)
		store, userID, sessionID, answerID := episodeWorld(t)
		now := time.Now().UTC()
		if _, err := store.UpsertEpisodeDesired(ctx, userID, sessionID, answerID, now); err != nil {
			t.Fatalf("publish: %v", err)
		}
		due, err := store.ListDueEpisodeJobs(ctx, 10, 3, now)
		if err != nil {
			t.Fatal(err)
		}
		if len(due) != 1 {
			t.Fatalf("due jobs = %d, want the seeded one", len(due))
		}
		jobID := due[0].ID

		const contenders = 8
		results := make(chan error, contenders)
		for index := 0; index < contenders; index++ {
			go func() {
				_, err := store.ClaimEpisodeJob(ctx, userID, jobID, now, time.Hour, 3)
				results <- err
			}()
		}
		wins := 0
		for index := 0; index < contenders; index++ {
			if err := <-results; err == nil {
				wins++
			}
		}
		if wins != 1 {
			t.Fatalf("%d of %d concurrent claims won on real MySQL, want exactly one", wins, contenders)
		}
	})

	t.Run("lease recovery revives the stranded claim", func(t *testing.T) {
		truncateAll(t, db)
		store, userID, sessionID, answerID := episodeWorld(t)
		now := time.Now().UTC()
		if _, err := store.UpsertEpisodeDesired(ctx, userID, sessionID, answerID, now); err != nil {
			t.Fatalf("publish: %v", err)
		}
		due, err := store.ListDueEpisodeJobs(ctx, 10, 3, now)
		if err != nil || len(due) != 1 {
			t.Fatalf("due jobs = %v (%v), want one", due, err)
		}
		// A worker claims and then dies before completing. Its lease lapses;
		// the recovery pass a restarted process runs must hand the job back.
		if _, err := store.ClaimEpisodeJob(ctx, userID, due[0].ID, now, time.Hour, 3); err != nil {
			t.Fatalf("claim: %v", err)
		}
		recovered, err := store.RecoverExpiredEpisodeJobs(ctx, now.Add(2*time.Hour), 3)
		if err != nil {
			t.Fatalf("recover: %v", err)
		}
		if recovered != 1 {
			t.Fatalf("recovered %d jobs, want 1", recovered)
		}
		// The reclaimed job is claimable again, with the attempt counted.
		again, err := store.ListDueEpisodeJobs(ctx, 10, 3, now.Add(2*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if len(again) != 1 {
			t.Fatalf("due jobs after recovery = %d, want 1", len(again))
		}
	})

	t.Run("version control survives a reopen", func(t *testing.T) {
		truncateAll(t, db)
		store, userID, sessionID, _ := episodeWorld(t)
		stored, err := store.GetEpisodeBySession(ctx, userID, sessionID)
		if err != nil {
			t.Fatalf("read the seeded card: %v", err)
		}
		// A second, independent connection to the same schema: what the first
		// "process" wrote, a process that never wrote it must see and guard.
		reopened := NewRepository(reopenIntegrationSchema(t, db))
		reread, err := reopened.GetEpisodeBySession(ctx, userID, sessionID)
		if err != nil {
			t.Fatalf("read after reopen: %v", err)
		}
		if reread.ID != stored.ID || reread.Version != stored.Version {
			t.Fatalf("reopened card drifted: %#v vs %#v", reread, stored)
		}
		advanced := *reread
		advanced.SearchText += " 第二版"
		advanced.SourceLastMessageID += "-x"
		if err := reopened.SaveEpisode(ctx, &advanced, reread.Version); err != nil {
			t.Fatalf("advance version after reopen: %v", err)
		}
		if advanced.Version != reread.Version+1 {
			t.Fatalf("advanced version = %d, want %d", advanced.Version, reread.Version+1)
		}
		if err := reopened.SaveEpisode(ctx, &advanced, reread.Version); !errors.Is(err, repository.ErrEpisodeConflict) {
			t.Fatalf("stale replay = %v, want ErrEpisodeConflict", err)
		}
	})

	t.Run("session delete beats an in-flight projection", func(t *testing.T) {
		truncateAll(t, db)
		store, userID, sessionID, answerID := episodeWorld(t)
		now := time.Now().UTC()
		if _, err := store.UpsertEpisodeDesired(ctx, userID, sessionID, answerID, now); err != nil {
			t.Fatalf("publish: %v", err)
		}
		due, err := store.ListDueEpisodeJobs(ctx, 10, 3, now)
		if err != nil || len(due) != 1 {
			t.Fatalf("due jobs = %v (%v), want one", due, err)
		}
		// Claim, then delete: the stranded lease's completion is refused, and
		// nothing the worker writes afterwards can resurrect the job.
		claimed, err := store.ClaimEpisodeJob(ctx, userID, due[0].ID, now, time.Hour, 3)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		var episodeIDs []string
		if stored, err := store.GetEpisodeBySession(ctx, userID, sessionID); err == nil {
			episodeIDs = []string{stored.ID}
		}
		if err := store.DeleteSession(ctx, userID, sessionID); err != nil {
			t.Fatalf("delete session: %v", err)
		}
		if _, err := store.CompleteOrRequeueEpisodeJob(ctx, claimed, claimed.ClaimedMessageID, now.Add(time.Hour)); !errors.Is(err, repository.ErrEpisodeJobConflict) {
			t.Fatalf("stranded completion = %v, want ErrEpisodeJobConflict", err)
		}
		// The card and the job are gone with the session, in its transaction.
		remaining, err := store.DeleteEpisodesBySession(ctx, userID, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		if len(remaining) != 0 {
			t.Fatalf("the session delete left %d card row(s) behind on real MySQL", len(remaining))
		}
		if len(episodeIDs) != 1 {
			t.Fatalf("expected to capture one card id, got %v", episodeIDs)
		}
	})
}
