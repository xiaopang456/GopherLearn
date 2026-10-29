package service

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

// topicFixture is the recall fixture plus one learning-agent conversation per account. The
// base fixture builds chat sessions only, and the topic grant decides on exactly two things
// a chat session cannot provide: the session's mode and the quiz open on it.
type topicFixture struct {
	recallFixture
	learnerAgent     string
	learnerAgentMsg  string
	intruderAgent    string
	intruderAgentMsg string
}

func newTopicFixture(t *testing.T) topicFixture {
	t.Helper()
	fixture := topicFixture{recallFixture: newRecallFixture(t)}
	fixture.learnerAgent, fixture.learnerAgentMsg = fixture.openSession(
		t, domain.SessionModeLearningAgent, fixture.userID, "学习会话")
	fixture.intruderAgent, fixture.intruderAgentMsg = fixture.openSession(
		t, domain.SessionModeLearningAgent, fixture.otherUser, "别人的学习会话")
	return fixture
}

// openSession creates one conversation of the given mode with a real user message inside it,
// so a memory filed from it cites provenance that exists rather than an invented id.
func (f topicFixture) openSession(t *testing.T, mode domain.SessionMode, userID, title string) (string, string) {
	t.Helper()
	ctx := t.Context()
	session, err := f.store.CreateSession(ctx, &domain.Session{UserID: userID, Title: title, Mode: mode})
	if err != nil {
		t.Fatalf("create %s session: %v", mode, err)
	}
	message, err := f.store.AppendMessage(ctx, userID, session.ID, &domain.Message{Role: "user", Content: "AOF 的 fsync 该选哪个"})
	if err != nil {
		t.Fatalf("append to %s session: %v", mode, err)
	}
	return session.ID, message.ID
}

// openQuiz files a quiz on one conversation straight through the store. Going through the
// store rather than LearningService.Start is deliberate: Start asks a model for questions,
// and a grant test must not depend on a provider or on the evidence selector finding text.
// The store still enforces its own rules - the conversation must exist and belong to the
// caller, and only one quiz may be open on it.
func (f topicFixture) openQuiz(t *testing.T, userID, sessionID, topic string, expiresAt time.Time) *domain.LearningSession {
	t.Helper()
	quiz, err := f.store.CreateLearningSession(t.Context(), &domain.LearningSession{
		UserID: userID, SessionID: sessionID, Topic: topic,
		TopicKey:        domain.NormalizeLearningTopicKey(topic),
		Status:          domain.LearningSessionCreated,
		CurrentQuestion: domain.LearningQuestionOrdinalFirst,
		TotalQuestions:  domain.LearningQuestionCount,
		Version:         1,
		ExpiresAt:       expiresAt,
	})
	if err != nil {
		t.Fatalf("open quiz on %s: %v", sessionID, err)
	}
	return quiz
}

// closeQuiz moves a quiz to a terminal state through the store's transition table, so the
// test exercises a genuinely finished row rather than a hand-written status column.
func (f topicFixture) closeQuiz(t *testing.T, userID string, quiz *domain.LearningSession) {
	t.Helper()
	closed, err := f.store.TransitionLearningSession(t.Context(), userID, quiz.ID,
		domain.LearningSessionCreated, domain.LearningSessionCancelled, quiz.Version, repository.LearningLease{})
	if err != nil {
		t.Fatalf("cancel quiz %s: %v", quiz.ID, err)
	}
	*quiz = *closed
}

// plantConsentIgnoringMode writes a consent row straight through the store, bypassing the
// service rule that only a learning-agent conversation may hold one. It exists for exactly one
// purpose: to show that the recall side refuses a chat conversation on its own, without needing
// the write side to have been perfect. A permission that only holds while every caller behaves is
// not a permission boundary.
func (f topicFixture) plantConsentIgnoringMode(t *testing.T, userID, sessionID string, enabled bool) {
	t.Helper()
	if _, err := f.store.CommitTopicMemoryPreference(t.Context(), userID, sessionID, enabled); err != nil {
		t.Fatalf("plant consent on %s: %v", sessionID, err)
	}
}

// topicAccess wires the topic authorizer over the same store the memories live in.
func (f topicFixture) topicAccess(t *testing.T, options MemorySearchOptions) *MemoryAccess {
	t.Helper()
	grant, err := NewLearningTopicGrant(f.store)
	if err != nil {
		t.Fatal(err)
	}
	return f.accessWith(t, grant, options)
}

// switchTopicMemory records the learner's own decision through the service that a request would
// reach, rather than planting a row straight into the store. Two reasons: the mode rule is a
// service rule and a test that skipped the service would never see it, and the recall path is
// then reading exactly the state the write path can produce.
func (f topicFixture) switchTopicMemory(t *testing.T, userID, sessionID string, enabled bool) {
	t.Helper()
	committed, err := NewSessionService(f.store).CommitTopicMemory(t.Context(), userID, sessionID, enabled)
	if err != nil {
		t.Fatalf("commit topic memory for %s: %v", sessionID, err)
	}
	if committed.Enabled != enabled {
		t.Fatalf("the switch was committed as %v, want %v", committed.Enabled, enabled)
	}
}

// studyOnOneSubject makes a topic read lawful in the way the product says it is: a live quiz on
// this conversation, and the learner's own decision to let this conversation read topic memory.
// A test that needs only one of the two calls the helper for that one, and names what it is
// leaving off.
func (f topicFixture) studyOnOneSubject(t *testing.T, topic string) {
	t.Helper()
	f.openQuiz(t, f.userID, f.learnerAgent, topic, time.Now().Add(time.Hour))
	f.switchTopicMemory(t, f.userID, f.learnerAgent, true)
}

