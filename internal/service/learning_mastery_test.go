package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/repository/repositorytest"
	"github.com/xiaopang456/GopherLearn/internal/responder"
)

// recordingResponder captures the turn it was handed, which is where the two claims in
// this file are checkable: that an agent turn is told how the learner has been doing,
// and that no other turn is.
type recordingResponder struct {
	requests []responder.Request
}

func (r *recordingResponder) Respond(_ context.Context, request responder.Request) (responder.Answer, error) {
	r.requests = append(r.requests, request)
	return responder.Answer{Content: "答案。"}, nil
}

func (r *recordingResponder) Stream(_ context.Context, request responder.Request, emit func(string) error) (responder.Answer, error) {
	r.requests = append(r.requests, request)
	if emit != nil {
		if err := emit("答案。"); err != nil {
			return responder.Answer{}, err
		}
	}
	return responder.Answer{Content: "答案。"}, nil
}

// stubMastery is a reader with a scripted answer, so the wiring can be tested without a
// quiz workflow behind it. It records the question it was asked about, because the
// question is now part of the contract: a reader that ignored it could hand back a note
// about the wrong subject and every other test here would still pass.
type stubMastery struct {
	note     string
	err      error
	calls    int
	whoRead  string
	question string
}

func (s *stubMastery) MasteryNote(_ context.Context, userID, question string) (string, error) {
	s.calls++
	s.whoRead = userID
	s.question = question
	if s.err != nil {
		return "", s.err
	}
	return s.note, nil
}

func newMasteryTurnFixture(t *testing.T, mode domain.SessionMode, reader MasteryReader,
) (*MessageService, string, string, *recordingResponder) {
	t.Helper()
	ctx := t.Context()
	store := repository.NewInMemoryRepository()
	user, err := store.CreateUser(ctx, &domain.User{Email: "learner@example.com", Username: "learner"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "会话", Mode: mode})
	if err != nil {
		t.Fatal(err)
	}
	assistant := &recordingResponder{}
	var options []MessageServiceOption
	options = append(options, WithCollectionAccess(openAccess{}))
	if reader != nil {
		options = append(options, WithMasteryContext(reader))
	}
	return NewMessageService(store, assistant, options...), user.ID, session.ID, assistant
}

func TestMasteryNoteReachesOnlyAgentTurns(t *testing.T) {
	const note = "服务端判断：讲解可以从边界情形开始。"
	for _, testCase := range []struct {
		name      string
		mode      domain.SessionMode
		knowledge domain.KnowledgeOptions
		want      string
		wantReads int
	}{
		{
			name: "an agent turn", mode: domain.SessionModeLearningAgent,
			knowledge: domain.KnowledgeOptions{}, want: note, wantReads: 1,
		},
		{
			// A direct answer is not planning anything. Telling it how well the learner
			// is doing gives it a judgement with no use for one.
			name: "a direct chat turn", mode: domain.SessionModeChat,
			knowledge: domain.KnowledgeOptions{}, want: "", wantReads: 0,
		},
		{
			name: "a knowledge turn", mode: domain.SessionModeChat,
			knowledge: domain.KnowledgeOptions{Enabled: true, CollectionIDs: []string{"col-1"}},
			want:      "", wantReads: 0,
		},
		{
			// Grounding is a per-turn option, so the strategy is what decides rather
			// than the session mode. An agent turn that also grounds is not a thing this
			// server allows - the mode check refuses it - so the closest lawful case is a
			// chat session that grounds: still no note.
			name: "a grounded turn", mode: domain.SessionModeChat,
			knowledge: domain.KnowledgeOptions{Enabled: true, CollectionIDs: []string{"col-1"}},
			want:      "", wantReads: 0,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			reader := &stubMastery{note: note}
			service, userID, sessionID, assistant := newMasteryTurnFixture(t, testCase.mode, reader)
			if _, err := service.Send(t.Context(), userID, sessionID, "讲讲虚拟节点。", testCase.knowledge); err != nil {
				t.Fatal(err)
			}
			if len(assistant.requests) != 1 {
				t.Fatalf("%d responder calls, want 1", len(assistant.requests))
			}
			if got := assistant.requests[0].TopicMastery; got != testCase.want {
				t.Fatalf("note = %q, want %q", got, testCase.want)
			}
			if reader.calls != testCase.wantReads {
				t.Fatalf("%d projection reads, want %d", reader.calls, testCase.wantReads)
			}
			if reader.calls == 0 {
				return
			}
			// The two arguments the reader needs are the authenticated owner and this
			// turn's own question. A reader handed something else could describe a
			// different person, or a topic the learner never asked about.
			if reader.whoRead != userID {
				t.Fatalf("note read for %q, want the authenticated user %q", reader.whoRead, userID)
			}
			if reader.question != "讲讲虚拟节点。" {
				t.Fatalf("note matched against %q, want the turn's question", reader.question)
			}
		})
	}
}

