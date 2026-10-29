package repository

import (
	"context"
	"errors"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// Episode storage errors. NotFound and Conflict follow the same owner-blinding
// rule as the rest of this package: a missing row and a row owned by somebody
// else are the same answer, so the store cannot be used to probe other users.
var (
	ErrEpisodeNotFound    = errors.New("repository: episode not found")
	ErrEpisodeConflict    = errors.New("repository: episode version conflict")
	ErrEpisodeJobNotFound = errors.New("repository: episode projection job not found")
	ErrEpisodeJobConflict = errors.New("repository: episode projection job conflict")
)

// EpisodeProjectionGap is one session whose stored conversation has moved past
// the Episode watermark. The reconciler turns gaps into projection jobs, which
// is how a publish that was lost - to a crash between the message write and the
// enqueue, for example - is repaired without a message queue.
type EpisodeProjectionGap struct {
	UserID          string
	SessionID       string
	LatestMessageID string
}

// EpisodeStore is the persistence half of the episodic memory system: one
// versioned card per session, plus the durable debounce-and-lease job rows the
// background worker claims.
//
// It lives beside the conversation methods rather than in a parallel store for
// the same reason the memory methods do: every operation needs the same three
// things - one ownership rule, one session-existence boundary, and one
// conformance suite both stores must pass - so a memory-mode process and a
// MySQL deployment cannot disagree about whether an episode is still readable.
//
// The watermark rule: message ids in this project compare lexicographically in
// creation order (domain.CompareMessageIDs). Every "already covered" decision
// goes through that comparison, never through timestamps.
type EpisodeStore interface {
	// GetEpisodeBySession returns the session's Episode only while its source
	// session exists and belongs to the caller. The session check is the privacy
	// boundary, not a cleanup optimization: a deleted session's card must not
	// answer a read even if a delete race left the row behind.
	GetEpisodeBySession(ctx context.Context, userID, sessionID string) (*domain.Episode, error)
	// GetEpisodeByID is the owner-scoped read that citation-time verification
	// uses: an id that does not belong to the caller is a miss, not a permission
	// question.
	GetEpisodeByID(ctx context.Context, userID, episodeID string) (*domain.Episode, error)
	// SaveEpisode publishes a new Episode (expectedVersion 0) or advances an
	// existing one under optimistic concurrency. An update must preserve the id
	// and move the complete-message watermark forward; anything else is
	// ErrEpisodeConflict and nothing is written.
	SaveEpisode(ctx context.Context, episode *domain.Episode, expectedVersion uint64) error
	// CommitEpisodeIndexStatus records whether the derived retrieval view for
	// this exact Episode version is in place. The version must match the stored
	// row: an index status that drifted onto a newer version would say nothing.
	CommitEpisodeIndexStatus(ctx context.Context, userID, episodeID string, version uint64, status domain.EpisodeIndexStatus) error
	// ListActiveEpisodesForReindex pages one user's active Episodes in id order,
	// which is how a retrieval view is rebuilt after an outage.
	ListActiveEpisodesForReindex(ctx context.Context, userID, afterID string, limit int) ([]domain.Episode, error)
	// ListAllActiveEpisodes pages every user's active Episodes in id order. The
	// process-local index uses it to warm itself at startup, because its content
	// dies with the process while the authoritative rows do not.
	ListAllActiveEpisodes(ctx context.Context, afterID string, limit int) ([]domain.Episode, error)
	// LoadEpisodeCandidates reloads candidate ids through the authority: owner,
	// active session, active status, indexed view, not the session the reader is
	// in. The returned rows follow the order of ids; candidates that fail any
	// check are simply absent. The provenance closure of each survivor is then
	// re-checked against live rows, because a derived card whose source version
	// no longer exists is not evidence.
	LoadEpisodeCandidates(ctx context.Context, userID, currentSession string, ids []string) ([]domain.Episode, error)
	// ListEpisodesByTime answers the time-anchored reads ("last session", a
	// date) straight from the authority. The fetch is deeper than the requested
	// limit so that filtering invalid provenance cannot turn "the newest valid
	// episode" into a false empty answer.
	ListEpisodesByTime(ctx context.Context, userID, currentSession string, from, to time.Time, limit int) ([]domain.Episode, error)
	// DeleteEpisodesBySession removes the session's Episode and projection-job
	// rows inside one critical section and returns the ids that were deleted,
	// so the caller can drop the same ids from the derived retrieval view.
	DeleteEpisodesBySession(ctx context.Context, userID, sessionID string) ([]string, error)
	// ListEpisodeProjectionGaps finds sessions whose latest assistant message is
	// newer than the stored Episode watermark. Only sessions that ended on a
	// complete assistant turn are reported: a conversation still waiting for its
	// answer is not a complete episode source.
	ListEpisodeProjectionGaps(ctx context.Context, limit int) ([]EpisodeProjectionGap, error)

	// UpsertEpisodeDesired is the durable debounce boundary. A higher desired
	// message revives terminal jobs and re-arms pending ones, but it never steals
	// a live lease: the current worker finishes its claimed target and the
	// completion requeues the row for the new tail.
	UpsertEpisodeDesired(ctx context.Context, userID, sessionID, desiredMessageID string, availableAt time.Time) (*domain.EpisodeProjectionJob, error)
	// ListDueEpisodeJobs returns claimable pending jobs for sessions that still
	// exist, oldest-available first.
	ListDueEpisodeJobs(ctx context.Context, limit, maxAttempts int, now time.Time) ([]domain.EpisodeProjectionJob, error)
	// ClaimEpisodeJob moves one pending job to processing with a fresh lease, or
	// reports ErrEpisodeJobConflict when the row is not claimable. The claim
	// freezes the target: the worker projects exactly ClaimedMessageID.
	ClaimEpisodeJob(ctx context.Context, userID, jobID string, now time.Time, lease time.Duration, maxAttempts int) (*domain.EpisodeProjectionJob, error)
	// CompleteOrRequeueEpisodeJob acknowledges exactly the claimed target. When
	// the desired watermark moved meanwhile, the same durable row returns to
	// pending with its attempts reset instead of losing the newly arrived tail.
	CompleteOrRequeueEpisodeJob(ctx context.Context, claimed *domain.EpisodeProjectionJob, coveredMessageID string, now time.Time) (*domain.EpisodeProjectionJob, error)
	// RetryEpisodeJob releases the lease with a closed-vocabulary error code. A
	// newer desired target restarts the budget; a target at its attempt ceiling
	// becomes failed; anything else returns to pending for a delayed retry.
	RetryEpisodeJob(ctx context.Context, claimed *domain.EpisodeProjectionJob, errorCode string, availableAt time.Time, maxAttempts int) (*domain.EpisodeProjectionJob, error)
	// RecoverExpiredEpisodeJobs reclaims processing rows whose lease has expired.
	// A worker that died on message 10 must not make message 20 inherit its
	// failure, so a newer desired target resets the attempt budget.
	RecoverExpiredEpisodeJobs(ctx context.Context, now time.Time, maxAttempts int) (int64, error)
	// ReleaseOtherEpisodeSessions makes this user's pending projections for
	// every other session immediately claimable: the quiet window exists so an
	// active conversation is not projected on every turn, and writing in a
	// different session is evidence the previous one is over.
	ReleaseOtherEpisodeSessions(ctx context.Context, userID, currentSessionID string, now time.Time) (int64, error)
}
