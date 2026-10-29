package agent

import (
	learningdao "GopherAI/dao/learning"
	"GopherAI/model"
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
)

type stubProgressReader struct {
	rows []model.TopicProgress
	err  error
	// lastLimit records what the provider asked the database for, so a test
	// can prove the read stays bounded.
	lastLimit int
	lastUser  int64
}

func (s *stubProgressReader) ListTopicProgress(_ context.Context, userID int64, limit int) ([]model.TopicProgress, error) {
	s.lastUser, s.lastLimit = userID, limit
	if s.err != nil {
		return nil, s.err
	}
	owned := make([]model.TopicProgress, 0, len(s.rows))
	for _, row := range s.rows {
		if row.UserID == userID {
			owned = append(owned, row)
		}
	}
	return owned, nil
}

type stubActiveLearning struct {
	session *model.LearningSession
	err     error
}

func (s stubActiveLearning) GetActive(context.Context, int64, string) (*model.LearningSession, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.session == nil {
		return nil, learningdao.ErrLearningSessionNotFound
	}
	return s.session, nil
}

func (s stubActiveLearning) GetQuestion(context.Context, int64, string, int) (*model.LearningQuestion, error) {
	return nil, learningdao.ErrLearningSessionNotFound
}

func progressRow(userID int64, topic string, score int, attempts int, level model.TopicProgressLevel, weak []string, lastAttempt *time.Time) model.TopicProgress {
	return model.TopicProgress{
		UserID:        userID,
		Topic:         topic,
		TopicKey:      model.NormalizeLearningTopicKey(topic),
		MasteryScore:  score,
		Confidence:    float64(attempts) / 6,
		ValidAttempts: attempts,
		Level:         level,
		WeakPoints:    weak,
		LastAttemptAt: lastAttempt,
	}
}

func newTestMasteryProvider(t *testing.T, reader TopicProgressReader, learning ActiveLearningReader) *TopicProgressMasteryProvider {
	t.Helper()
	provider, err := NewMasteryProvider(reader, learning, DefaultMasteryConfig())
	if err != nil {
		t.Fatalf("new mastery provider: %v", err)
	}
	provider.now = func() time.Time { return time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC) }
	return provider
}

func TestMasterySilentWithoutAssessment(t *testing.T) {
	provider := newTestMasteryProvider(t, &stubProgressReader{}, stubActiveLearning{})
	text, err := provider.CurrentMastery(context.Background(), RunContext{UserID: 11, SessionID: "s1", Question: "缓存穿透怎么办"})
	if err != nil {
		t.Fatalf("current mastery: %v", err)
	}
	if text != "" {
		t.Fatalf("expected no mastery text without progress, got %q", text)
	}
}

// The whole point of AGENT-017 is that a graded quiz changes how an ordinary
// question is answered, with no active learning session in sight.
func TestMasteryMatchesOrdinaryQuestionByTopic(t *testing.T) {
	recent := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	reader := &stubProgressReader{rows: []model.TopicProgress{
		progressRow(11, "Redis 缓存", 62, 4, model.TopicProgressDeveloping, []string{"缓存击穿与穿透的区别"}, &recent),
		progressRow(11, "Go 并发", 90, 5, model.TopicProgressMastered, nil, &recent),
	}}
	provider := newTestMasteryProvider(t, reader, stubActiveLearning{})

	text, err := provider.CurrentMastery(context.Background(), RunContext{UserID: 11, SessionID: "s1", Question: "缓存穿透怎么办"})
	if err != nil {
		t.Fatalf("current mastery: %v", err)
	}
	if !strings.Contains(text, "Redis 缓存") {
		t.Fatalf("expected the matched topic in %q", text)
	}
	if strings.Contains(text, "Go 并发") {
		t.Errorf("unrelated topic leaked into mastery text: %q", text)
	}
	if !strings.Contains(text, "缓存击穿与穿透的区别") {
		t.Errorf("weak point missing from mastery text: %q", text)
	}
	if reader.lastLimit != DefaultMasteryConfig().CandidateLimit {
		t.Errorf("database read limit = %d, want %d", reader.lastLimit, DefaultMasteryConfig().CandidateLimit)
	}
}