func (f topicFixture) defaultAccess(t *testing.T, options MemorySearchOptions) *MemoryAccess {
	t.Helper()
	return f.accessWith(t, SessionMemoryGrant{}, options)
}

func (f topicFixture) accessWith(t *testing.T, authorizer MemoryAuthorizer, options MemorySearchOptions) *MemoryAccess {
	t.Helper()
	access, err := NewMemoryAccess(f.store, authorizer, options)
	if err != nil {
		t.Fatal(err)
	}
	return access
}

// recall runs one read and fails the test on an error, which is right for the tests below:
// each of them is about which rows come back, not about whether the store answered.
func recall(t *testing.T, access *MemoryAccess, userID, sessionID, query string) ([]MemoryHit, string) {
	t.Helper()
	hits, note, err := access.Recall(t.Context(), MemoryQuery{UserID: userID, SessionID: sessionID, Query: query})
	if err != nil {
		t.Fatalf("recall for %s in %s: %v", userID, sessionID, err)
	}
	return hits, note
}

func memoryIDs(hits []MemoryHit) string {
	ids := make([]string, 0, len(hits))
	for _, hit := range hits {
		ids = append(ids, hit.Memory.MemoryID)
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

// recalled compares ids rather than searching the joined list: one memory id is a prefix of
// another as soon as a fixture passes ten rows, and a membership test that quietly matches a
// prefix reads as a pass.
func recalled(hits []MemoryHit, memoryID string) bool {
	for _, hit := range hits {
		if hit.Memory.MemoryID == memoryID {
			return true
		}
	}
	return false
}

func contents(hits []MemoryHit) string {
	parts := make([]string, 0, len(hits))
	for _, hit := range hits {
		parts = append(parts, hit.Content)
	}
	return strings.Join(parts, "\n")
}

// sameReference compares two server-internal handles. It spells out the fields instead of
// using == because MemoryReference carries the source message ids, and a slice makes a
// struct incomparable - which is precisely the field a widened grant would have to change.
func sameReference(left, right MemoryReference) bool {
	return left.MemoryID == right.MemoryID &&
		left.Scope == right.Scope &&
		left.TopicKey == right.TopicKey &&
		left.SourceSessionID == right.SourceSessionID &&
		left.Confidence == right.Confidence &&
		strings.Join(left.SourceMessageIDs, ",") == strings.Join(right.SourceMessageIDs, ",") &&
		left.Citation == right.Citation
}

// wideOptions raises the result cap so a scope test sees every authorized match instead of
// the three best-scoring ones. It stays inside Normalize's ceiling on purpose: a test that
// asked for an unbounded read would be testing a configuration production cannot have.
func wideOptions() MemorySearchOptions {
	return MemorySearchOptions{MaxResults: 10}
}

// seedTopicMemories files one memory per layer, all matching "fsync", and returns the ids in
// a struct so a test can name what it expects rather than rebuilding the list.
type topicMemories struct {
	redisTopic   *domain.Memory
	mysqlTopic   *domain.Memory
	global       *domain.Memory
	episodeHere  *domain.Memory
	episodeThere *domain.Memory
	foreignRedis *domain.Memory
}

func (f topicFixture) seedTopicMemories(t *testing.T) topicMemories {
	t.Helper()
	return topicMemories{
		redisTopic: f.file(t, f.learnerAgent, f.learnerAgentMsg,
			topicDraft("redis", "redis 主题：AOF 的 fsync 选 everysec", 1), true),
		mysqlTopic: f.file(t, f.learnerAgent, f.learnerAgentMsg,
			topicDraft("mysql", "mysql 主题：binlog 的 fsync 已确认", 1), true),
		global: f.file(t, f.learnerAgent, f.learnerAgentMsg,
			globalDraft("全局：解释 fsync 时用中文", 1), true),
		episodeHere: f.file(t, f.learnerAgent, f.learnerAgentMsg,
			episodeDraft("本轮决定先掌握 fsync", 1), true),
		episodeThere: f.file(t, f.secondSess, f.secondMsg,
			episodeDraft("另一会话里的 fsync 决定", 1), true),
		foreignRedis: f.fileForeign(t, topicDraft("redis", "别人的 redis fsync，绝不可见", 1)),
	}
}

func TestTopicMemoryIsInvisibleOnTheDefaultAuthorizer(t *testing.T) {
	fixture := newTopicFixture(t)
	memories := fixture.seedTopicMemories(t)
	fixture.studyOnOneSubject(t, "redis")

	// The production wiring, with everything a topic read would otherwise need: an open quiz on
	// a learning-agent conversation and the learner's own yes. Neither of them is a reason the
	// default authorizer reads a topic - only installing a different authorizer is, and that is a
	// decision made in the assembly, not in the data.
	defaultAccess := fixture.defaultAccess(t, wideOptions())
	hits, note := recall(t, defaultAccess, fixture.userID, fixture.learnerAgent, "fsync")
	if want := memoryIDs([]MemoryHit{{Memory: *memories.global}, {Memory: *memories.episodeHere}}); memoryIDs(hits) != want {
		t.Fatalf("default authorizer hits = %s, want %s", memoryIDs(hits), want)
	}
	if strings.Contains(contents(hits), "redis 主题") {
		t.Fatalf("the default grant leaked topic memory: %s", contents(hits))
	}
	if strings.Contains(note, "已授权主题") {
		t.Fatalf("the scope note claimed a topic read that did not happen: %q", note)
	}
}

func TestTopicGrantReadsOnlyTheLiveQuizTopic(t *testing.T) {
	fixture := newTopicFixture(t)
	memories := fixture.seedTopicMemories(t)
	fixture.studyOnOneSubject(t, "redis")

	hits, note := recall(t, fixture.topicAccess(t, wideOptions()), fixture.userID, fixture.learnerAgent, "fsync")

	// Three layers at once: the quiz's own topic, plus everything the default grant already
	// covered. Opting in adds a subject; it does not replace the base scope.
	want := memoryIDs([]MemoryHit{{Memory: *memories.redisTopic}, {Memory: *memories.global}, {Memory: *memories.episodeHere}})
	if got := memoryIDs(hits); got != want {
		t.Fatalf("opted-in hits = %s, want %s", got, want)
	}
	for _, forbidden := range []string{"mysql 主题", "另一会话里的 fsync 决定", "别人的 redis fsync"} {
		if strings.Contains(contents(hits), forbidden) {
			t.Fatalf("the grant reached %q: %s", forbidden, contents(hits))
		}
	}
	if !strings.Contains(note, "已授权主题的记忆（redis）") {
		t.Fatalf("the scope note must name the one authorized subject: %q", note)
	}
}

// TestTopicGrantRequiresLearningAgentMode covers the two conversations that must stay shut
// even with the opt-in installed: an ordinary chat, and a knowledge Q&A, which is the same
// mode with a per-turn option rather than a mode of its own.
func TestTopicGrantRequiresLearningAgentMode(t *testing.T) {
	fixture := newTopicFixture(t)
	fixture.seedTopicMemories(t)

	// A quiz and a consent row filed straight onto chat conversations. The store allows both -
	// the mode rule is a service rule and a storage rule for neither - so these are exactly the
	// rows a grant that looked only at "is there a quiz" or "did anybody say yes" would wrongly
	// trust. The recall side has to refuse them on its own.
	fixture.openQuiz(t, fixture.userID, fixture.sessionID, "redis", time.Now().Add(time.Hour))
	fixture.openQuiz(t, fixture.userID, fixture.secondSess, "mysql", time.Now().Add(time.Hour))
	fixture.plantConsentIgnoringMode(t, fixture.userID, fixture.sessionID, true)
	fixture.plantConsentIgnoringMode(t, fixture.userID, fixture.secondSess, true)

	access := fixture.topicAccess(t, wideOptions())
	for name, sessionID := range map[string]string{
		"chat":              fixture.sessionID,
		"knowledge chat":    fixture.secondSess,
		"no session at all": "",
	} {
		query := MemoryQuery{UserID: fixture.userID, SessionID: sessionID, Query: "fsync"}
		hits, note, err := access.Recall(t.Context(), query)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, hit := range hits {
			if hit.Memory.Scope == domain.MemoryScopeTopic {
				t.Fatalf("%s: a non-learning conversation read topic memory %q", name, hit.Memory.Content)
			}
		}
		if strings.Contains(note, "已授权主题") {
			t.Fatalf("%s: scope note claimed a topic read: %q", name, note)
		}
	}
}

func TestTopicGrantRequiresALiveQuiz(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T, f topicFixture)
		want    bool
	}{
		{"no quiz was ever started", func(t *testing.T, f topicFixture) {
			f.switchTopicMemory(t, f.userID, f.learnerAgent, true)
		}, false},
		{"quiz past its deadline", func(t *testing.T, f topicFixture) {
			f.openQuiz(t, f.userID, f.learnerAgent, "redis", time.Now().Add(-time.Hour))
			f.switchTopicMemory(t, f.userID, f.learnerAgent, true)
		}, false},
		{"quiz the learner cancelled", func(t *testing.T, f topicFixture) {
			quiz := f.openQuiz(t, f.userID, f.learnerAgent, "redis", time.Now().Add(time.Hour))
			f.closeQuiz(t, f.userID, quiz)
			f.switchTopicMemory(t, f.userID, f.learnerAgent, true)
		}, false},
		{"quiz still open, learner said yes", func(t *testing.T, f topicFixture) {
			f.studyOnOneSubject(t, "redis")
		}, true},
		{"quiz still open, learner never asked", func(t *testing.T, f topicFixture) {
			f.openQuiz(t, f.userID, f.learnerAgent, "redis", time.Now().Add(time.Hour))
		}, false},
		{"learner said yes then took it back", func(t *testing.T, f topicFixture) {
			f.studyOnOneSubject(t, "redis")
			f.switchTopicMemory(t, f.userID, f.learnerAgent, false)
		}, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newTopicFixture(t)
			seeded := fixture.seedTopicMemories(t)
			testCase.prepare(t, fixture)
			hits, _ := recall(t, fixture.topicAccess(t, wideOptions()),
				fixture.userID, fixture.learnerAgent, "fsync")
			found := recalled(hits, seeded.redisTopic.MemoryID)
			if found != testCase.want {
				t.Fatalf("topic memory visible = %v, want %v; hits = %s", found, testCase.want, memoryIDs(hits))
			}
			// A quiz that grants no topic must not remove the layers that were already
			// readable either, or "not in a quiz" would silently disable the user's own
			// global facts.
			if !recalled(hits, seeded.global.MemoryID) || !recalled(hits, seeded.episodeHere.MemoryID) {
				t.Fatalf("the base grant stopped covering its own layers: %s", memoryIDs(hits))
			}
		})
	}
}

