package agenttool

import (
	"context"
	"encoding/json"
	"testing"

	summarysegmentdao "GopherAI/dao/summarysegment"
	"GopherAI/model"
	serviceagent "GopherAI/service/agent"
	"GopherAI/service/episode"

	"github.com/cloudwego/eino/components/tool"
)

// The lock at the tool.
//
// The learner said which conversation they meant, the server checked that it
// is theirs, and it read that conversation's record. The turn may read THAT
// conversation and no other.
//
// The case under test is the one that looks harmless and is not: the model
// asks for ANOTHER conversation of the SAME LEARNER. The store would happily
// answer - it is owner-scoped, and the conversation is the learner's own. The
// refusal has to come from the lock.

// lockedRun is a run whose server locked one conversation.
//
// The other conversation has a CARD, so that the refusal under test is never
// "the card is missing": the learner owns it, it has a card, and the store
// would answer. Only the lock stops the read.
func lockedRun(locked string) serviceagent.RunContext {
	run := serviceagent.RunContext{UserID: 7, SessionID: "current-session",
		Question: "上次那三道题里第二道练的是什么", LockedSessionID: locked}
	run.CardScopes = []serviceagent.CardScope{
		{SessionID: "other-session", CardID: "card-other", CardVersion: 3, CardWatermark: 99},
	}
	run.GrantSet = serviceagent.NewHistoryGrantSet(run, nil)
	return run
}

// lockedSegmentReader is the browser the browse tool reads.
//
// It is owner-scoped the way the real one is: a segment of a conversation the
// learner does not own is a miss, and a segment of one they DO own is there.
// The refusal under test is therefore never the store's.
type lockedSegmentReader struct {
	segments map[string]*model.SessionSummarySegment
	reads    int
}

func (f *lockedSegmentReader) GetSummarySegment(_ context.Context, userID int64,
	sessionID string, seq int) (*model.SessionSummarySegment, error) {
	f.reads++
	segment, ok := f.segments[sessionID+":"+itoa(seq)]
	if !ok || segment.UserID != userID {
		return nil, model.ErrSummarySegmentNotFound
	}
	return segment, nil
}

func (f *lockedSegmentReader) ListSummarySegments(_ context.Context, userID int64,
	sessionID string, limit int) ([]model.SessionSummarySegment, error) {
	out := make([]model.SessionSummarySegment, 0, limit)
	for seq := 1; seq <= limit; seq++ {
		segment, ok := f.segments[sessionID+":"+itoa(seq)]
		if !ok || segment.UserID != userID {
			continue
		}
		out = append(out, *segment)
	}
	return out, nil
}

func (f *lockedSegmentReader) GetWhiteboard(_ context.Context, _ int64,
	_ string) (*model.SessionWhiteboard, error) {
	return nil, model.ErrWhiteboardNotFound
}

// itoa is the small number formatter the fake needs.
func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := ""
	for value > 0 {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
	}
	return digits
}

// The browse tool refuses another conversation of the learner's own.
//
// The conversation has a card and the learner owns it, so nothing but the
// lock stops the read - and the read is exactly what must not happen.
func TestTheBrowseRefusesAnotherConversationOfTheLearnersOwn(t *testing.T) {
	reader := &lockedSegmentReader{segments: map[string]*model.SessionSummarySegment{
		"other-session:1": {UserID: 7, SessionID: "other-session", Seq: 1,
			Content: storedSegment(t, "别的会话", []model.SessionSummarySegmentPoint{
				{Text: "别的会话里的内容", EvidenceMessageIDs: []uint{5}}})},
	}}
	registry, err := NewBrowseSummarySegmentsRegistry(reader)
	if err != nil {
		t.Fatal(err)
	}
	registry.UseSessionParameter(true)
	run := lockedRun("locked-session")
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	out := callBrowseIn(t, tools[0], "other-session", "三道题")
	if len(out.Segments) > 0 {
		t.Fatalf("the browse read another conversation of the learner's own: %+v", out)
	}
	if out.Reason != browseReasonSessionNotAllowed {
		t.Fatalf("the browse reported %q, want %q", out.Reason, browseReasonSessionNotAllowed)
	}
	if reader.reads != 0 {
		t.Fatalf("the store was read %d times, want none", reader.reads)
	}
}