// A score reaching the prompt is a hard gate failure: the model could quote it
// back to the learner, or treat it as a fact rather than a teaching hint.
func TestMasteryNeverEmitsNumbers(t *testing.T) {
	recent := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	reader := &stubProgressReader{rows: []model.TopicProgress{
		progressRow(11, "Redis 缓存", 67, 5, model.TopicProgressDeveloping, []string{"布隆过滤器误判方向"}, &recent),
	}}
	provider := newTestMasteryProvider(t, reader, stubActiveLearning{})

	text, err := provider.CurrentMastery(context.Background(), RunContext{UserID: 11, SessionID: "s1", Question: "redis 缓存怎么设计"})
	if err != nil {
		t.Fatalf("current mastery: %v", err)
	}
	if text == "" {
		t.Fatal("expected mastery text for an assessed topic")
	}
	if digits := regexp.MustCompile(`[0-9]`).FindAllString(text, -1); len(digits) > 0 {
		t.Fatalf("mastery text leaked digits %v: %q", digits, text)
	}
	for _, banned := range []string{"score", "67", "confidence", "0.8"} {
		if strings.Contains(strings.ToLower(text), banned) {
			t.Errorf("mastery text leaked %q: %q", banned, text)
		}
	}
}

// Below the confidence floor the learning service itself calls the number
// unreliable, so the model must not act on it.
func TestMasteryInsufficientDoesNotAdjustDepth(t *testing.T) {
	recent := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	reader := &stubProgressReader{rows: []model.TopicProgress{
		progressRow(11, "Redis 缓存", 100, 1, model.TopicProgressInsufficient, []string{"不该被采信的薄弱点"}, &recent),
	}}
	provider := newTestMasteryProvider(t, reader, stubActiveLearning{})

	text, err := provider.CurrentMastery(context.Background(), RunContext{UserID: 11, SessionID: "s1", Question: "redis 缓存怎么设计"})
	if err != nil {
		t.Fatalf("current mastery: %v", err)
	}
	if !strings.Contains(text, "not enough assessment evidence") {
		t.Fatalf("expected an insufficient-evidence instruction, got %q", text)
	}
	if !strings.Contains(text, "default depth") {
		t.Errorf("expected depth to stay at default, got %q", text)
	}
	if !strings.Contains(text, "offer a short quiz") {
		t.Errorf("insufficient evidence should turn into a quiz suggestion, got %q", text)
	}
	if strings.Contains(text, "不该被采信的薄弱点") {
		t.Errorf("weak points from an unreliable assessment must not be used: %q", text)
	}
}

func TestMasteryPrefersActiveLearningTopic(t *testing.T) {
	recent := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	reader := &stubProgressReader{rows: []model.TopicProgress{
		progressRow(11, "Redis 缓存", 62, 4, model.TopicProgressDeveloping, nil, &recent),
		progressRow(11, "RPC 粘包", 55, 4, model.TopicProgressDeveloping, []string{"半包时缓冲区不能直接复位"}, &recent),
	}}
	active := &model.LearningSession{TopicKey: model.NormalizeLearningTopicKey("RPC 粘包"), Topic: "RPC 粘包"}
	provider := newTestMasteryProvider(t, reader, stubActiveLearning{session: active})

	// The question mentions Redis, but an active quiz names its own topic.
	text, err := provider.CurrentMastery(context.Background(), RunContext{UserID: 11, SessionID: "s1", Question: "redis 缓存怎么设计"})
	if err != nil {
		t.Fatalf("current mastery: %v", err)
	}
	if !strings.HasPrefix(firstTopicLine(text), "- topic \"RPC 粘包\"") {
		t.Fatalf("active learning topic should rank first, got %q", text)
	}
}