// TestForgedIdentityDoesNotWidenTheTopicGrant is the reason the grant re-reads ownership
// instead of trusting the session id it was handed. The turn's identity is server-supplied
// today, but the authorizer is the last place that can check it, so it checks.
func TestForgedIdentityDoesNotWidenTheTopicGrant(t *testing.T) {
	fixture := newTopicFixture(t)
	fixture.seedTopicMemories(t)
	fixture.openQuiz(t, fixture.userID, fixture.learnerAgent, "redis", time.Now().Add(time.Hour))
	fixture.openQuiz(t, fixture.otherUser, fixture.intruderAgent, "mysql", time.Now().Add(time.Hour))
	fixture.switchTopicMemory(t, fixture.userID, fixture.learnerAgent, true)
	fixture.switchTopicMemory(t, fixture.otherUser, fixture.intruderAgent, true)

	access := fixture.topicAccess(t, wideOptions())

	// (a) The intruder names the owner's learning-agent conversation. Ownership fails, so the
	// mode is never even asked, and no topic is granted - to either account.
	hits, _ := recall(t, access, fixture.otherUser, fixture.learnerAgent, "fsync")
	if strings.Contains(contents(hits), "redis 主题") || strings.Contains(contents(hits), "别人的 redis") {
		t.Fatalf("a forged session id reached a topic read: %s", contents(hits))
	}

	// (b) The owner names the intruder's learning-agent conversation, which does have a live
	// quiz on "mysql". Leaking that key would expose the owner's own mysql row through a
	// session they do not own.
	hits, _ = recall(t, access, fixture.userID, fixture.intruderAgent, "fsync")
	for _, hit := range hits {
		if hit.Memory.Scope == domain.MemoryScopeTopic {
			t.Fatalf("a foreign conversation granted topic memory: %q", hit.Memory.Content)
		}
	}

	// (c) A request that says the other subject's name out loud still cannot read it. The
	// query text is a search, not a scope.
	hits, _ = recall(t, access, fixture.userID, fixture.learnerAgent, "mysql 的 fsync")
	for _, hit := range hits {
		if hit.Memory.TopicKey == "mysql" {
			t.Fatalf("naming a topic in the query authorized it: %q", hit.Memory.Content)
		}
	}

	// (d) There is no "all users" in this read: the same topic key confirmed by another
	// account is never reachable, in any of the four combinations of owner and session.
	for _, pair := range [][2]string{
		{fixture.userID, fixture.learnerAgent},
		{fixture.userID, fixture.sessionID},
		{fixture.otherUser, fixture.intruderAgent},
		{fixture.otherUser, fixture.intruderAgent},
	} {
		hits, _ := recall(t, access, pair[0], pair[1], "fsync")
		for _, hit := range hits {
			if hit.Memory.UserID != pair[0] {
				t.Fatalf("read crossed accounts: %+v", hit.Memory)
			}
			if strings.Contains(hit.Content, "绝不可见") {
				t.Fatalf("another account's row reached this read: %q", hit.Content)
			}
		}
	}
}

