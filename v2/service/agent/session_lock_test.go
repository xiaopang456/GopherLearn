package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"GopherAI/model"
)

// The session lock's own tests.
//
// The claim they defend is the one the learner's click depends on: the
// server resolves their answer into ONE conversation of their own, reads the
// evidence out of it, and asks again - keeping the original question - when
// it cannot.

// fakeCrossLocator is the lightweight search the lock falls back on.
type fakeCrossLocator struct {
	candidates []SessionCandidate
	err        error
	clues      []string
}

func (f *fakeCrossLocator) Locate(_ context.Context, _ int64, _ string, clue string) ([]SessionCandidate, error) {
	f.clues = append(f.clues, clue)
	if f.err != nil {
		return nil, f.err
	}
	return f.candidates, nil
}

// fakeCrossReader is the door to the learner's own conversations.
type fakeCrossReader struct {
	locked   map[string]*LockedSession
	notOwned map[string]bool
	err      error
	asked    []string
}

func (f *fakeCrossReader) LockSession(_ context.Context, _ int64, sessionID string) (*LockedSession, error) {
	f.asked = append(f.asked, sessionID)
	if f.err != nil {
		return nil, f.err
	}
	if f.notOwned[sessionID] {
		return nil, ErrSessionNotOwned
	}
	if locked, ok := f.locked[sessionID]; ok {
		return locked, nil
	}
	return nil, ErrCrossSessionNotFound
}

// clarificationFixture is a clarification with two candidates.
func clarificationFixture() *model.SessionClarification {
	return &model.SessionClarification{
		ClarificationID: "clar-1",
		Question:        "上次你给我排的那三道题里，第二道练的是什么？",
		Prompt:          "你在问哪一次？",
		AllowFreeText:   true,
		Candidates: []model.SessionClarificationCandidate{
			{SessionID: "session-a", Title: "洄漪"},
			{SessionID: "session-b", Title: "浔漳"},
		},
	}
}

// A click on a candidate locks that conversation.
//
// The click carries the conversation's id, so the resolution needs no
// interpretation at all: it is the one reading of the learner's answer that
// is exact.
func TestAClickOnACandidateLocksThatConversation(t *testing.T) {
	reader := &fakeCrossReader{locked: map[string]*LockedSession{
		"session-b": {SessionID: "session-b", Title: "浔漳", Evidence: "第二道练的是读写拆分"},
	}}
	locator := &fakeCrossLocator{}

	resolution := ResolveSessionLock(context.Background(), locator, reader, 7, "current",
		clarificationFixture(), "session-b")

	if !resolution.Resolved() {
		t.Fatalf("the click locked nothing: %+v", resolution)
	}
	if resolution.Locked.SessionID != "session-b" {
		t.Fatalf("the lock is %q, want session-b", resolution.Locked.SessionID)
	}
	if resolution.Reason != lockReasonCandidate {
		t.Fatalf("the reason is %q, want %q", resolution.Reason, lockReasonCandidate)
	}
	// The clue search did not run: a click needs no search.
	if len(locator.clues) != 0 {
		t.Fatalf("the clue search ran on a click: %v", locator.clues)
	}
}

// A pick from the learner's own list locks that conversation too.
//
// It arrives as a session id that was never among the candidates, and it is
// locked by identity - which is what makes a target that never made it into
// a candidate list reachable.
func TestAPickFromTheLearnersOwnListLocksThatConversation(t *testing.T) {
	reader := &fakeCrossReader{locked: map[string]*LockedSession{
		"session-z": {SessionID: "session-z", Title: "目标会话", Evidence: "证据"},
	}}
	locator := &fakeCrossLocator{}

	resolution := ResolveSessionLock(context.Background(), locator, reader, 7, "current",
		clarificationFixture(), "session-z")

	if !resolution.Resolved() {
		t.Fatalf("the pick locked nothing: %+v", resolution)
	}
	if resolution.Locked.SessionID != "session-z" {
		t.Fatalf("the lock is %q, want session-z", resolution.Locked.SessionID)
	}
	if resolution.Reason != lockReasonSessionID {
		t.Fatalf("the reason is %q, want %q", resolution.Reason, lockReasonSessionID)
	}
}