func TestMasteryTurnSurvivesAnUnreadableProjection(t *testing.T) {
	// A note changes how deep an explanation starts. It is not something a learner can
	// lose their turn over, and it is not something to pretend never happened either: the
	// turn goes on without it.
	for name, failure := range map[string]error{
		"storage unavailable": errors.New("database is unavailable"),
		"cancelled context":   context.Canceled,
	} {
		t.Run(name, func(t *testing.T) {
			service, userID, sessionID, assistant := newMasteryTurnFixture(t,
				domain.SessionModeLearningAgent, &stubMastery{err: failure})
			if _, err := service.Send(t.Context(), userID, sessionID, "讲讲 RDB。", domain.KnowledgeOptions{}); err != nil {
				t.Fatalf("a turn failed because the projection could not be read: %v", err)
			}
			if assistant.requests[0].TopicMastery != "" {
				t.Fatalf("note = %q, want none", assistant.requests[0].TopicMastery)
			}
		})
	}
	// With no reader installed at all - the ordinary configuration for a deployment
	// without the quiz workflow - the field stays empty and nothing is consulted.
	service, userID, sessionID, assistant := newMasteryTurnFixture(t, domain.SessionModeLearningAgent, nil)
	if _, err := service.Send(t.Context(), userID, sessionID, "讲讲 RDB。", domain.KnowledgeOptions{}); err != nil {
		t.Fatal(err)
	}
	if assistant.requests[0].TopicMastery != "" {
		t.Fatal("a deployment without the quiz workflow carried a mastery note")
	}
}

// masteryProjection is a stored record written directly, because the renderer's contract
// is about what a record says and not about how it came to say it. The path that builds
// such a row from answers is covered by TestMasteryNoteReadsTheMostRecentlyPractisedTopic.
func masteryProjection(level domain.TopicProgressLevel, score, attempts int, weak ...string) domain.TopicProgress {
	stamped := time.Unix(1000, 0).UTC()
	return domain.TopicProgress{
		UserID: "user-1", TopicKey: "redis-持久化", Topic: "Redis 持久化",
		Mastery: score, Confidence: float64(attempts) / 6, ValidCount: attempts,
		Level: level, WeakPoints: append([]string(nil), weak...), LastAttempt: &stamped,
	}
}

func TestRenderMasteryNoteIsProseAboutOneTopicAndCarriesNoNumbers(t *testing.T) {
	cases := []struct {
		name        string
		progress    domain.TopicProgress
		wantBlank   bool
		wantPhrases []string
	}{
		{
			name:      "a topic practised once is not a description of anyone",
			progress:  masteryProjection(domain.TopicProgressDeveloping, 90, 1, "缺口一"),
			wantBlank: true,
		},
		{
			name:      "an insufficient level says so",
			progress:  masteryProjection(domain.TopicProgressInsufficient, 99, 9, "缺口一"),
			wantBlank: true,
		},
		{
			name:        "three answers that hold up",
			progress:    masteryProjection(domain.TopicProgressMastered, 92, 3),
			wantPhrases: []string{"Redis 持久化", "可以直接进入边界情形"},
		},
		{
			name:        "three answers with gaps",
			progress:    masteryProjection(domain.TopicProgressDeveloping, 64, 4, "没说出重写触发条件", "混淆了追加与回写"),
			wantPhrases: []string{"Redis 持久化", "没说出重写触发条件", "混淆了追加与回写"},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			note := renderMasteryNote(testCase.progress)
			if testCase.wantBlank {
				if note != "" {
					t.Fatalf("note = %q, want nothing said about an unreliable record", note)
				}
				return
			}
			if note == "" {
				t.Fatal("a reliable projection produced no note")
			}
			for _, phrase := range testCase.wantPhrases {
				if !strings.Contains(note, phrase) {
					t.Fatalf("note %q omits %q", note, phrase)
				}
			}
			// This is the whole reason the projection is rendered instead of forwarded:
			// a model handed "64" will say "64" to the person it is explaining to.
			for _, forbidden := range []string{"分数", "得分", "等级", "mastered", "developing", "insufficient"} {
				if strings.Contains(note, forbidden) {
					t.Fatalf("note %q states the record in terms a model would repeat: %q", note, forbidden)
				}
			}
			if strings.ContainsFunc(note, unicode.IsDigit) {
				t.Fatalf("note carries a digit somewhere:\n%s", note)
			}
			if !strings.Contains(note, "不要向学习者复述") {
				t.Fatal("a note a model may repeat has to say not to")
			}
			if !strings.Contains(note, "《Redis 持久化》") {
				t.Fatal("a note that does not name its topic can be applied to the wrong subject")
			}
		})
	}
}