// TestTopicGrantUsesTheNormalizedKeyOnly pins which of the two spellings is the join label.
// A learner types "Redis 持久化", the store files the quiz under "redis-持久化", and only a
// memory filed under that same normalized key is reachable - a row filed under the display
// text, or under a neighbouring subject, stays unread. The display string is the learner's
// own words and the key is the server's, and only the second one may decide a scope.
func TestTopicGrantUsesTheNormalizedKeyOnly(t *testing.T) {
	fixture := newTopicFixture(t)
	fixture.studyOnOneSubject(t, "Redis 持久化")
	typed := fixture.file(t, fixture.learnerAgent, fixture.learnerAgentMsg,
		topicDraft("Redis 持久化", "按学习者原话存的 fsync 结论", 1), true)
	sibling := fixture.file(t, fixture.learnerAgent, fixture.learnerAgentMsg,
		topicDraft("rdb-aof", "相邻主题的 fsync 结论", 1), true)
	normalized := fixture.file(t, fixture.learnerAgent, fixture.learnerAgentMsg,
		topicDraft("redis-持久化", "归一化键下的 fsync 结论", 1), true)

	hits, note := recall(t, fixture.topicAccess(t, wideOptions()), fixture.userID, fixture.learnerAgent, "fsync")
	if !recalled(hits, normalized.MemoryID) {
		t.Fatalf("the row under the quiz's own normalized key is missing: %s", memoryIDs(hits))
	}
	for _, hidden := range []*domain.Memory{typed, sibling} {
		if recalled(hits, hidden.MemoryID) {
			t.Fatalf("a neighbouring or unnormalized key %q was reached: %q", hidden.TopicKey, hidden.Content)
		}
	}
	if !strings.Contains(note, "已授权主题的记忆（redis-持久化）") {
		t.Fatalf("the note must name the key that was actually authorized: %q", note)
	}
}

