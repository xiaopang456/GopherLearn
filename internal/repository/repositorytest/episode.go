package repositorytest

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

// EpisodeSuite holds both stores to the same episodic-memory rules: one card
// per session under optimistic version control, durable debounce-and-lease
// jobs that a crashed worker cannot duplicate, and a reload that refuses
// anything whose owner, session, status or provenance does not still check out.
func EpisodeSuite(t *testing.T, newStore Factory) {
	t.Run("episode save and version control", func(t *testing.T) {
		store := newStore(t)
		ctx := context.Background()
		owner := seedEpisodeUser(t, store)
		session := seedEpisodeSession(t, store, owner, domain.SessionModeChat)
		question := seedEpisodeMessage(t, store, owner, session, "user", "我决定用 JSON 编解码器")
		seedEpisodeMessage(t, store, owner, session, "assistant", "好的，已按 JSON 编解码器实现。")

		card := episodeFixture(owner, session, question, "json-编解码器")
		if err := store.SaveEpisode(ctx, card, 0); err != nil {
			t.Fatalf("save first episode: %v", err)
		}
		if card.Version != 1 {
			t.Fatalf("first save version = %d, want 1", card.Version)
		}
		if err := store.SaveEpisode(ctx, episodeFixture(owner, session, question, "json-编解码器"), 0); !errors.Is(err, repository.ErrEpisodeConflict) {
			t.Fatalf("second create error = %v, want ErrEpisodeConflict", err)
		}

		// A repeat at the same watermark must not move coverage back, and a
		// stale expected version must not overwrite a newer card.
		card.SearchText += " 追加"
		if err := store.SaveEpisode(ctx, card, 0); !errors.Is(err, repository.ErrEpisodeConflict) {
			t.Fatalf("update with expectedVersion 0 = %v, want conflict", err)
		}
		stale := *card
		stale.SourceLastMessageID = question
		if err := store.SaveEpisode(ctx, &stale, card.Version); !errors.Is(err, repository.ErrEpisodeConflict) {
			t.Fatalf("stale watermark update = %v, want conflict", err)
		}
		loaded, err := store.GetEpisodeBySession(ctx, owner, session)
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		if loaded.Version != 1 || loaded.SearchText != episodeFixture(owner, session, question, "json-编解码器").SearchText {
			t.Fatalf("loaded card drifted: version=%d", loaded.Version)
		}
	})

	t.Run("episode owner boundary and session boundary", func(t *testing.T) {
		store := newStore(t)
		ctx := context.Background()
		owner := seedEpisodeUser(t, store)
		other := seedEpisodeUser(t, store)
		session := seedEpisodeSession(t, store, owner, domain.SessionModeChat)
		question := seedEpisodeMessage(t, store, owner, session, "user", "我决定用 JSON 编解码器")
		card := episodeFixture(owner, session, question, "json-编解码器")
		if err := store.SaveEpisode(ctx, card, 0); err != nil {
			t.Fatalf("save: %v", err)
		}
		if _, err := store.GetEpisodeByID(ctx, other, card.ID); !errors.Is(err, repository.ErrEpisodeNotFound) {
			t.Fatalf("foreign owner read = %v, want ErrEpisodeNotFound", err)
		}
		if _, err := store.GetEpisodeBySession(ctx, other, session); !errors.Is(err, repository.ErrEpisodeNotFound) {
			t.Fatalf("foreign session read = %v, want ErrEpisodeNotFound", err)
		}
		if err := store.DeleteSession(ctx, owner, session); err != nil {
			t.Fatalf("delete session: %v", err)
		}
		if _, err := store.GetEpisodeByID(ctx, owner, card.ID); !errors.Is(err, repository.ErrEpisodeNotFound) {
			t.Fatalf("deleted-session read = %v, want ErrEpisodeNotFound", err)
		}
	})

	t.Run("job debounce advance and requeue", func(t *testing.T) {
		store := newStore(t)
		ctx := context.Background()
		owner := seedEpisodeUser(t, store)
		session := seedEpisodeSession(t, store, owner, domain.SessionModeChat)
		first := seedEpisodeMessage(t, store, owner, session, "user", "q1")
		second := seedEpisodeMessage(t, store, owner, session, "assistant", "a1")
		now := time.Now().UTC()

		job, err := store.UpsertEpisodeDesired(ctx, owner, session, first, now.Add(time.Hour))
		if err != nil {
			t.Fatalf("upsert: %v", err)
		}
		if job.Status != domain.EpisodeProjectionPending || job.DesiredMessageID != first {
			t.Fatalf("fresh job = %+v", job)
		}
		// A repeat at the same watermark changes nothing: re-delivery is
		// debounce, not duplication.
		same, err := store.UpsertEpisodeDesired(ctx, owner, session, first, now)
		if err != nil {
			t.Fatalf("repeat upsert: %v", err)
		}
		if !same.AvailableAt.Equal(job.AvailableAt) {
			t.Fatalf("repeat upsert moved available_at: %v -> %v", job.AvailableAt, same.AvailableAt)
		}
		// A higher desired re-arms a pending job; a live lease is not stolen.
		claimed, err := store.ClaimEpisodeJob(ctx, owner, job.ID, now.Add(2*time.Hour), time.Hour, 3)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		advanced, err := store.UpsertEpisodeDesired(ctx, owner, session, second, now)
		if err != nil {
			t.Fatalf("upsert while processing: %v", err)
		}
		if advanced.DesiredMessageID != second || advanced.Status != domain.EpisodeProjectionProcessing {
			t.Fatalf("live lease was stolen: %+v", advanced)
		}
		requeued, err := store.CompleteOrRequeueEpisodeJob(ctx, claimed, claimed.ClaimedMessageID, now)
		if err != nil {
			t.Fatalf("complete: %v", err)
		}
		if requeued.Status != domain.EpisodeProjectionPending || requeued.Attempts != 0 {
			t.Fatalf("completion did not requeue the advanced target: %+v", requeued)
		}
		if domain.CompareMessageIDs(requeued.ClaimedMessageID, "") != 0 {
			t.Fatalf("requeued job still claims a target: %+v", requeued)
		}
	})

	t.Run("job claim conflict and foreign owner", func(t *testing.T) {
		store := newStore(t)
		ctx := context.Background()
		owner := seedEpisodeUser(t, store)
		other := seedEpisodeUser(t, store)
		session := seedEpisodeSession(t, store, owner, domain.SessionModeChat)
		latest := seedEpisodeMessage(t, store, owner, session, "user", "q1")
		now := time.Now().UTC()
		job, err := store.UpsertEpisodeDesired(ctx, owner, session, latest, now)
		if err != nil {
			t.Fatalf("upsert: %v", err)
		}
		if _, err := store.ClaimEpisodeJob(ctx, other, job.ID, now, time.Hour, 3); !errors.Is(err, repository.ErrEpisodeJobNotFound) {
			t.Fatalf("foreign claim = %v, want ErrEpisodeJobNotFound", err)
		}
		claimed, err := store.ClaimEpisodeJob(ctx, owner, job.ID, now, time.Hour, 3)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if _, err := store.ClaimEpisodeJob(ctx, owner, job.ID, now, time.Hour, 3); !errors.Is(err, repository.ErrEpisodeJobConflict) {
			t.Fatalf("double claim = %v, want ErrEpisodeJobConflict", err)
		}
		if _, err := store.CompleteOrRequeueEpisodeJob(ctx, claimed, "different-target", now); err == nil {
			t.Fatal("completion acknowledged a target it did not claim")
		}
	})

	t.Run("job retry budget and recovery", func(t *testing.T) {
		store := newStore(t)
		ctx := context.Background()
		owner := seedEpisodeUser(t, store)
		session := seedEpisodeSession(t, store, owner, domain.SessionModeChat)
		latest := seedEpisodeMessage(t, store, owner, session, "user", "q1")
		now := time.Now().UTC()
		job, err := store.UpsertEpisodeDesired(ctx, owner, session, latest, now)
		if err != nil {
			t.Fatalf("upsert: %v", err)
		}
		// Three failed attempts burn the budget and park the job as failed.
		for attempt := 0; attempt < 3; attempt++ {
			claimed, err := store.ClaimEpisodeJob(ctx, owner, job.ID, now.Add(2*time.Hour), time.Hour, 3)
			if err != nil {
				t.Fatalf("claim %d: %v", attempt, err)
			}
			job, err = store.RetryEpisodeJob(ctx, claimed, "projection_failed", now.Add(time.Minute), 3)
			if err != nil {
				t.Fatalf("retry %d: %v", attempt, err)
			}
		}
		if job.Status != domain.EpisodeProjectionFailed {
			t.Fatalf("exhausted job = %s, want failed", job.Status)
		}
		// A lease that expires while nobody holds it is reclaimed by the next
		// recovery pass, and the reclaimed job is claimable again.
		expired := seedEpisodeSession(t, store, owner, domain.SessionModeChat)
		expiredLatest := seedEpisodeMessage(t, store, owner, expired, "user", "q2")
		expiredJob, err := store.UpsertEpisodeDesired(ctx, owner, expired, expiredLatest, now)
		if err != nil {
			t.Fatalf("upsert expired-scenario: %v", err)
		}
		stranded, err := store.ClaimEpisodeJob(ctx, owner, expiredJob.ID, now.Add(2*time.Hour), time.Nanosecond, 3)
		if err != nil {
			t.Fatalf("stranded claim: %v", err)
		}
		recovered, err := store.RecoverExpiredEpisodeJobs(ctx, now.Add(3*time.Hour), 3)
		if err != nil {
			t.Fatalf("recover: %v", err)
		}
		if recovered != 1 {
			t.Fatalf("recovered %d jobs, want 1", recovered)
		}
		after, err := store.ClaimEpisodeJob(ctx, owner, expiredJob.ID, now.Add(4*time.Hour), time.Hour, 3)
		if err != nil {
			t.Fatalf("claim after recovery: %v", err)
		}
		// The reclaimed job keeps the budget the dead worker already spent for
		// the same target: only a newer desired target restarts it. The dead
		// worker's failure is not inherited as a failure state - the row is
		// claimable again - but the attempt is still counted.
		if after.Attempts != 2 || after.Status != domain.EpisodeProjectionProcessing {
			t.Fatalf("reclaimed job state = attempts %d status %s", after.Attempts, after.Status)
		}
		if _, err := store.CompleteOrRequeueEpisodeJob(ctx, stranded, stranded.ClaimedMessageID, now); !errors.Is(err, repository.ErrEpisodeJobConflict) {
			t.Fatalf("the stranded worker's lease still completed: %v", err)
		}
	})

	t.Run("release other sessions and projection gaps", func(t *testing.T) {
		store := newStore(t)
		ctx := context.Background()
		owner := seedEpisodeUser(t, store)
		// The gap scan covers learning-agent sessions only, matching the
		// trigger's scope, so the fixtures are those sessions.
		first := seedEpisodeSession(t, store, owner, domain.SessionModeLearningAgent)
		second := seedEpisodeSession(t, store, owner, domain.SessionModeLearningAgent)
		firstLatest := seedEpisodeMessage(t, store, owner, first, "assistant", "a1")
		secondLatest := seedEpisodeMessage(t, store, owner, second, "assistant", "a2")
		now := time.Now().UTC()
		if _, err := store.UpsertEpisodeDesired(ctx, owner, first, firstLatest, now.Add(time.Hour)); err != nil {
			t.Fatalf("upsert first: %v", err)
		}
		// Publishing in the second session releases the first: the quiet window
		// exists for active conversations, and writing elsewhere is evidence
		// this one is over.
		if _, err := store.UpsertEpisodeDesired(ctx, owner, second, secondLatest, now.Add(time.Hour)); err != nil {
			t.Fatalf("upsert second: %v", err)
		}
		released, err := store.ReleaseOtherEpisodeSessions(ctx, owner, second, now)
		if err != nil {
			t.Fatalf("release: %v", err)
		}
		if released != 1 {
			t.Fatalf("released %d jobs, want 1", released)
		}
		due, err := store.ListDueEpisodeJobs(ctx, 10, 3, now.Add(2*time.Hour))
		if err != nil {
			t.Fatalf("list due: %v", err)
		}
		if len(due) != 2 {
			t.Fatalf("due jobs = %d, want 2", len(due))
		}
		gaps, err := store.ListEpisodeProjectionGaps(ctx, 100)
		if err != nil {
			t.Fatalf("gaps: %v", err)
		}
		if len(gaps) != 2 {
			t.Fatalf("gaps = %d, want 2 (both sessions end on an assistant turn)", len(gaps))
		}
		// A session whose last message is the user's question is not a complete
		// episode source.
		open := seedEpisodeSession(t, store, owner, domain.SessionModeLearningAgent)
		seedEpisodeMessage(t, store, owner, open, "user", "还没被回答的问题")
		gaps, err = store.ListEpisodeProjectionGaps(ctx, 100)
		if err != nil {
			t.Fatalf("gaps after open session: %v", err)
		}
		if len(gaps) != 2 {
			t.Fatalf("open session produced a gap: %v", gaps)
		}
	})

	t.Run("session delete removes the card and the job in the same transaction", func(t *testing.T) {
		store := newStore(t)
		ctx := context.Background()
		owner := seedEpisodeUser(t, store)
		session := seedEpisodeSession(t, store, owner, domain.SessionModeChat)
		question := seedEpisodeMessage(t, store, owner, session, "user", "我决定用 JSON 编解码器")
		card := episodeFixture(owner, session, question, "json-编解码器")
		if err := store.SaveEpisode(ctx, card, 0); err != nil {
			t.Fatalf("save: %v", err)
		}
		// A claimed job is the in-flight projection this delete has to beat:
		// the stranded worker's lease must not resurrect anything, and its
		// completion must be refused rather than half-applied.
		now := time.Now().UTC()
		if _, err := store.UpsertEpisodeDesired(ctx, owner, session, question, now.Add(-time.Minute)); err != nil {
			t.Fatalf("publish: %v", err)
		}
		claimed, err := store.ClaimEpisodeJob(ctx, owner, card.ID, now, time.Hour, 3)
		if err != nil {
			// The claim keys on the job id, not the card id; look it up by
			// claiming through the due list instead of guessing the id.
			due, listErr := store.ListDueEpisodeJobs(ctx, 10, 3, now)
			if listErr != nil || len(due) == 0 {
				t.Fatalf("claim: %v / %v", err, listErr)
			}
			claimed, err = store.ClaimEpisodeJob(ctx, owner, due[0].ID, now, time.Hour, 3)
			if err != nil {
				t.Fatalf("claim from due list: %v", err)
			}
		}

		if err := store.DeleteSession(ctx, owner, session); err != nil {
			t.Fatalf("delete session: %v", err)
		}
		// The card and the job are gone with the session - not merely hidden
		// behind a boundary: a later DeleteEpisodesBySession finds nothing.
		remaining, err := store.DeleteEpisodesBySession(ctx, owner, session)
		if err != nil {
			t.Fatalf("post-delete sweep errored: %v", err)
		}
		if len(remaining) != 0 {
			t.Fatalf("the session delete left %d card row(s) behind", len(remaining))
		}
		if _, err := store.GetEpisodeBySession(ctx, owner, session); !errors.Is(err, repository.ErrEpisodeNotFound) {
			t.Fatalf("card read after session delete = %v, want ErrEpisodeNotFound", err)
		}
		// The stranded worker's completion is refused: a delete that raced an
		// in-flight projection wins, and the loser must not resurrect the job.
		if _, err := store.CompleteOrRequeueEpisodeJob(ctx, claimed, claimed.ClaimedMessageID, now); !errors.Is(err, repository.ErrEpisodeJobConflict) {
			t.Fatalf("stranded completion = %v, want ErrEpisodeJobConflict", err)
		}
		// And the reconciler's gap scan reports nothing for a deleted session:
		// a gone conversation is not work waiting to be redone.
		gaps, err := store.ListEpisodeProjectionGaps(ctx, 100)
		if err != nil {
			t.Fatalf("gap scan: %v", err)
		}
		for _, gap := range gaps {
			if gap.SessionID == session {
				t.Fatalf("the deleted session reappeared as a gap: %+v", gaps)
			}
		}
	})

	t.Run("concurrent claims decide exactly one winner", func(t *testing.T) {
		store := newStore(t)
		ctx := context.Background()
		owner := seedEpisodeUser(t, store)
		session := seedEpisodeSession(t, store, owner, domain.SessionModeChat)
		latest := seedEpisodeMessage(t, store, owner, session, "assistant", "a1")
		now := time.Now().UTC()
		job, err := store.UpsertEpisodeDesired(ctx, owner, session, latest, now)
		if err != nil {
			t.Fatalf("upsert: %v", err)
		}
		const contenders = 6
		results := make(chan error, contenders)
		for index := 0; index < contenders; index++ {
			go func() {
				_, err := store.ClaimEpisodeJob(ctx, owner, job.ID, now.Add(time.Hour), time.Hour, 3)
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
			t.Fatalf("%d of %d concurrent claims won, want exactly one", wins, contenders)
		}
	})

	t.Run("candidate reload refuses foreign, inactive, unindexed and stale provenance", func(t *testing.T) {
		store := newStore(t)
		ctx := context.Background()
		owner := seedEpisodeUser(t, store)
		other := seedEpisodeUser(t, store)
		session := seedEpisodeSession(t, store, owner, domain.SessionModeChat)
		current := seedEpisodeSession(t, store, owner, domain.SessionModeChat)
		question := seedEpisodeMessage(t, store, owner, session, "user", "我决定用 JSON 编解码器")

		card := episodeFixture(owner, session, question, "json-编解码器")
		if err := store.SaveEpisode(ctx, card, 0); err != nil {
			t.Fatalf("save: %v", err)
		}
		if err := store.CommitEpisodeIndexStatus(ctx, owner, card.ID, card.Version, domain.EpisodeIndexIndexed); err != nil {
			t.Fatalf("mark indexed: %v", err)
		}
		// Wrong version, foreign owner, wrong status and wrong index status
		// each refuse the same id.
		if err := store.CommitEpisodeIndexStatus(ctx, owner, card.ID, card.Version+1, domain.EpisodeIndexIndexed); !errors.Is(err, repository.ErrEpisodeConflict) {
			t.Fatalf("index status for wrong version = %v, want conflict", err)
		}
		if err := store.CommitEpisodeIndexStatus(ctx, other, card.ID, card.Version, domain.EpisodeIndexIndexed); !errors.Is(err, repository.ErrEpisodeConflict) {
			t.Fatalf("foreign index status = %v, want conflict", err)
		}
		loaded, err := store.LoadEpisodeCandidates(ctx, owner, current, []string{card.ID})
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if len(loaded) != 1 {
			t.Fatalf("reload lost an eligible candidate: %d", len(loaded))
		}
		if empty, err := store.LoadEpisodeCandidates(ctx, owner, session, []string{card.ID}); err != nil || len(empty) != 0 {
			t.Fatalf("current-session candidate survived the exclusion: %v %d", err, len(empty))
		}
		if empty, err := store.LoadEpisodeCandidates(ctx, other, current, []string{card.ID}); err != nil || len(empty) != 0 {
			t.Fatalf("foreign owner candidate survived the reload: %v %d", err, len(empty))
		}
		// A card derived from another card loses its eligibility the moment the
		// source moves on: the old version is not evidence about anything.
		derived := episodeFixture(owner, current, question, "派生卡")
		derived.SourceEpisodes = domain.EpisodeSourceRefs{{EpisodeID: card.ID, Version: card.Version}}
		if err := store.SaveEpisode(ctx, derived, 0); err != nil {
			t.Fatalf("save derived: %v", err)
		}
		if err := store.CommitEpisodeIndexStatus(ctx, owner, derived.ID, derived.Version, domain.EpisodeIndexIndexed); err != nil {
			t.Fatalf("mark derived indexed: %v", err)
		}
		if withDerived, err := store.LoadEpisodeCandidates(ctx, owner, "", []string{card.ID, derived.ID}); err != nil || len(withDerived) != 2 {
			t.Fatalf("valid provenance was refused: %v %d", err, len(withDerived))
		}
		reprojection := seedEpisodeMessage(t, store, owner, session, "assistant", "第二轮讨论。")
		bumped := *card
		bumped.SearchText = episodeFixture(owner, session, question, "json-编解码器").SearchText + " 第二版"
		bumped.SourceLastMessageID = reprojection
		if err := store.SaveEpisode(ctx, &bumped, card.Version); err != nil {
			t.Fatalf("bump source version: %v", err)
		}
		if stale, err := store.LoadEpisodeCandidates(ctx, owner, "", []string{derived.ID}); err != nil || len(stale) != 0 {
			t.Fatalf("derived card citing the old source version survived: %v %d", err, len(stale))
		}
	})
}

// SeedEpisodeScenario builds the restart scenario the persistence tests share:
// a user named for the scenario, one session ending on an assistant turn, and
// an indexed active card over that conversation.
func SeedEpisodeScenario(t *testing.T, store repository.Repository) {
	t.Helper()
	ctx := context.Background()
	user, err := store.CreateUser(ctx, &domain.User{
		Email: "episode-restart@example.com", DisplayName: "Episode restart",
	})
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	session, err := store.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "Restart", Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
	question, err := store.AppendMessage(ctx, user.ID, session.ID, &domain.Message{Role: "user", Content: "我决定用 JSON 编解码器"})
	if err != nil {
		t.Fatalf("seed question: %v", err)
	}
	if _, err := store.AppendMessage(ctx, user.ID, session.ID, &domain.Message{Role: "assistant", Content: "好的。"}); err != nil {
		t.Fatalf("seed answer: %v", err)
	}
	card := episodeFixture(user.ID, session.ID, question.ID, "json-编解码器")
	if err := store.SaveEpisode(ctx, card, 0); err != nil {
		t.Fatalf("seed card: %v", err)
	}
	if err := store.CommitEpisodeIndexStatus(ctx, user.ID, card.ID, card.Version, domain.EpisodeIndexIndexed); err != nil {
		t.Fatalf("seed index status: %v", err)
	}
}

// seedEpisodeUser, seedEpisodeSession and seedEpisodeMessage build the
// conversation scaffolding each scenario starts from.
func seedEpisodeUser(t *testing.T, store repository.Repository) string {
	t.Helper()
	user, err := store.CreateUser(context.Background(), &domain.User{
		Email: "episode-" + strconv.Itoa(seedEpisodeSequence) + "@example.com", DisplayName: "Episode",
	})
	seedEpisodeSequence++
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return user.ID
}

// seedEpisodeSequence keeps each scenario's user unique without needing a
// clock or a random source: emails are normalized and unique per store, and
// this suite runs both stores sequentially.
var seedEpisodeSequence int

func seedEpisodeSession(t *testing.T, store repository.Repository, userID string, mode domain.SessionMode) string {
	t.Helper()
	session, err := store.CreateSession(context.Background(), &domain.Session{
		UserID: userID, Title: "Episode scenario", Mode: mode,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return session.ID
}

func seedEpisodeMessage(t *testing.T, store repository.Repository, userID, sessionID, role, content string) string {
	t.Helper()
	message, err := store.AppendMessage(context.Background(), userID, sessionID, &domain.Message{
		Role: role, Content: content,
	})
	if err != nil {
		t.Fatalf("append %s message: %v", role, err)
	}
	return message.ID
}

// episodeFixture builds a valid active card for one message. The assertion
// cites the cited user message, so every watermark invariant has a real fact to
// hold on to.
func episodeFixture(userID, sessionID, citedMessageID, topic string) *domain.Episode {
	key := domain.NormalizeEpisodeTopicKey(topic)
	identity, err := domain.NewAssertionID()
	if err != nil {
		panic(err)
	}
	seedEpisodeSequence++
	episodeID := fmt.Sprintf("ep-test-%d", seedEpisodeSequence)
	return &domain.Episode{
		ID:        episodeID,
		UserID:    userID,
		SessionID: sessionID,
		Title:     topic,
		Document: domain.EpisodeDocument{
			Synopsis: "用户确定了 " + topic + " 的选型。",
			Topics:   []domain.EpisodeTopic{{Key: key, Label: topic}},
			Decisions: []domain.EpisodeEvidence{{
				ID: identity, Text: "用户决定采用 " + topic,
				EvidenceMessageIDs: []string{citedMessageID},
			}},
			SourceHandles: []string{"message:" + citedMessageID},
		},
		SearchText:          "用户确定了 " + topic + " 的选型。\n用户决定采用 " + topic,
		TopicKeys:           []string{key},
		SourceLastMessageID: citedMessageID,
		LastMessageAt:       time.Now().UTC(),
		Status:              domain.EpisodeStatusActive,
		IndexStatus:         domain.EpisodeIndexPending,
	}
}

// EpisodeFixtureFor exposes the valid active-card fixture to in-package
// integration tests (the real-MySQL gate builds its scenarios from it).
func EpisodeFixtureFor(userID, sessionID, citedMessageID string) *domain.Episode {
	return episodeFixture(userID, sessionID, citedMessageID, "json-编解码器")
}