func TestMasteryIsolatedAcrossUsers(t *testing.T) {
	recent := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	reader := &stubProgressReader{rows: []model.TopicProgress{
		progressRow(99, "Redis 缓存", 40, 5, model.TopicProgressDeveloping, []string{"别人的薄弱点"}, &recent),
	}}
	provider := newTestMasteryProvider(t, reader, stubActiveLearning{})

	text, err := provider.CurrentMastery(context.Background(), RunContext{UserID: 11, SessionID: "s1", Question: "redis 缓存怎么设计"})
	if err != nil {
		t.Fatalf("current mastery: %v", err)
	}
	if text != "" {
		t.Fatalf("another user's mastery leaked: %q", text)
	}
	if reader.lastUser != 11 {
		t.Errorf("provider queried user %d, want 11", reader.lastUser)
	}
}

// "go" must not be found inside "google"; this mirrors the same trap that the
// memory tag vocabulary had to solve.
func TestMasteryDoesNotMatchOnSubstrings(t *testing.T) {
	recent := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	reader := &stubProgressReader{rows: []model.TopicProgress{
		progressRow(11, "Go", 80, 5, model.TopicProgressDeveloping, nil, &recent),
	}}
	provider := newTestMasteryProvider(t, reader, stubActiveLearning{})

	text, err := provider.CurrentMastery(context.Background(), RunContext{UserID: 11, SessionID: "s1", Question: "google 的搜索排序怎么做"})
	if err != nil {
		t.Fatalf("current mastery: %v", err)
	}
	if text != "" {
		t.Fatalf("substring match produced a false positive: %q", text)
	}
}

func TestMasteryMarksStaleAssessment(t *testing.T) {
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reader := &stubProgressReader{rows: []model.TopicProgress{
		progressRow(11, "Redis 缓存", 62, 4, model.TopicProgressDeveloping, nil, &old),
	}}
	provider := newTestMasteryProvider(t, reader, stubActiveLearning{})

	text, err := provider.CurrentMastery(context.Background(), RunContext{UserID: 11, SessionID: "s1", Question: "redis 缓存怎么设计"})
	if err != nil {
		t.Fatalf("current mastery: %v", err)
	}
	if !strings.Contains(text, "indicative rather than current") {
		t.Errorf("an old assessment should be marked stale, got %q", text)
	}
}

func TestMasteryPropagatesReaderFailure(t *testing.T) {
	reader := &stubProgressReader{err: errors.New("mysql down")}
	provider := newTestMasteryProvider(t, reader, stubActiveLearning{})

	// The provider reports the failure; ContextBuilder is the layer that
	// decides to degrade rather than fail the run.
	if _, err := provider.CurrentMastery(context.Background(), RunContext{UserID: 11, SessionID: "s1", Question: "redis"}); err == nil {
		t.Fatal("expected the reader failure to surface")
	}
}

func TestMasteryConfigValidation(t *testing.T) {
	base := DefaultMasteryConfig()
	if err := base.Validate(); err != nil {
		t.Fatalf("default mastery config invalid: %v", err)
	}
	invalid := []MasteryConfig{
		{CandidateLimit: 0, MaxTopics: 2, MinConfidentAttempts: 3, StaleAfter: time.Hour},
		{CandidateLimit: 50, MaxTopics: 0, MinConfidentAttempts: 3, StaleAfter: time.Hour},
		{CandidateLimit: 50, MaxTopics: 2, MinConfidentAttempts: 0, StaleAfter: time.Hour},
		{CandidateLimit: 50, MaxTopics: 2, MinConfidentAttempts: 3, StaleAfter: 0},
	}
	for i, config := range invalid {
		if err := config.Validate(); err == nil {
			t.Errorf("config %d should be rejected", i)
		}
	}
}

func firstTopicLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "- topic") {
			return line
		}
	}
	return ""
}