// TestTopicGrantKeepsEveryOtherFilter checks that authorizing a subject is not the same act
// as authorizing a row. One topic key can only hold one active memory - confirming a second
// supersedes the first - so the four lifecycle states are produced in the order the store
// defines them, and the store itself is asked what it still holds before the recall is
// blamed for hiding it.
func TestTopicGrantKeepsEveryOtherFilter(t *testing.T) {
	ctx := t.Context()
	fixture := newTopicFixture(t)
	fixture.studyOnOneSubject(t, "redis")

	pending := fixture.file(t, fixture.learnerAgent, fixture.learnerAgentMsg,
		topicDraft("redis", "还没确认的 fsync 候选", 1), false)
	superseded := fixture.file(t, fixture.learnerAgent, fixture.learnerAgentMsg,
		topicDraft("redis", "被替代的 fsync 结论", 1), true)
	archived := fixture.file(t, fixture.learnerAgent, fixture.learnerAgentMsg,
		topicDraft("redis", "已归档的 fsync 结论", 1), true)
	if _, err := fixture.service.ArchiveMemory(ctx, fixture.userID, archived.MemoryID); err != nil {
		t.Fatal(err)
	}
	current := fixture.file(t, fixture.learnerAgent, fixture.learnerAgentMsg,
		topicDraft("redis", "现行的 fsync 结论", 1), true)

	active, err := fixture.service.ActiveMemories(ctx, fixture.userID, domain.MemoryScopeTopic, "redis")
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].MemoryID != current.MemoryID {
		t.Fatalf("the store itself does not hold the one active row this test describes: %+v", active)
	}

	hits, _ := recall(t, fixture.topicAccess(t, wideOptions()), fixture.userID, fixture.learnerAgent, "fsync")
	if !recalled(hits, current.MemoryID) {
		t.Fatalf("the one active, confident row is missing from %s", memoryIDs(hits))
	}
	for _, hidden := range []*domain.Memory{pending, superseded, archived} {
		if recalled(hits, hidden.MemoryID) {
			t.Fatalf("a row the store no longer treats as active was recalled: %q", hidden.Content)
		}
	}

	// The floor still applies inside the authorized subject: here the only active row under
	// the quiz's topic key is a 0.59 guess, and being confirmed is not the same as being
	// good enough to put in front of a model.
	guessFixture := newTopicFixture(t)
	guessFixture.studyOnOneSubject(t, "thin")
	guess := guessFixture.file(t, guessFixture.learnerAgent, guessFixture.learnerAgentMsg,
		topicDraft("thin", "把握不足的 fsync 猜测", 0.59), true)
	stored, err := guessFixture.service.ActiveMemories(ctx, guessFixture.userID, domain.MemoryScopeTopic, "thin")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].MemoryID != guess.MemoryID {
		t.Fatalf("the low-confidence row is not the store's active one: %+v", stored)
	}
	guessHits, note := recall(t, guessFixture.topicAccess(t, wideOptions()),
		guessFixture.userID, guessFixture.learnerAgent, "fsync")
	for _, hit := range guessHits {
		if hit.Memory.MemoryID == guess.MemoryID {
			t.Fatalf("a 0.59 memory cleared the 0.6 floor because its topic was authorized")
		}
	}
	if !strings.Contains(note, "已授权主题的记忆（thin）") {
		t.Fatalf("the subject was authorized but the note does not say so: %q", note)
	}

	// And a request that asks for a looser floor does not get one: authorization opens a
	// subject, it does not re-open rows the 0.6 default had already set aside.
	lowered, _, err := guessFixture.topicAccess(t, wideOptions()).Recall(ctx, MemoryQuery{
		UserID: guessFixture.userID, SessionID: guessFixture.learnerAgent, Query: "fsync", MinConfidence: 0.3,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, hit := range lowered {
		if hit.Memory.MemoryID == guess.MemoryID {
			t.Fatalf("a 0.3 request floor loosened the 0.6 default on the opted-in path")
		}
	}
}

// TestTopicGrantLeavesTheDefaultReadByteIdentical is the compatibility proof. Where no topic
// is authorized, the opt-in authorizer must produce the same hits, in the same order, with
// the same scope note and the same reference fields as SessionMemoryGrant - so a deployment
// can install it without re-checking every memory behaviour it already relies on.
func TestTopicGrantLeavesTheDefaultReadByteIdentical(t *testing.T) {
	fixture := newTopicFixture(t)
	fixture.seedTopicMemories(t)
	// A live quiz exists, but on a conversation this loop does not read, except as the one
	// case where the grant is supposed to fire.
	fixture.openQuiz(t, fixture.userID, fixture.sessionID, "redis", time.Now().Add(time.Hour))

	accessOn := fixture.topicAccess(t, wideOptions())
	accessOff := fixture.defaultAccess(t, wideOptions())
	for _, sessionID := range []string{fixture.sessionID, fixture.secondSess, fixture.intruderAgent, "", "   "} {
		for _, query := range []string{"fsync", "binlog", "中文"} {
			// Only the owner's own sessions are compared as the owner; the intruder id keeps
			// the loop honest about a session the caller does not own.
			on, noteOn := recall(t, accessOn, fixture.userID, sessionID, query)
			off, noteOff := recall(t, accessOff, fixture.userID, sessionID, query)
			if memoryIDs(on) != memoryIDs(off) {
				t.Fatalf("user=%s session=%q query=%q: hits differ %s vs %s",
					fixture.userID, sessionID, query, memoryIDs(on), memoryIDs(off))
			}
			if noteOn != noteOff {
				t.Fatalf("session=%q query=%q: scope note differs\n on %q\noff %q", sessionID, query, noteOn, noteOff)
			}
			for index := range on {
				if on[index].Content != off[index].Content || on[index].Score != off[index].Score {
					t.Fatalf("session=%q query=%q hit %d: rendered text or score differs", sessionID, query, index)
				}
				if !sameReference(on[index].Reference, off[index].Reference) {
					t.Fatalf("session=%q query=%q hit %d: reference differs\n on %+v\noff %+v",
						sessionID, query, index, on[index].Reference, off[index].Reference)
				}
			}
		}
	}
}

func TestTopicGrantReferenceFollowsTheRecalledRow(t *testing.T) {
	fixture := newTopicFixture(t)
	memories := fixture.seedTopicMemories(t)
	fixture.studyOnOneSubject(t, "redis")
	access := fixture.topicAccess(t, wideOptions())

	// The citation is produced from the stored row, so an opted-in topic memory is checkable
	// in exactly the way a global one is: which row, which subject, which conversation.
	hits, _ := recall(t, access, fixture.userID, fixture.learnerAgent, "fsync")
	var topicHit *MemoryHit
	for index := range hits {
		if hits[index].Memory.MemoryID == memories.redisTopic.MemoryID {
			topicHit = &hits[index]
		}
	}
	if topicHit == nil {
		t.Fatalf("the authorized topic row never came back: %s", memoryIDs(hits))
	}
	if topicHit.Reference.Scope != domain.MemoryScopeTopic || topicHit.Reference.TopicKey != "redis" ||
		topicHit.Reference.Citation.SourceID != "src-"+memories.redisTopic.MemoryID ||
		topicHit.Reference.Citation.Title != "主题记忆：redis" ||
		topicHit.Reference.SourceSessionID != fixture.learnerAgent {
		t.Fatalf("the topic reference does not describe its row: %+v", topicHit.Reference)
	}

	// One item's worth of budget: a read that can only show one memory must still only cite
	// the one it showed. "Truncated means uncited" is a property of the result, not of the
	// scope, and the topic layer does not get to be the exception.
	tight := fixture.topicAccess(t, MemorySearchOptions{TotalRunes: 20, ItemRunes: 20})
	hits, _ = recall(t, tight, fixture.userID, fixture.learnerAgent, "fsync")
	if len(hits) == 0 {
		t.Fatal("a tight budget returned nothing at all")
	}
	for _, hit := range hits {
		if hit.Memory.UserID != fixture.userID {
			t.Fatalf("a truncated read crossed accounts: %+v", hit.Memory)
		}
		if hit.Reference.Citation.SourceID != "src-"+hit.Memory.MemoryID {
			t.Fatalf("a truncated read cites another row: %+v", hit.Reference)
		}
		if len([]rune(hit.Content)) > 21 {
			t.Fatalf("the per-item cap did not hold on the opted-in path: %q", hit.Content)
		}
	}
}

// failingTopicStore answers the two grant questions with an error, and counts the memory
// reads behind them.
type failingTopicStore struct {
	repository.Repository
	getSessionErr string
	quizErr       string
	consentErr    string
	reads         int
}

func (s *failingTopicStore) GetTopicMemoryPreference(ctx context.Context, userID, sessionID string) (domain.TopicMemoryPreference, error) {
	if s.consentErr != "" {
		return domain.TopicMemoryPreference{}, errors.New(s.consentErr)
	}
	return s.Repository.GetTopicMemoryPreference(ctx, userID, sessionID)
}

func (s *failingTopicStore) GetSession(ctx context.Context, userID, sessionID string) (*domain.Session, error) {
	if s.getSessionErr != "" {
		return nil, errors.New(s.getSessionErr)
	}
	return s.Repository.GetSession(ctx, userID, sessionID)
}

func (s *failingTopicStore) GetActiveLearningSession(ctx context.Context, userID, chatSessionID string) (*domain.LearningSession, error) {
	if s.quizErr != "" {
		return nil, errors.New(s.quizErr)
	}
	return s.Repository.GetActiveLearningSession(ctx, userID, chatSessionID)
}

func (s *failingTopicStore) ListActiveMemories(ctx context.Context, userID string, scope domain.MemoryScope, topicKey string) ([]domain.Memory, error) {
	s.reads++
	return s.Repository.ListActiveMemories(ctx, userID, scope, topicKey)
}

// TestTopicGrantFailureEndsTheReadInsteadOfWideningIt: a store that could not answer must
// not be treated as a store that answered "no topics", and it must certainly not fall back to
// reading every subject. The distinction is the one this layer exists for.
func TestTopicGrantFailureEndsTheReadInsteadOfWideningIt(t *testing.T) {
	for name, failure := range map[string][2]string{
		"session lookup failed": {"database is down", ""},
		"quiz lookup failed":    {"", "database is down"},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newTopicFixture(t)
			fixture.seedTopicMemories(t)
			fixture.studyOnOneSubject(t, "redis")
			spy := &failingTopicStore{Repository: fixture.store,
				getSessionErr: failure[0], quizErr: failure[1]}
			grant, err := NewLearningTopicGrant(spy)
			if err != nil {
				t.Fatal(err)
			}
			access := fixture.accessWith(t, grant, wideOptions())
			hits, note, err := access.Recall(t.Context(), MemoryQuery{
				UserID: fixture.userID, SessionID: fixture.learnerAgent, Query: "fsync",
			})
			if err == nil {
				t.Fatalf("a failed %s produced hits %s note %q, want an error", name, memoryIDs(hits), note)
			}
			if len(hits) != 0 || note != "" {
				t.Fatalf("a failed %s still returned %d hits and a note", name, len(hits))
			}
			if spy.reads != 0 {
				t.Fatalf("a failed %s went on to read %d memory lists", name, spy.reads)
			}
		})
	}
}