// The browse tool DOES read the locked conversation.
//
// The lock is a restriction and not a refusal of everything: the conversation
// the learner pointed at is the one the turn answers from.
func TestTheBrowseReadsTheLockedConversation(t *testing.T) {
	reader := &lockedSegmentReader{segments: map[string]*model.SessionSummarySegment{
		"locked-session:1": {UserID: 7, SessionID: "locked-session", Seq: 1,
			Content: storedSegment(t, "三道题", []model.SessionSummarySegmentPoint{
				{Text: "第二道练的是读写拆分", EvidenceMessageIDs: []uint{5}}})},
	}}
	registry, err := NewBrowseSummarySegmentsRegistry(reader)
	if err != nil {
		t.Fatal(err)
	}
	registry.UseSessionParameter(true)
	run := lockedRun("locked-session")
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	out := callBrowseIn(t, tools[0], "locked-session", "三道题")
	if len(out.Segments) == 0 {
		t.Fatalf("the browse did not read the locked conversation: %+v", out)
	}
}

// The whiteboard tool refuses another conversation of the learner's own.
//
// It is the same boundary on another door: the board is the conversation's own
// current state, and the conversation in front of the learner is not the one
// they asked about.
func TestTheWhiteboardRefusesAnotherConversationOfTheLearnersOwn(t *testing.T) {
	reader := &fakeWhiteboardReader{board: storedBoard(t, "other-session",
		model.SessionWhiteboardDocument{SchemaVersion: 1,
			Goal: &model.SessionWhiteboardGoal{Text: "别的会话的目标", SourceMessageIDs: []uint{5}}})}
	registry, err := NewReadSessionWhiteboardRegistry(reader)
	if err != nil {
		t.Fatal(err)
	}
	run := lockedRun("locked-session")
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	out := callWhiteboard(t, tools[0], "other-session")
	if out.Status == whiteboardStatusOK {
		t.Fatalf("the whiteboard read another conversation of the learner's own: %+v", out)
	}
	if out.Reason != whiteboardReasonNotAllowed {
		t.Fatalf("the whiteboard reported %q, want %q", out.Reason, whiteboardReasonNotAllowed)
	}
	if reader.calls != 0 {
		t.Fatalf("the board was read %d times, want none", reader.calls)
	}
}

// The card search refuses another conversation of the learner's own.
//
// The search is the one tool that finds conversations by their words, so it
// is the one most likely to be handed a conversation the learner owns and the
// lock does not.
func TestTheCardSearchRefusesAnotherConversationOfTheLearnersOwn(t *testing.T) {
	retriever := &fakeThinCardRetriever{candidates: []episode.ThinCardCandidate{
		{SessionID: "other-session", CardID: "card-2", CardVersion: 3, Title: "别的会话"},
		{SessionID: "locked-session", CardID: "card-1", CardVersion: 1, Title: "锁定的那次"},
	}}
	registry, err := NewSearchSessionCardsRegistry(retriever)
	if err != nil {
		t.Fatal(err)
	}
	run := lockedRun("locked-session")
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	out := callCards(t, tools[0], "other-session", "三道题")
	if len(out.Candidates) != 0 {
		t.Fatalf("the card search opened another conversation of the learner's own: %+v", out.Candidates)
	}
	if out.Reason != searchSessionReasonNotAllowed {
		t.Fatalf("the card search reported %q, want %q", out.Reason, searchSessionReasonNotAllowed)
	}
}

// The card search still returns CANDIDATES, and does not open them.
//
// The candidates are the automatic way IN, and the learner has already been
// through that door. What the lock stops is the READING, and a candidate list
// is not a read.
func TestTheCardSearchStillReturnsCandidatesWithoutOpeningThem(t *testing.T) {
	retriever := &fakeThinCardRetriever{candidates: []episode.ThinCardCandidate{
		{SessionID: "other-session", CardID: "card-2", CardVersion: 3, Title: "别的会话"},
		{SessionID: "locked-session", CardID: "card-1", CardVersion: 1, Title: "锁定的那次"},
	}}
	registry, err := NewSearchSessionCardsRegistry(retriever)
	if err != nil {
		t.Fatal(err)
	}
	run := lockedRun("locked-session")
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	out := callCards(t, tools[0], "", "三道题")
	if len(out.Candidates) != 2 {
		t.Fatalf("the card search returned %d candidates, want both", len(out.Candidates))
	}
	if run.GrantSet.AuthorisesSession(run, "other-session") {
		t.Fatal("the card search opened a conversation beside the lock")
	}
	if !run.GrantSet.AuthorisesSession(run, "locked-session") {
		t.Fatal("the locked conversation is not open")
	}
}