func TestRenderMasteryNoteBoundsTheGapList(t *testing.T) {
	// Eight recorded gaps is what the projection can hold; three is what a prompt should
	// carry. A gap long enough to dominate the instruction is clipped rather than
	// trusted.
	progress := masteryProjection(domain.TopicProgressDeveloping, 60, 6,
		strings.Repeat("很长的缺口", 40), "缺口一", "  ", "缺口二", "缺口三", "缺口四")
	note := renderMasteryNote(progress)
	if strings.Contains(note, "缺口四") {
		t.Fatalf("the note carried more gaps than it should:\n%s", note)
	}
	// Three is the cap, in first-seen order, so the long one leads and 缺口三 is the
	// first thing given up.
	for _, phrase := range []string{"缺口一", "缺口二"} {
		if !strings.Contains(note, phrase) {
			t.Fatalf("the note dropped %q:\n%s", phrase, note)
		}
	}
	for _, dropped := range []string{"缺口三", "缺口四"} {
		if strings.Contains(note, dropped) {
			t.Fatalf("the note carried %q past the cap:\n%s", dropped, note)
		}
	}
	// The clip is 60 runes and the gap is 120, so the gap must not arrive whole. Counting
	// the repeats is how that is checked without restating the clip length.
	if strings.Contains(note, strings.Repeat("很长的缺口", 21)) {
		t.Fatalf("an unclipped gap swamped the note:\n%s", note)
	}
	if !strings.Contains(note, "…") {
		t.Fatalf("a clipped gap should look clipped:\n%s", note)
	}
	// A projection whose only gaps are blank still reads as a direction rather than as a
	// sentence ending in a colon.
	blank := renderMasteryNote(masteryProjection(domain.TopicProgressDeveloping, 60, 3, "   "))
	if !strings.Contains(blank, "还说不清的地方") {
		t.Fatalf("blank gaps produced %q", blank)
	}
}

// newLearningStoreFixture builds a store with one user and one learning-agent
// conversation, for the cases that drive the repository directly rather than through a
// service.
func newLearningStoreFixture(t *testing.T) (context.Context, repository.Repository, string, string) {
	t.Helper()
	ctx := t.Context()
	store := repository.NewInMemoryRepository()
	user, err := store.CreateUser(ctx, &domain.User{Email: "learner@example.com", Username: "learner"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "会话",
		Mode: domain.SessionModeLearningAgent})
	if err != nil {
		t.Fatal(err)
	}
	return ctx, store, user.ID, session.ID
}

// practiceTopic drives three graded answers on one topic through the real store, so the
// projection under test is the one a learner would actually have.
func practiceTopic(t *testing.T, ctx context.Context, store repository.Repository,
	userID, prefix, topic string, attempts int, gap string,
) {
	t.Helper()
	chat, err := store.CreateSession(ctx, &domain.Session{UserID: userID, Title: "会话-" + topic,
		Mode: domain.SessionModeLearningAgent})
	if err != nil {
		t.Fatal(err)
	}
	quiz, questions := repositorytest.OpenLearningQuiz(t, store, userID, chat.ID, topic)
	for index := range attempts {
		repositorytest.AnswerLearningQuestion(t, store, quiz, questions[index],
			prefix+"-"+string(rune('a'+index)),
			domain.LearningGrade{Correctness: 40, Completeness: 15, Clarity: 9,
				Feedback: "答到了主要机制。", MissingPoints: []string{gap}, Confidence: 0.8})
	}
}