// lyingTopicStore hands back facts that belong to somebody else, which is how a test checks
// the one property the store's own scoping cannot: that the grant re-asserts "this quiz is
// on this conversation for this user" before it turns a topic key into an authorization.
type lyingTopicStore struct {
	repository.Repository
	quiz *domain.LearningSession
	mode domain.SessionMode
}

func (s *lyingTopicStore) GetSession(ctx context.Context, userID, sessionID string) (*domain.Session, error) {
	session, err := s.Repository.GetSession(ctx, userID, sessionID)
	if err != nil {
		// The caller does not own this conversation; answer anyway, as a store with a broken
		// ownership filter would.
		return &domain.Session{ID: sessionID, UserID: "someone-else", Mode: s.mode}, nil
	}
	// Copy before overriding: the store's own record must not be edited by a test double.
	overridden := *session
	overridden.Mode = s.mode
	return &overridden, nil
}

func (s *lyingTopicStore) GetActiveLearningSession(context.Context, string, string) (*domain.LearningSession, error) {
	return s.quiz, nil
}

// quizOn builds a row shaped like one the store would return, so a double can hand back a quiz
// that disagrees with the conversation in exactly one field at a time.
func (s *lyingTopicStore) quizOn(userID, sessionID, key string) *domain.LearningSession {
	return &domain.LearningSession{
		ID: "lsn-" + key + "-" + sessionID, UserID: userID, SessionID: sessionID,
		Topic: key, TopicKey: key, Status: domain.LearningSessionCreated,
		CurrentQuestion: domain.LearningQuestionOrdinalFirst,
		TotalQuestions:  domain.LearningQuestionCount, Version: 1,
		ExpiresAt: time.Now().Add(time.Hour),
	}
}

