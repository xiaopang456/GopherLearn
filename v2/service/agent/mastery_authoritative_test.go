package agent

import (
	learningdao "GopherAI/dao/learning"
	"GopherAI/model"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// masteryTestClock is the clock newTestMasteryProvider pins. Learning session
// fixtures are placed relative to it so expiry is a property of the fixture
// rather than of when the suite happens to run.
var masteryTestClock = time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)

// scopedActiveLearning honours the identity arguments the real DAO filters on.
// stubActiveLearning ignores them, which is fine for the tests it was written
// for and useless here: the questions this file asks are exactly "what happens
// when the row belongs to someone else".
type scopedActiveLearning struct {
	session *model.LearningSession
	err     error
	calls   atomic.Int32
}

func (s *scopedActiveLearning) GetActive(_ context.Context, userID int64, sessionID string) (*model.LearningSession, error) {
	s.calls.Add(1)
	if s.err != nil {
		return nil, s.err
	}
	if s.session == nil || s.session.UserID != userID || s.session.SessionID != sessionID {
		return nil, learningdao.ErrLearningSessionNotFound
	}
	return s.session, nil
}

func (s *scopedActiveLearning) GetQuestion(context.Context, int64, string, int) (*model.LearningQuestion, error) {
	return nil, learningdao.ErrLearningSessionNotFound
}

func learningSession(userID int64, sessionID, topic string, status model.LearningSessionStatus,
	expires time.Time) *model.LearningSession {
	return &model.LearningSession{
		ID: "l-1", UserID: userID, SessionID: sessionID, AgentRunID: "r-1",
		Topic: topic, TopicKey: model.NormalizeLearningTopicKey(topic),
		Status: status, CurrentQuestion: 1, TotalQuestions: 3, Version: 1, ExpiresAt: expires,
	}
}