// A clue that names one conversation locks it.
func TestAClueThatNamesOneConversationLocksIt(t *testing.T) {
	reader := &fakeCrossReader{locked: map[string]*LockedSession{
		"session-c": {SessionID: "session-c", Title: "缓存那���", Evidence: "证据"},
	}}
	locator := &fakeCrossLocator{candidates: []SessionCandidate{{SessionID: "session-c", Title: "缓存那次"}}}

	resolution := ResolveSessionLock(context.Background(), locator, reader, 7, "current",
		clarificationFixture(), "上周讲缓存的那次")

	if !resolution.Resolved() {
		t.Fatalf("the clue locked nothing: %+v", resolution)
	}
	if resolution.Locked.SessionID != "session-c" {
		t.Fatalf("the lock is %q, want session-c", resolution.Locked.SessionID)
	}
	if resolution.Reason != lockReasonClue {
		t.Fatalf("the reason is %q, want %q", resolution.Reason, lockReasonClue)
	}
}

// A clue that names several conversations is ambiguous, and the question is
// asked again.
//
// The learner has not changed their mind about WHAT they are asking, so the
// original question is kept: what changed is only which conversation it is
// about, and that is what they are asked again.
func TestAClueThatNamesSeveralConversationsAsksAgain(t *testing.T) {
	reader := &fakeCrossReader{}
	locator := &fakeCrossLocator{candidates: []SessionCandidate{
		{SessionID: "session-a", Title: "洄漪"},
		{SessionID: "session-b", Title: "浔漳"},
	}}

	resolution := ResolveSessionLock(context.Background(), locator, reader, 7, "current",
		clarificationFixture(), "排了三道题的那次")

	if resolution.Resolved() {
		t.Fatal("an ambiguous clue locked a conversation")
	}
	if !resolution.Ambiguous {
		t.Fatalf("the clue was not reported as ambiguous: %+v", resolution)
	}
	if len(resolution.Candidates) != 2 {
		t.Fatalf("the candidates are %+v, want the two the clue named", resolution.Candidates)
	}
	reask := BuildReaskClarification(clarificationFixture(), resolution.Candidates)
	if reask == nil {
		t.Fatal("no question was asked again")
	}
	if reask.Question != clarificationFixture().Question {
		t.Fatalf("the question asked again is %q, want the original one", reask.Question)
	}
	if len(reask.Candidates) != 2 {
		t.Fatalf("the re-ask offers %d candidates, want the two", len(reask.Candidates))
	}
	if !reask.AllowHistoryBrowse {
		t.Fatal("the re-ask does not offer the learner's own conversation list")
	}
}

// A clue that names nothing asks again, and the learner's own list stays
// open.
//
// A target that never made it into a candidate list is still the learner's
// own conversation, and the list is how they reach it.
func TestAClueThatNamesNothingAsksAgainWithTheHistoryOpen(t *testing.T) {
	reader := &fakeCrossReader{}
	locator := &fakeCrossLocator{}

	resolution := ResolveSessionLock(context.Background(), locator, reader, 7, "current",
		clarificationFixture(), "三月里那个没标题的")

	if resolution.Resolved() {
		t.Fatal("a clue that named nothing locked a conversation")
	}
	if resolution.Reason != lockReasonNoCandidate {
		t.Fatalf("the reason is %q, want %q", resolution.Reason, lockReasonNoCandidate)
	}
	reask := BuildReaskClarification(clarificationFixture(), nil)
	if reask == nil {
		t.Fatal("no question was asked again")
	}
	if reask.Question != clarificationFixture().Question {
		t.Fatalf("the question asked again is %q, want the original one", reask.Question)
	}
	// The conversations that were offered before are still offered: the
	// learner may simply have picked the wrong one of them.
	if len(reask.Candidates) != 2 {
		t.Fatalf("the re-ask offers %d candidates, want the two that were offered", len(reask.Candidates))
	}
	if !reask.AllowHistoryBrowse {
		t.Fatal("the re-ask does not offer the learner's own conversation list")
	}
}