// mustGrant is the one-line construction the double-based tests below all need.
func mustGrant(t *testing.T, store LearningTopicStore) MemoryAuthorizer {
	t.Helper()
	grant, err := NewLearningTopicGrant(store)
	if err != nil {
		t.Fatal(err)
	}
	return grant
}

// emptyAnswerStore answers one or both of the grant's two questions with "no record, and no
// error either". Neither of the two real stores produces that shape - they answer ErrNotFound -
// but an interface cannot promise that, and a grant that panicked on a nil record would take
// the whole agent turn down with it.
type emptyAnswerStore struct {
	repository.Repository
	emptySession bool
	emptyQuiz    bool
}

func (s emptyAnswerStore) GetSession(ctx context.Context, userID, sessionID string) (*domain.Session, error) {
	if s.emptySession {
		return nil, nil
	}
	return s.Repository.GetSession(ctx, userID, sessionID)
}

func (s emptyAnswerStore) GetActiveLearningSession(ctx context.Context, userID, chatSessionID string) (*domain.LearningSession, error) {
	if s.emptyQuiz {
		return nil, nil
	}
	return s.Repository.GetActiveLearningSession(ctx, userID, chatSessionID)
}

// GetTopicMemoryPreference answers yes deliberately. The consent gate sits in front of the quiz
// read, so letting the real store answer this for a conversation nobody switched on would stop
// the grant one step early and turn the nil-quiz case below into a test of the wrong line.
func (s emptyAnswerStore) GetTopicMemoryPreference(context.Context, string, string) (domain.TopicMemoryPreference, error) {
	return domain.TopicMemoryPreference{Enabled: true}, nil
}

// TestTopicGrantTreatsAnEmptyAnswerAsNoAuthorization covers the two reads that answer "nothing
// and no error". Neither may become a panic inside a tool loop and neither may be read as an
// open grant: an empty record authorizes the base layers and stops there.
func TestTopicGrantTreatsAnEmptyAnswerAsNoAuthorization(t *testing.T) {
	fixture := newTopicFixture(t)
	seeded := fixture.seedTopicMemories(t)
	fixture.studyOnOneSubject(t, "redis")

	empties := []struct {
		name   string
		double LearningTopicStore
	}{
		{"no conversation record", emptyAnswerStore{Repository: fixture.store, emptySession: true}},
		{"conversation but no quiz record", emptyAnswerStore{Repository: fixture.store, emptyQuiz: true}},
	}
	for _, empty := range empties {
		t.Run(empty.name, func(t *testing.T) {
			grant, err := NewLearningTopicGrant(empty.double)
			if err != nil {
				t.Fatal(err)
			}
			access := fixture.accessWith(t, grant, wideOptions())
			hits, note := recall(t, access, fixture.userID, fixture.learnerAgent, "fsync")
			for _, hit := range hits {
				if hit.Memory.Scope == domain.MemoryScopeTopic {
					t.Fatalf("an empty store answer granted topic memory: %q", hit.Content)
				}
			}
			if recalled(hits, seeded.redisTopic.MemoryID) {
				t.Fatal("the open quiz's topic was reached through an empty answer")
			}
			if strings.Contains(note, "已授权主题") {
				t.Fatalf("the note claimed a subject nobody authorized: %q", note)
			}
			if !recalled(hits, seeded.global.MemoryID) || !recalled(hits, seeded.episodeHere.MemoryID) {
				t.Fatalf("the base layers stopped working: %s", memoryIDs(hits))
			}
		})
	}
}

// TestTopicGrantRefusesAQuizWithNoNormalizedKey pins the property rather than the line that
// happens to enforce it: a quiz record that names no subject cannot authorize a subject. The
// grant checks it and the recall's key normalization checks it again, so this stays true even
// if either half is later rearranged.
func TestTopicGrantRefusesAQuizWithNoNormalizedKey(t *testing.T) {
	fixture := newTopicFixture(t)
	seeded := fixture.seedTopicMemories(t)
	fixture.switchTopicMemory(t, fixture.userID, fixture.learnerAgent, true)

	// Control: the same double, the same conversation, one well-formed quiz row. Without this,
	// the refusal below could be the consent gate or the mode gate firing first, and a double
	// that made every read refuse would pass it.
	lying := &lyingTopicStore{Repository: fixture.store, mode: domain.SessionModeLearningAgent}
	lying.quiz = lying.quizOn(fixture.userID, fixture.learnerAgent, "mysql")
	control, controlNote := recall(t, fixture.accessWith(t, mustGrant(t, lying), wideOptions()),
		fixture.userID, fixture.learnerAgent, "fsync")
	if !recalled(control, seeded.mysqlTopic.MemoryID) {
		t.Fatalf("a correctly attributed quiz on the double granted nothing, so the case below "+
			"proves nothing: %s / %s", memoryIDs(control), controlNote)
	}

	keyless := &domain.LearningSession{
		ID: "lsn-keyless", UserID: fixture.userID, SessionID: fixture.learnerAgent,
		Topic: "Redis 持久化", TopicKey: "   ", Status: domain.LearningSessionCreated,
		CurrentQuestion: domain.LearningQuestionOrdinalFirst,
		TotalQuestions:  domain.LearningQuestionCount, Version: 1,
		ExpiresAt: time.Now().Add(time.Hour),
	}
	grant, err := NewLearningTopicGrant(&lyingTopicStore{
		Repository: fixture.store, quiz: keyless, mode: domain.SessionModeLearningAgent,
	})
	if err != nil {
		t.Fatal(err)
	}
	hits, note := recall(t, fixture.accessWith(t, grant, wideOptions()),
		fixture.userID, fixture.learnerAgent, "fsync")
	for _, hit := range hits {
		if hit.Memory.Scope == domain.MemoryScopeTopic {
			t.Fatalf("a quiz with no topic key authorized %q", hit.Content)
		}
	}
	if recalled(hits, seeded.redisTopic.MemoryID) || recalled(hits, seeded.mysqlTopic.MemoryID) {
		t.Fatalf("an unnamed subject reached a subject: %s", memoryIDs(hits))
	}
	if strings.Contains(note, "已授权主题") {
		t.Fatalf("the note claimed an authorization that cannot name its subject: %q", note)
	}
}