func TestMasteryNoteNamesOnlyTheTopicTheQuestionAsksAbout(t *testing.T) {
	ctx, store, userID, _ := newLearningStoreFixture(t)
	service := NewLearningService(store, nil, nil, nil, nil)

	// Nothing practised yet is not an error and not an empty-with-a-reason: there is
	// simply nothing to say, which is the ordinary case for a new account.
	note, err := service.MasteryNote(ctx, userID, "Redis 持久化里 AOF 的重写条件是什么？")
	if err != nil {
		t.Fatal(err)
	}
	if note != "" {
		t.Fatalf("note before any practice = %q, want nothing", note)
	}
	if _, err := service.MasteryNote(ctx, "", "Redis 持久化"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("note with no identity = %v, want unauthorized", err)
	}

	practiceTopic(t, ctx, store, userID, "sub-redis", "Redis 持久化", domain.LearningQuestionCount, "没说出重写触发条件")
	practiceTopic(t, ctx, store, userID, "sub-go", "Go 调度器", 2, "说不清抢占点")

	// The reliable topic's own subject line gets the note.
	note, err = service.MasteryNote(ctx, userID, "Redis 持久化里，AOF 的重写触发条件是谁定的？")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note, "Redis 持久化") {
		t.Fatalf("note for the practised topic = %q", note)
	}
	if strings.ContainsFunc(note, unicode.IsDigit) {
		t.Fatalf("the rendered note leaked a number into a prompt:\n%s", note)
	}

	// A question about a subject nobody ever drilled gets nothing at all. This is the
	// case the previous rule got wrong: two answers on a newer topic used to be able to
	// displace the reliable one, and a question about a third subject used to borrow
	// whichever note was most recent.
	for _, unrelated := range []string{
		"Kafka 的分区再平衡是怎么触发的？",
		"讲讲虚拟节点和槽位迁移。",
		"为什么天空是蓝的？",
		"   ",
		"",
	} {
		quiet, err := service.MasteryNote(ctx, userID, unrelated)
		if err != nil {
			t.Fatalf("question %q: %v", unrelated, err)
		}
		if quiet != "" {
			t.Fatalf("question %q borrowed a note it did not earn:\n%s", unrelated, quiet)
		}
	}

	// A second, unreliable topic does not get a note either, even though the question
	// names it: two answers describe one lesson, not a learner.
	two, err := service.MasteryNote(ctx, userID, "Go 调度器的抢占点在哪里？")
	if err != nil {
		t.Fatal(err)
	}
	if two != "" {
		t.Fatalf("a topic with two answers produced a note:\n%s", two)
	}

	// Running the same question twice must produce byte-identical prose. The note is
	// positioned as a server-written fact, so a second read that phrased it differently -
	// or picked the other matching topic - would be visible to the learner as the system
	// changing its mind about them.
	first, err := service.MasteryNote(ctx, userID, "Redis 持久化里 RDB 和 AOF 各失去什么？")
	if err != nil {
		t.Fatal(err)
	}
	if first == "" {
		t.Fatal("the topic's own name produced no note")
	}
	for repeat := range 5 {
		again, err := service.MasteryNote(ctx, userID, "Redis 持久化里 RDB 和 AOF 各失去什么？")
		if err != nil {
			t.Fatal(err)
		}
		if again != first {
			t.Fatalf("run %d differs from the first:\n%s\n%s", repeat, first, again)
		}
	}
}

// TestMasteryNotePrefersTheBestMatchOverTheNewest is the case that separates this gate
// from the rule it replaced. Both topics are reliable and both are named by the question,
// so a note exists either way - the only thing that decides which one is how completely
// the question stated the topic. The weaker match is the one practised last, so a
// selection that fell back to recency would name the wrong subject and every other test in
// this file would still pass.
func TestMasteryNotePrefersTheBestMatchOverTheNewest(t *testing.T) {
	ctx, store, userID, _ := newLearningStoreFixture(t)
	service := NewLearningService(store, nil, nil, nil, nil)

	practiceTopic(t, ctx, store, userID, "sub-strong", "Redis 持久化", domain.LearningQuestionCount, "没说出重写触发条件")
	practiceTopic(t, ctx, store, userID, "sub-weak", "缓存淘汰策略与内存回收", domain.LearningQuestionCount, "说不清 LRU 与 LFU")

	const question = "Redis 持久化和缓存淘汰策略怎么选？"
	// Both must clear the floor, or the comparison below is about nothing.
	matcher := newLearningTopicMatcher(question)
	strong, weak := matcher.coverage("Redis 持久化"), matcher.coverage("缓存淘汰策略与内存回收")
	if strong <= weak || weak < domain.LearningTopicMatchMinCoverage {
		t.Fatalf("the fixture does not produce a strong/weaker pair: %v vs %v", strong, weak)
	}

	note, err := service.MasteryNote(ctx, userID, question)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note, "Redis 持久化") {
		t.Fatalf("the note went to the weaker match:\n%s", note)
	}
	if strings.Contains(note, "缓存淘汰策略与内存回收") {
		t.Fatalf("the most recent topic displaced the best match:\n%s", note)
	}
}