// A conversation that belongs to somebody else is refused.
//
// It is not read as a clue either: a foreign id is somebody reaching for a
// conversation that is not theirs, and answering it from a search that
// happened to match would be the wrong answer to the wrong question.
func TestAConversationOfSomebodyElseIsRefused(t *testing.T) {
	reader := &fakeCrossReader{notOwned: map[string]bool{"session-x": true}}
	locator := &fakeCrossLocator{candidates: []SessionCandidate{{SessionID: "session-a", Title: "洄漪"}}}

	resolution := ResolveSessionLock(context.Background(), locator, reader, 7, "current",
		clarificationFixture(), "session-x")

	if resolution.Resolved() {
		t.Fatal("a conversation of somebody else was locked")
	}
	if resolution.Reason != lockReasonNotOwned {
		t.Fatalf("the reason is %q, want %q", resolution.Reason, lockReasonNotOwned)
	}
	if len(locator.clues) != 0 {
		t.Fatalf("the foreign id was read as a clue: %v", locator.clues)
	}
}

// A store that cannot answer does not lock anything, and says so.
//
// The reason is a failure of the store, and it is counted as one: the run
// that has to look at it is the one where the store was unwell.
func TestAStoreThatCannotAnswerLocksNothing(t *testing.T) {
	reader := &fakeCrossReader{err: errors.New("the database is down")}
	locator := &fakeCrossLocator{}

	resolution := ResolveSessionLock(context.Background(), locator, reader, 7, "current",
		clarificationFixture(), "session-a")

	if resolution.Resolved() {
		t.Fatal("a failed lock locked a conversation")
	}
	if resolution.Reason != lockReasonUnavailable {
		t.Fatalf("the reason is %q, want %q", resolution.Reason, lockReasonUnavailable)
	}
}

// A reply that is a sentence is never tried as a conversation id.
func TestASentenceIsNeverTriedAsAConversationID(t *testing.T) {
	reader := &fakeCrossReader{locked: map[string]*LockedSession{
		"session-a": {SessionID: "session-a", Title: "洄漪", Evidence: "证据"},
	}}
	locator := &fakeCrossLocator{}

	resolution := ResolveSessionLock(context.Background(), locator, reader, 7, "current",
		clarificationFixture(), "周一聊 context 的那次")

	if resolution.Resolved() {
		t.Fatal("a sentence locked a conversation")
	}
	if len(reader.asked) != 0 {
		t.Fatalf("a sentence was tried as a conversation id: %v", reader.asked)
	}
	if len(locator.clues) != 1 || locator.clues[0] != "周一聊 context 的那次" {
		t.Fatalf("the clue search was not asked with the learner's own words: %v", locator.clues)
	}
}

// The locked block carries the original question and the evidence.
//
// The model is not asked to find the conversation and it is not asked to look
// for another one: the learner has already answered.
func TestTheLockedBlockCarriesTheQuestionAndTheEvidence(t *testing.T) {
	locked := &LockedSession{SessionID: "session-b", Title: "浔漳", Evidence: "第二道练的是读写拆分"}
	block := renderLockedSessionBlock(locked, "上次你给我排的那三道题里，第二道练的是什么？")

	if !strings.Contains(block, "session-b") {
		t.Fatalf("the block does not name the conversation: %s", block)
	}
	if !strings.Contains(block, "上次你给我排的那三道题") {
		t.Fatalf("the block does not carry the original question: %s", block)
	}
	if !strings.Contains(block, "读写拆分") {
		t.Fatalf("the block does not carry the evidence: %s", block)
	}
	if !strings.Contains(block, "Do not open another conversation") {
		t.Fatalf("the block does not stop the model from going looking: %s", block)
	}
}

// The re-ask block asks, and it does not answer.
//
// A turn that answered a question the learner has not settled which one is
// about is a turn that answered the wrong question.
func TestTheReaskBlockAsksAndDoesNotAnswer(t *testing.T) {
	reask := BuildReaskClarification(clarificationFixture(), []SessionCandidate{
		{SessionID: "session-a", Title: "洄漪"},
		{SessionID: "session-b", Title: "浔漳"},
	})
	block := renderReaskBlock(reask, "排了三道题的那次")

	if !strings.Contains(block, "Ask them which one they mean") {
		t.Fatalf("the block does not ask: %s", block)
	}
	if !strings.Contains(block, "Do not answer the question yet") {
		t.Fatalf("the block does not hold the answer back: %s", block)
	}
	if !strings.Contains(block, "上次你给我排的那三道题") {
		t.Fatalf("the block does not carry the original question: %s", block)
	}
}