// An in-force quiz in this very session is the one source the server vouches
// for, and it produces the key even when TopicProgress holds nothing at all -
// a learner on their first quiz has notes and no grades yet.
func TestActiveLearningSessionProducesAnAuthoritativeKey(t *testing.T) {
	learning := &scopedActiveLearning{session: learningSession(11, "s-1", "RPC 粘包",
		model.LearningSessionWaitingAnswer, masteryTestClock.Add(time.Hour))}
	provider := newTestMasteryProvider(t, &stubProgressReader{}, learning)

	_, topics, err := provider.CurrentMasteryWithTopicSources(context.Background(), RunContext{
		UserID: 11, SessionID: "s-1", Question: "这个我还是没搞懂",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(topics.Authoritative) != 1 || topics.Authoritative[0] != "rpc-粘包" {
		t.Fatalf("authoritative = %v", topics.Authoritative)
	}
	if len(topics.Matched) != 0 {
		t.Fatalf("an empty progress list produced matched keys: %v", topics.Matched)
	}
}

// Everything the server cannot vouch for. Each of these rows would still win
// the lexical contest with score 2 if it reached the matcher, which is exactly
// why the bootstrap gate is separate from it.
func TestAuthoritativeKeyRejectsEverythingButAnInForceSessionOfThisUser(t *testing.T) {
	cases := []struct {
		name    string
		session *model.LearningSession
		user    int64
		chat    string
	}{
		{"another user", learningSession(12, "s-1", "RPC 粘包", model.LearningSessionAsking,
			masteryTestClock.Add(time.Hour)), 11, "s-1"},
		{"another chat session", learningSession(11, "s-other", "RPC 粘包", model.LearningSessionAsking,
			masteryTestClock.Add(time.Hour)), 11, "s-1"},
		{"completed", learningSession(11, "s-1", "RPC 粘包", model.LearningSessionCompleted,
			masteryTestClock.Add(time.Hour)), 11, "s-1"},
		{"cancelled", learningSession(11, "s-1", "RPC 粘包", model.LearningSessionCancelled,
			masteryTestClock.Add(time.Hour)), 11, "s-1"},
		{"expired status", learningSession(11, "s-1", "RPC 粘包", model.LearningSessionExpired,
			masteryTestClock.Add(time.Hour)), 11, "s-1"},
		{"expired deadline", learningSession(11, "s-1", "RPC 粘包", model.LearningSessionAsking,
			masteryTestClock.Add(-time.Hour)), 11, "s-1"},
		{"unnormalisable topic key", learningSession(11, "s-1", "!!!", model.LearningSessionAsking,
			masteryTestClock.Add(time.Hour)), 11, "s-1"},
		{"no session at all", nil, 11, "s-1"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			provider := newTestMasteryProvider(t, &stubProgressReader{},
				&scopedActiveLearning{session: testCase.session})
			_, topics, err := provider.CurrentMasteryWithTopicSources(context.Background(), RunContext{
				UserID: testCase.user, SessionID: testCase.chat, Question: "RPC 粘包怎么处理",
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(topics.Authoritative) != 0 {
				t.Fatalf("authoritative = %v", topics.Authoritative)
			}
		})
	}
}

// A terminal session is the interesting one: it still matches, it still shapes
// the explanation, and it is still not authoritative. The two gates are not the
// same gate, and this is the case that proves it rather than asserting it.
func TestTerminalSessionStillMatchesButNeverBootstraps(t *testing.T) {
	recent := masteryTestClock.Add(-24 * time.Hour)
	reader := &stubProgressReader{rows: []model.TopicProgress{
		progressRow(11, "RPC 粘包", 80, 4, model.TopicProgressDeveloping, []string{"读半包"}, &recent),
	}}
	learning := &scopedActiveLearning{session: learningSession(11, "s-1", "RPC 粘包",
		model.LearningSessionCompleted, masteryTestClock.Add(time.Hour))}
	provider := newTestMasteryProvider(t, reader, learning)

	text, topics, err := provider.CurrentMasteryWithTopicSources(context.Background(), RunContext{
		UserID: 11, SessionID: "s-1", Question: "换个话题，说说 Redis 过期",
	})
	if err != nil {
		t.Fatal(err)
	}
	if text == "" {
		t.Fatal("a finished quiz stopped shaping the explanation")
	}
	if len(topics.Matched) != 1 || topics.Matched[0] != "rpc-粘包" {
		t.Fatalf("matched = %v", topics.Matched)
	}
	if len(topics.Authoritative) != 0 {
		t.Fatalf("a finished quiz produced an authoritative key: %v", topics.Authoritative)
	}
}

// stubTopicTexts serves the quiz vocabulary index from a fixture.
type stubTopicTexts struct {
	rows []learningdao.TopicAttemptRecord
}

func (s stubTopicTexts) ListRecentAttemptTexts(context.Context, int64, int) ([]learningdao.TopicAttemptRecord, error) {
	return s.rows, nil
}

// The two weaker matchers reach Matched and stop there. A question that merely
// names a topic, and a question that merely reuses wording out of that topic's
// quiz history, are both good enough to pick a teaching depth and neither is
// good enough to open the learner's notes unasked.
func TestLexicalAndQuizVocabularyMatchesStayOutOfAuthoritative(t *testing.T) {
	recent := masteryTestClock.Add(-24 * time.Hour)
	reader := &stubProgressReader{rows: []model.TopicProgress{
		progressRow(11, "go 并发", 60, 4, model.TopicProgressDeveloping, nil, &recent),
		progressRow(11, "redis ttl", 60, 4, model.TopicProgressDeveloping, nil, &recent),
		progressRow(11, "rabbitmq 确认", 60, 4, model.TopicProgressDeveloping, nil, &recent),
	}}
	provider := newTestMasteryProvider(t, reader, &scopedActiveLearning{})

	// Named outright: the coverage matcher carries it.
	_, byName, err := provider.CurrentMasteryWithTopicSources(context.Background(), RunContext{
		UserID: 11, SessionID: "s-1", Question: "redis ttl 是怎么回事",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(byName.Matched) == 0 || byName.Matched[0] != "redis-ttl" {
		t.Fatalf("matched = %v", byName.Matched)
	}
	if len(byName.Authoritative) != 0 {
		t.Fatalf("a lexical match became authoritative: %v", byName.Authoritative)
	}

	// Reached only through wording out of that topic's quiz history. The
	// question names neither "redis" nor "ttl", so the coverage matcher cannot
	// carry it and the term index is the only way in.
	provider.UseTopicTerms(stubTopicTexts{rows: []learningdao.TopicAttemptRecord{
		attemptText("go-并发", "goroutine 之间通过 channel 通信时阻塞行为有何不同？", "数据竞争"),
		attemptText("redis-ttl", "key 过期后是立刻删除吗？惰性删除与定期删除的触发时机？"),
		attemptText("rabbitmq-确认", "autoAck 为什么危险？"),
	}})
	_, byTerms, err := provider.CurrentMasteryWithTopicSources(context.Background(), RunContext{
		UserID: 11, SessionID: "s-1", Question: "惰性删除和定期删除的区别",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(byTerms.Matched) == 0 || byTerms.Matched[0] != "redis-ttl" {
		t.Fatalf("the quiz vocabulary did not carry the topic: %v", byTerms.Matched)
	}
	if len(byTerms.Authoritative) != 0 {
		t.Fatalf("a quiz-vocabulary hit became authoritative: %v", byTerms.Authoritative)
	}
}

// Splitting the interface must not split the query. Both halves come out of one
// read, so a build cannot end up with a mastery block about one topic and a
// bootstrap about another.
func TestOneBuildReadsTheActiveLearningSessionOnce(t *testing.T) {
	learning := &scopedActiveLearning{session: learningSession(11, "s-1", "RPC 粘包",
		model.LearningSessionAsking, masteryTestClock.Add(time.Hour))}
	provider := newTestMasteryProvider(t, &stubProgressReader{}, learning)
	builder, err := NewContextBuilder(memoryToolV2ContextConfig(), stubHistory{}, nil, provider, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	builder.UseGlobalMemoryProvider(&recordingMemoryProvider{})
	builder.UseAuthoritativeTopicMemoryProvider(&recordingTopicBootstrap{})

	if _, err := builder.Build(context.Background(), RunContext{
		UserID: 11, SessionID: "s-1", Question: "这个我还是没搞懂",
		MemoryEvidence: NewMemoryEvidenceRegistry(),
	}); err != nil {
		t.Fatal(err)
	}
	if got := learning.calls.Load(); got != 1 {
		t.Fatalf("one build read the active learning session %d times", got)
	}
}

// A failed read has not established a topic either. Dropping the mastery block
// and keeping a key that came out of the same call would be bootstrapping from
// something this turn could not confirm.
func TestAFailedMasteryReadYieldsNoAuthoritativeKey(t *testing.T) {
	learning := &scopedActiveLearning{err: errors.New("learning store down")}
	provider := newTestMasteryProvider(t, &stubProgressReader{}, learning)
	bootstrap := &recordingTopicBootstrap{}
	builder, err := NewContextBuilder(memoryToolV2ContextConfig(), stubHistory{}, nil, provider, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	builder.UseGlobalMemoryProvider(&recordingMemoryProvider{})
	builder.UseAuthoritativeTopicMemoryProvider(bootstrap)

	window, err := builder.Build(context.Background(), RunContext{
		UserID: 11, SessionID: "s-1", Question: "问题", MemoryEvidence: NewMemoryEvidenceRegistry(),
	})
	if err != nil {
		t.Fatalf("a degraded mastery read failed the turn: %v", err)
	}
	if len(window.AuthoritativeTopicKeys) != 0 {
		t.Fatalf("authoritative keys survived a failed read: %v", window.AuthoritativeTopicKeys)
	}
	if bootstrap.calls.Load() != 0 {
		t.Fatal("the topic reader ran with no confirmed key")
	}
}