func TestTopicGrantRefusesAQuizThatIsNotTheCallersOwn(t *testing.T) {
	fixture := newTopicFixture(t)
	seeded := fixture.seedTopicMemories(t)
	live := time.Now().Add(time.Hour)
	fixture.switchTopicMemory(t, fixture.userID, fixture.learnerAgent, true)
	lying := &lyingTopicStore{Repository: fixture.store, mode: domain.SessionModeLearningAgent}

	// Control, for the same reason as the keyless case: this double does reach a grant when the
	// row it returns is honestly attributed, so what follows is the identity check refusing.
	lying.quiz = lying.quizOn(fixture.userID, fixture.learnerAgent, "mysql")
	control, _ := recall(t, fixture.accessWith(t, mustGrant(t, lying), wideOptions()),
		fixture.userID, fixture.learnerAgent, "fsync")
	if !recalled(control, seeded.mysqlTopic.MemoryID) {
		t.Fatalf("the double never reached a grant, so the refusals below prove nothing: %s",
			memoryIDs(control))
	}

	// Two misattributed rows, one for each half of the identity check: another account's quiz
	// stamped on this conversation, and this account's own user id stamped on a conversation
	// that has no quiz. Dropping either comparison would unlock the owner's mysql row, which
	// no quiz on this conversation has ever named.
	cases := []struct {
		name string
		quiz *domain.LearningSession
	}{
		{"another account owns the quiz", &domain.LearningSession{
			ID: "lsn-other", UserID: "someone-else", SessionID: fixture.learnerAgent,
			Topic: "mysql", TopicKey: "mysql", Status: domain.LearningSessionCreated,
			CurrentQuestion: domain.LearningQuestionOrdinalFirst,
			TotalQuestions:  domain.LearningQuestionCount, Version: 1, ExpiresAt: live,
		}},
		{"the quiz belongs to another conversation", &domain.LearningSession{
			ID: "lsn-elsewhere", UserID: fixture.userID, SessionID: fixture.secondSess,
			Topic: "mysql", TopicKey: "mysql", Status: domain.LearningSessionCreated,
			CurrentQuestion: domain.LearningQuestionOrdinalFirst,
			TotalQuestions:  domain.LearningQuestionCount, Version: 1, ExpiresAt: live,
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			grant, err := NewLearningTopicGrant(&lyingTopicStore{
				Repository: fixture.store, quiz: testCase.quiz, mode: domain.SessionModeLearningAgent,
			})
			if err != nil {
				t.Fatal(err)
			}
			access := fixture.accessWith(t, grant, wideOptions())
			hits, note := recall(t, access, fixture.userID, fixture.learnerAgent, "fsync")
			for _, hit := range hits {
				if hit.Memory.Scope == domain.MemoryScopeTopic {
					t.Fatalf("a topic row was recalled from a quiz that is not this session's own: %q", hit.Content)
				}
			}
			if recalled(hits, seeded.mysqlTopic.MemoryID) {
				t.Fatal("the misattributed quiz unlocked the owner's other subject")
			}
			if strings.Contains(note, "已授权主题") {
				t.Fatalf("the read was described as topic-authorized: %q", note)
			}
			// The base layers still work, so a refused topic does not quietly become a
			// memory-less conversation.
			if !recalled(hits, seeded.global.MemoryID) || !recalled(hits, seeded.episodeHere.MemoryID) {
				t.Fatalf("the base grant stopped covering its own layers: %s", memoryIDs(hits))
			}
		})
	}
}

func TestNewLearningTopicGrantRefusesAStoreWithNoLearningRead(t *testing.T) {
	if _, err := NewLearningTopicGrant(nil); !errors.Is(err, ErrNoLearningTopicStore) {
		t.Fatalf("nil store = %v, want %v", err, ErrNoLearningTopicStore)
	}
	// A grant with nothing to ask must not be constructible by hand into a working authorizer
	// either: its zero value reads no topics, which is the safe direction, but it is also a
	// silently inert feature, and the constructor is where that gets said out loud.
	grant, err := NewLearningTopicGrant(repository.NewInMemoryRepository())
	if err != nil {
		t.Fatal(err)
	}
	built, err := grant.AuthorizeMemoryScope(t.Context(), "user-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(built.TopicKeys) != 0 {
		t.Fatalf("a session with no quiz authorized %v", built.TopicKeys)
	}
	if !built.AllowGlobal {
		t.Fatal("the base grant must still cover the user's own global memories")
	}
}
