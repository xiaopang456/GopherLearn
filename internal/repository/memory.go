package repository

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// InMemoryRepository is a concurrency-safe Repository implementation. All
// entities are kept in memory and are lost when the process exits.
type InMemoryRepository struct {
	mu sync.RWMutex

	users        map[string]domain.User
	usersByEmail map[string]string
	sessions     map[string]domain.Session
	sessionOrder []string
	messages     map[string][]domain.Message
	messagesByID map[string]string
	// summaries is keyed by session id: one rolling summary per session, which is
	// the same uniqueness rule the SQL side enforces with a unique index.
	summaries map[string]domain.SessionSummary
	// topicMemory is keyed by session id for the same reason: the topic-memory consent is
	// a property of one conversation, mutable while the conversation is immutable. A
	// missing entry is "off", which is why nothing here stores a default row.
	topicMemory map[string]domain.TopicMemoryPreference
	// memories holds long-term memories by id, in no particular order, and memoryOrder
	// is the creation order that makes a memory listing deterministic. Long-term memory
	// deliberately survives a session delete: it is the one thing in this store whose
	// whole purpose is to outlive the conversation it came from.
	memories    map[string]domain.Memory
	memoryOrder []string
	// learningSessions is keyed by quiz id and learningSessionOrder is the order the
	// store received them in, which is what makes a listing deterministic without
	// sorting on an id whose lexical order differs between stores.
	learningSessions     map[string]domain.LearningSession
	learningSessionOrder []string
	// learningQuestions is keyed by quiz id, and each list is kept in ordinal order
	// as it arrives, so a read cannot reorder the questions a learner is asked.
	learningQuestions map[string][]domain.LearningQuestion
	// learningAttempts is keyed by attempt id; learningAttemptBySubmission maps
	// "owner and submission id" onto it, which is the in-memory shape of the unique
	// index the SQL side uses for idempotency.
	learningAttempts        map[string]domain.LearningAttempt
	learningAttemptOrder    []string
	learningAttemptBySubmit map[string]string
	learningProgress        map[string]domain.TopicProgress
	learningProgressOrder   []string
	// Episode state follows the same uniqueness rule the SQL side enforces with
	// a (user_id, session_id) index: one card and one durable job per session.
	episodes         map[string]domain.Episode
	episodeBySess    map[episodeSessionKey]string
	episodeJobs      map[string]domain.EpisodeProjectionJob
	episodeJobBySess map[episodeSessionKey]string
	sequence         uint64
}

// NewInMemoryRepository constructs an empty in-memory repository.
func NewInMemoryRepository() *InMemoryRepository {
	return &InMemoryRepository{
		users:        make(map[string]domain.User),
		usersByEmail: make(map[string]string),
		sessions:     make(map[string]domain.Session),
		messages:     make(map[string][]domain.Message),
		messagesByID: make(map[string]string),
		summaries:    make(map[string]domain.SessionSummary),
		topicMemory:  make(map[string]domain.TopicMemoryPreference),
		memories:     make(map[string]domain.Memory),

		learningSessions:        make(map[string]domain.LearningSession),
		learningQuestions:       make(map[string][]domain.LearningQuestion),
		learningAttempts:        make(map[string]domain.LearningAttempt),
		learningAttemptBySubmit: make(map[string]string),
		learningProgress:        make(map[string]domain.TopicProgress),
	}
}