// The segment search refuses a hit in another conversation of the learner's own.
//
// This is the case the tool used to get wrong: it FOUND a card for the
// conversation and treated that as permission. The grant set is the
// authority, and a scope it refused is a conversation the turn may not open.
func TestTheSegmentSearchRefusesAHitInAnotherConversationOfTheLearnersOwn(t *testing.T) {
	finder := &fakeSegmentFinder{hits: []summarysegmentdao.UserSegmentHit{
		{SessionID: "other-session", Seq: 1, EndMessageID: 6,
			Content: storedSegment(t, "别的会话", []model.SessionSummarySegmentPoint{
				{Text: "别的会话里的内容", EvidenceMessageIDs: []uint{5}}})},
	}}
	reader := &fakeSegmentReader{segments: map[string]*model.SessionSummarySegment{
		"other-session:1": {UserID: 7, SessionID: "other-session", Seq: 1,
			Content: storedSegment(t, "别的会话", []model.SessionSummarySegmentPoint{
				{Text: "别的会话里的内容", EvidenceMessageIDs: []uint{5}}})},
	}}
	retriever := &fakeThinCardRetriever{candidates: []episode.ThinCardCandidate{
		{SessionID: "other-session", CardID: "card-2", CardVersion: 3, Title: "别的会话"},
	}}
	registry, err := NewSearchHistorySegmentsRegistry(finder, reader, retriever)
	if err != nil {
		t.Fatal(err)
	}
	run := lockedRun("locked-session")
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	out := callSegments(t, tools[0], "三道题")
	for _, hit := range out.Segments {
		if hit.SessionID == "other-session" && hit.Reachable {
			t.Fatalf("the segment search opened another conversation of the learner's own: %+v", hit)
		}
	}
	if run.GrantSet.AuthorisesSession(run, "other-session") {
		t.Fatal("the segment search opened a conversation beside the lock")
	}
}

// The message read-back tool refuses an id from another conversation.
//
// The id is the learner's own, and the store would return it. The grant set
// is what says which ids this turn may read back, and the lock narrowed it.
//
// The run carries a grant for the LOCKED conversation's own id, because the
// tool is only offered when there is something to read at all - and that id
// is read, while the other conversation's id is refused.
func TestTheMessageReadBackRefusesAnIDFromAnotherConversation(t *testing.T) {
	reader := &lockedMessageReader{messages: map[uint]model.Message{
		5: {ID: 5, SessionID: "other-session", UserName: "learner", Content: "别的会话里的话"},
		6: {ID: 6, SessionID: "locked-session", UserName: "learner", Content: "锁定那轮里的话", IsUser: true},
	}}
	registry, err := NewHistoryMessageRegistry(reader)
	if err != nil {
		t.Fatal(err)
	}
	run := lockedRun("locked-session")
	run.HistoryGrants = []serviceagent.HistoryMessageGrant{{
		MessageID: 6, SourceKind: serviceagent.HistorySourceSessionSearch,
		SourceSessionID: "locked-session",
	}}
	run.GrantSet = serviceagent.NewHistoryGrantSet(run, run.HistoryGrants)
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) == 0 {
		t.Fatal("the read-back tool was not offered")
	}
	invokable, ok := tools[0].(tool.InvokableTool)
	if !ok {
		t.Fatal("the read-back tool is not invokable")
	}
	raw, err := invokable.InvokableRun(context.Background(), `{"message_ids":[5,6]}`)
	if err != nil {
		t.Fatal(err)
	}
	out := struct {
		Messages []struct {
			MessageID int    `json:"message_id"`
			Content   string `json:"content"`
		} `json:"messages"`
		Refused []int `json:"refused"`
	}{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("decode the tool result: %v", err)
	}
	for _, message := range out.Messages {
		if message.MessageID == 5 {
			t.Fatalf("the read-back returned a message from another conversation: %+v", out.Messages)
		}
	}
	if len(out.Messages) != 1 || out.Messages[0].MessageID != 6 {
		t.Fatalf("the read-back did not return the locked conversation's own message: %+v", out.Messages)
	}
	if len(out.Refused) != 1 || out.Refused[0] != 5 {
		t.Fatalf("the refused ids are %v, want the other conversation's id", out.Refused)
	}
}

// lockedMessageReader is the store the read-back tool reads.
//
// It is owner-scoped the way the real one is, so the refusal under test is
// never the store's: the message IS the learner's and the store WOULD
// return it.
type lockedMessageReader struct {
	messages map[uint]model.Message
}

func (f *lockedMessageReader) ReadOwnedMessages(_ context.Context, userID int64,
	messageIDs []uint) ([]model.Message, error) {
	out := make([]model.Message, 0, len(messageIDs))
	for _, id := range messageIDs {
		message, ok := f.messages[id]
		if !ok {
			continue
		}
		out = append(out, message)
	}
	return out, nil
}

func (f *lockedMessageReader) ReadOwnedMessagesBySession(_ context.Context, _ int64,
	messageIDsBySession map[string][]uint) ([]model.Message, error) {
	out := make([]model.Message, 0, 8)
	for session, ids := range messageIDsBySession {
		for _, id := range ids {
			message, ok := f.messages[id]
			if !ok || message.SessionID != session {
				continue
			}
			out = append(out, message)
		}
	}
	return out, nil
}