func TestMasteryNoteGoesQuietOnceTheTopicWentStale(t *testing.T) {
	ctx, store, userID, _ := newLearningStoreFixture(t)
	// Attempts are stamped by the store's own clock, so the window is walked by moving
	// the service's clock forward rather than by waiting. The bound is what is under test,
	// and a test that slept for sixty days would be asserting nothing about it.
	probe := NewLearningService(store, nil, nil, nil, nil)
	practiceTopic(t, ctx, store, userID, "sub-stale", "Redis 持久化", domain.LearningQuestionCount, "没说出重写触发条件")

	const question = "Redis 持久化里，AOF 的重写触发条件是谁定的？"
	if note, err := probe.MasteryNote(ctx, userID, question); err != nil || note == "" {
		t.Fatalf("a fresh projection must speak: %q, %v", note, err)
	}

	// One hour short of the window still counts; one hour past it does not. Straddling
	// the boundary by an hour rather than by a tick keeps the pair honest about which side
	// of the line each case is on.
	inside := NewLearningService(store, nil, nil, nil, nil).WithLearningClock(
		func() time.Time { return time.Now().UTC().Add(domain.LearningMasteryStaleAfter - time.Hour) })
	if note, err := inside.MasteryNote(ctx, userID, question); err != nil || note == "" {
		t.Fatalf("%v inside the window went silent: %q, %v", domain.LearningMasteryStaleAfter, note, err)
	}
	outside := NewLearningService(store, nil, nil, nil, nil).WithLearningClock(
		func() time.Time { return time.Now().UTC().Add(domain.LearningMasteryStaleAfter + time.Hour) })
	if note, err := outside.MasteryNote(ctx, userID, question); err != nil {
		t.Fatal(err)
	} else if note != "" {
		t.Fatalf("a stale projection still reached the prompt:\n%s", note)
	}

	// Sixty days is a product decision, not an implementation detail: it is the promise
	// that a note describes somebody's recent work. Pinning the literal is the only way a
	// quiet edit to the constant breaks something.
	if domain.LearningMasteryStaleAfter != 60*24*time.Hour {
		t.Fatalf("stale window = %v, want sixty days", domain.LearningMasteryStaleAfter)
	}
	if domain.LearningMasteryCandidateTopics != 50 {
		t.Fatalf("candidate read = %d, want the documented bound of 50", domain.LearningMasteryCandidateTopics)
	}
}

// TestMasteryMatchingNeverAsksAModel pins the cost side of the gate. The matcher is
// lexical, so an unmatched question - the common case - must not reach a provider at all,
// and must not consume one of a quiz's generation calls.
func TestMasteryMatchingNeverAsksAModel(t *testing.T) {
	ctx, store, userID, _ := newLearningStoreFixture(t)
	chat := &learningScriptedModel{replies: []string{learningQuestions([]int{1}, []int{2}, []int{3})}}
	service := NewLearningService(store, chat, nil, nil, nil)
	practiceTopic(t, ctx, store, userID, "sub-model", "Redis 持久化", domain.LearningQuestionCount, "没说出重写触发条件")

	for _, question := range []string{
		"Redis 持久化里 AOF 的重写条件", "Kafka 分区再平衡", "随便问点什么", "",
	} {
		if _, err := service.MasteryNote(ctx, userID, question); err != nil {
			t.Fatal(err)
		}
	}
	if chat.calls != 0 {
		t.Fatalf("topic matching spent %d model calls, want none", chat.calls)
	}
}