// CreateUser stores user and returns a copy of the stored value. If ID or
// CreatedAt is empty, the repository supplies them. Email is unique when set.
func (r *InMemoryRepository) CreateUser(ctx context.Context, user *domain.User) (*domain.User, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if user == nil {
		return nil, ErrInvalidArgument
	}

	candidate := *user
	// Email is stored normalized so the uniqueness rule cannot be defeated by
	// case or surrounding space, and so this repository and the GORM one agree.
	candidate.Email = normalizeEmail(candidate.Email)
	generatedID := candidate.ID == ""
	if generatedID {
		candidate.ID = r.nextID("user")
	}
	if candidate.CreatedAt.IsZero() {
		candidate.CreatedAt = time.Now().UTC()
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.initLocked()
	for {
		if _, exists := r.users[candidate.ID]; !exists {
			break
		}
		if !generatedID {
			return nil, ErrConflict
		}
		candidate.ID = r.nextID("user")
	}
	emailKey := candidate.Email
	if emailKey != "" {
		if _, exists := r.usersByEmail[emailKey]; exists {
			return nil, ErrConflict
		}
	}
	r.users[candidate.ID] = candidate
	if emailKey != "" {
		r.usersByEmail[emailKey] = candidate.ID
	}
	return cloneUser(candidate), nil
}

// FindUserByEmail returns a copy of the user registered with email.
func (r *InMemoryRepository) FindUserByEmail(ctx context.Context, email string) (*domain.User, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	emailKey := normalizeEmail(email)
	if emailKey == "" {
		return nil, ErrInvalidArgument
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	userID, exists := r.usersByEmail[emailKey]
	if !exists {
		return nil, ErrNotFound
	}
	user, exists := r.users[userID]
	if !exists {
		return nil, ErrNotFound
	}
	return cloneUser(user), nil
}

// GetUser returns a copy of the user with userID.
func (r *InMemoryRepository) GetUser(ctx context.Context, userID string) (*domain.User, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" {
		return nil, ErrInvalidArgument
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	user, exists := r.users[userID]
	if !exists {
		return nil, ErrNotFound
	}
	return cloneUser(user), nil
}

// CreateSession stores a session and returns a copy of the stored value. The
// owning user must already exist. If ID or timestamps are empty, they are
// supplied by the repository.
func (r *InMemoryRepository) CreateSession(ctx context.Context, session *domain.Session) (*domain.Session, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if session == nil {
		return nil, ErrInvalidArgument
	}

	candidate := *session
	candidate.UserID = strings.TrimSpace(candidate.UserID)
	generatedID := candidate.ID == ""
	if candidate.UserID == "" || !candidate.Mode.IsValid() {
		return nil, ErrInvalidArgument
	}
	if generatedID {
		candidate.ID = r.nextID("session")
	}
	now := time.Now().UTC()
	if candidate.CreatedAt.IsZero() {
		candidate.CreatedAt = now
	}
	if candidate.UpdatedAt.IsZero() {
		candidate.UpdatedAt = candidate.CreatedAt
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.initLocked()
	if _, exists := r.users[candidate.UserID]; !exists {
		return nil, ErrNotFound
	}
	for {
		if _, exists := r.sessions[candidate.ID]; !exists {
			break
		}
		if !generatedID {
			return nil, ErrConflict
		}
		candidate.ID = r.nextID("session")
	}
	r.sessions[candidate.ID] = candidate
	r.sessionOrder = append(r.sessionOrder, candidate.ID)
	return cloneSession(candidate), nil
}

// ListSessions returns the owner's sessions in creation order.
func (r *InMemoryRepository) ListSessions(ctx context.Context, userID string) ([]domain.Session, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" {
		return nil, ErrInvalidArgument
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	if _, exists := r.users[userID]; !exists {
		return nil, ErrNotFound
	}

	result := make([]domain.Session, 0)
	for _, sessionID := range r.sessionOrder {
		session, exists := r.sessions[sessionID]
		if exists && session.UserID == userID {
			result = append(result, session)
		}
	}
	return cloneSessions(result), nil
}

// GetSession returns a copy of a session only when it belongs to userID.
func (r *InMemoryRepository) GetSession(ctx context.Context, userID, sessionID string) (*domain.Session, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(sessionID) == "" {
		return nil, ErrInvalidArgument
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	session, exists := r.sessions[sessionID]
	if !exists || session.UserID != userID {
		return nil, ErrNotFound
	}
	return cloneSession(session), nil
}

// DeleteSession deletes an owned session and all messages in it.
func (r *InMemoryRepository) DeleteSession(ctx context.Context, userID, sessionID string) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(sessionID) == "" {
		return ErrInvalidArgument
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.initLocked()
	session, exists := r.sessions[sessionID]
	if !exists || session.UserID != userID {
		return ErrNotFound
	}
	storedMessages := r.messages[sessionID]
	delete(r.sessions, sessionID)
	delete(r.messages, sessionID)
	// The rolling summary is a compression of this session's messages, so it has no
	// meaning once they are gone, and no other session may inherit its boundary id.
	delete(r.summaries, sessionID)
	// The topic-memory consent is a permission over this conversation, so it leaves with
	// the conversation: a surviving row would be a standing "yes" pointing at nothing, and
	// the next conversation the learner starts must not inherit it.
	delete(r.topicMemory, sessionID)
	// The episodic card and its projection job are derived from this
	// conversation and die with it, in the same critical section as the
	// session row: a card whose source session is gone must not outlive the
	// delete by even one crashed step. The derived retrieval view is cleaned
	// separately and best-effort by the caller - every read re-checks the
	// store, so a lagging view costs recall, never correctness.
	episodeState := r.episodeStateLocked()
	if episodeID, exists := episodeState.episodeBySess[episodeSessionKey{userID, sessionID}]; exists {
		delete(episodeState.episodes, episodeID)
		delete(episodeState.episodeBySess, episodeSessionKey{userID, sessionID})
	}
	if jobID, exists := episodeState.jobBySess[episodeSessionKey{userID, sessionID}]; exists {
		delete(episodeState.jobs, jobID)
		delete(episodeState.jobBySess, episodeSessionKey{userID, sessionID})
	}
	// r.memories is deliberately untouched. A long-term memory cites messages by id, and
	// several memories can cite the same conversation, so removing them here would delete
	// confirmed facts on the strength of a housekeeping action. The references stay, and
	// a later phase decides what a citation to a deleted session means.
	for index, storedID := range r.sessionOrder {
		if storedID == sessionID {
			r.sessionOrder = append(r.sessionOrder[:index], r.sessionOrder[index+1:]...)
			break
		}
	}
	for _, message := range storedMessages {
		delete(r.messagesByID, message.ID)
	}
	return nil
}

// AppendMessage atomically appends message to an owned session. Messages are
// returned and later listed in append order.
func (r *InMemoryRepository) AppendMessage(ctx context.Context, userID, sessionID string, message *domain.Message) (*domain.Message, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if message == nil {
		return nil, ErrInvalidArgument
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(sessionID) == "" {
		return nil, ErrInvalidArgument
	}

	candidate := *message
	candidate.SessionID = sessionID
	candidate.Sources = domain.CloneSources(message.Sources)
	generatedID := candidate.ID == ""
	if generatedID {
		candidate.ID = r.nextID("message")
	}
	if candidate.CreatedAt.IsZero() {
		candidate.CreatedAt = time.Now().UTC()
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.initLocked()
	session, exists := r.sessions[sessionID]
	if !exists || session.UserID != userID {
		return nil, ErrNotFound
	}
	for {
		if _, exists := r.messagesByID[candidate.ID]; !exists {
			break
		}
		if !generatedID {
			return nil, ErrConflict
		}
		candidate.ID = r.nextID("message")
	}
	r.messages[sessionID] = append(r.messages[sessionID], candidate)
	r.messagesByID[candidate.ID] = sessionID
	session.UpdatedAt = candidate.CreatedAt
	r.sessions[sessionID] = session
	return cloneMessage(candidate), nil
}

// ListMessages returns copies of the owned session's messages in append order.
func (r *InMemoryRepository) ListMessages(ctx context.Context, userID, sessionID string) ([]domain.Message, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(sessionID) == "" {
		return nil, ErrInvalidArgument
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	session, exists := r.sessions[sessionID]
	if !exists || session.UserID != userID {
		return nil, ErrNotFound
	}
	return cloneMessages(r.messages[sessionID]), nil
}

func (r *InMemoryRepository) nextID(kind string) string {
	sequence := atomic.AddUint64(&r.sequence, 1)
	return kind + "-" + strconv.FormatUint(sequence, 10)
}

func (r *InMemoryRepository) initLocked() {
	if r.users == nil {
		r.users = make(map[string]domain.User)
	}
	if r.usersByEmail == nil {
		r.usersByEmail = make(map[string]string)
	}
	if r.sessions == nil {
		r.sessions = make(map[string]domain.Session)
	}
	if r.messages == nil {
		r.messages = make(map[string][]domain.Message)
	}
	if r.messagesByID == nil {
		r.messagesByID = make(map[string]string)
	}
	if r.summaries == nil {
		r.summaries = make(map[string]domain.SessionSummary)
	}
	if r.topicMemory == nil {
		r.topicMemory = make(map[string]domain.TopicMemoryPreference)
	}
	if r.memories == nil {
		r.memories = make(map[string]domain.Memory)
	}
	if r.learningSessions == nil {
		r.learningSessions = make(map[string]domain.LearningSession)
	}
	if r.learningQuestions == nil {
		r.learningQuestions = make(map[string][]domain.LearningQuestion)
	}
	if r.learningAttempts == nil {
		r.learningAttempts = make(map[string]domain.LearningAttempt)
	}
	if r.learningAttemptBySubmit == nil {
		r.learningAttemptBySubmit = make(map[string]string)
	}
	if r.learningProgress == nil {
		r.learningProgress = make(map[string]domain.TopicProgress)
	}
	if r.episodes == nil {
		r.episodes = make(map[string]domain.Episode)
	}
	if r.episodeBySess == nil {
		r.episodeBySess = make(map[episodeSessionKey]string)
	}
	if r.episodeJobs == nil {
		r.episodeJobs = make(map[string]domain.EpisodeProjectionJob)
	}
	if r.episodeJobBySess == nil {
		r.episodeJobBySess = make(map[episodeSessionKey]string)
	}
}

func checkContext(ctx context.Context) error {
	if ctx == nil {
		return ErrInvalidArgument
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func cloneUser(user domain.User) *domain.User {
	copy := user
	return &copy
}

func cloneSession(session domain.Session) *domain.Session {
	copy := session
	return &copy
}

func cloneSessions(sessions []domain.Session) []domain.Session {
	copy := make([]domain.Session, len(sessions))
	for index := range sessions {
		copy[index] = sessions[index]
	}
	return copy
}

// cloneMessage detaches the citation slice as well as the struct: a caller that
// sorts or truncates the sources it was handed must not be able to rewrite what
// the repository stores.
func cloneMessage(message domain.Message) *domain.Message {
	copy := message
	copy.Sources = domain.CloneSources(message.Sources)
	return &copy
}

func cloneMessages(messages []domain.Message) []domain.Message {
	copy := make([]domain.Message, len(messages))
	for index := range messages {
		copy[index] = messages[index]
		copy[index].Sources = domain.CloneSources(messages[index].Sources)
	}
	return copy
}

// GetSessionSummary returns a copy of the session's rolling summary. An unknown
// session, a session owned by another user and a session that has never had a
// summary refreshed are all ErrNotFound, so this cannot be used to tell whether
// somebody else's conversation has been summarized.
func (r *InMemoryRepository) GetSessionSummary(ctx context.Context, userID, sessionID string) (*domain.SessionSummary, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(sessionID) == "" {
		return nil, ErrInvalidArgument
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	r.initLocked()
	session, exists := r.sessions[sessionID]
	if !exists || session.UserID != userID {
		return nil, ErrNotFound
	}
	stored, exists := r.summaries[sessionID]
	if !exists {
		return nil, ErrNotFound
	}
	clone := stored
	return &clone, nil
}

// CommitSessionSummary applies the version check the contract describes. The whole
// decision happens under the write lock, so a compare and a set cannot be split by a
// concurrent turn the way they are split by two clients of a real database.
//
// A missing session is ErrNotFound; a session owned by another user is ErrNotFound
// rather than ErrConflict, because an authorization failure must not be
// distinguishable from a missing row.
func (r *InMemoryRepository) CommitSessionSummary(ctx context.Context, summary *domain.SessionSummary, expectVersion int64) (*domain.SessionSummary, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if summary == nil || strings.TrimSpace(summary.UserID) == "" || strings.TrimSpace(summary.SessionID) == "" ||
		strings.TrimSpace(summary.Content) == "" || strings.TrimSpace(summary.CoveredMessageID) == "" ||
		expectVersion < 0 {
		return nil, ErrInvalidArgument
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.initLocked()
	session, exists := r.sessions[summary.SessionID]
	if !exists || session.UserID != summary.UserID {
		return nil, ErrNotFound
	}
	stored, existed := r.summaries[summary.SessionID]
	if existed && stored.Version != expectVersion {
		return nil, ErrConflict
	}
	if !existed && expectVersion != 0 {
		// Building on a version that is not there would mean the row was removed
		// while the refresh ran; that is the same race, reported the same way.
		return nil, ErrConflict
	}
	committed := *summary
	committed.SessionID = session.ID
	committed.UserID = session.UserID
	committed.Version = expectVersion + 1
	if committed.UpdatedAt.IsZero() {
		committed.UpdatedAt = time.Now().UTC()
	} else {
		committed.UpdatedAt = committed.UpdatedAt.UTC()
	}
	r.summaries[summary.SessionID] = committed
	clone := committed
	return &clone, nil
}
