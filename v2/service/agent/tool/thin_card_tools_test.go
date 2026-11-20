package agenttool

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"

	summarysegmentdao "GopherAI/dao/summarysegment"
	"GopherAI/model"
	serviceagent "GopherAI/service/agent"
	"GopherAI/service/episode"

	"github.com/cloudwego/eino/components/tool"
)

// The on-demand retrieval tools.
//
// They are exercised against fakes: no index, no database, no model. What is
// under test is the part that is easy to get wrong and expensive to get wrong
// later - which conversation a tool may open, and which one it may not.

// fakeThinCardRetriever is the card index the tools read.
type fakeThinCardRetriever struct {
	candidates []episode.ThinCardCandidate
	terms      []string
	topics     []string
	temporal   episode.TemporalMode
	period     episode.ThinCardQuery
	err        error
}

func (f *fakeThinCardRetriever) Recall(_ context.Context, _ int64, _ string,
	query episode.ThinCardQuery) ([]episode.ThinCardCandidate, error) {
	f.terms = query.Terms
	f.topics = query.Topics
	f.temporal = query.Temporal
	f.period = query
	if f.err != nil {
		return nil, f.err
	}
	return f.candidates, nil
}

// CardOfSession is the lookup by identity.
//
// The fake answers from the candidate list it was given: a candidate whose
// session matches is a conversation that has a card.
func (f *fakeThinCardRetriever) CardOfSession(_ context.Context, _ int64,
	sessionID string) (episode.ThinCardCandidate, bool, error) {
	for _, candidate := range f.candidates {
		if candidate.SessionID == sessionID {
			return candidate, true, nil
		}
	}
	return episode.ThinCardCandidate{}, false, nil
}

// fakeSegmentFinder is the cross-session segment index the tool reads.
type fakeSegmentFinder struct {
	hits  []summarysegmentdao.UserSegmentHit
	terms []string
	err   error
}

func (f *fakeSegmentFinder) SearchUserSegmentsByText(_ context.Context, _ int64,
	terms []string, _ int) ([]summarysegmentdao.UserSegmentHit, error) {
	f.terms = terms
	if f.err != nil {
		return nil, f.err
	}
	return f.hits, nil
}

// fakeSegmentReader is the segment row the tool reads.
type fakeSegmentReader struct {
	segments map[string]*model.SessionSummarySegment
}

func (f *fakeSegmentReader) GetSummarySegment(_ context.Context, _ int64,
	sessionID string, seq int) (*model.SessionSummarySegment, error) {
	if segment, ok := f.segments[sessionID+":"+strconv.Itoa(seq)]; ok {
		return segment, nil
	}
	return nil, model.ErrSummarySegmentNotFound
}

// fakeWhiteboardReader is the board the tool reads.
type fakeWhiteboardReader struct {
	board *model.SessionWhiteboard
	err   error
	calls int
}

func (f *fakeWhiteboardReader) GetWhiteboard(_ context.Context, _ int64,
	_ string) (*model.SessionWhiteboard, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.board, nil
}

// thinCardRun is a run in a session that was shown no card at all.
func thinCardRun() serviceagent.RunContext {
	run := serviceagent.RunContext{UserID: 7, SessionID: "current-session",
		Question: "上次关于缓冲区复用的决定是什么"}
	run.GrantSet = serviceagent.NewHistoryGrantSet(run, nil)
	return run
}

// storedSegment builds one segment's stored body.
func storedSegment(t *testing.T, topic string, points []model.SessionSummarySegmentPoint) string {
	t.Helper()
	encoded, err := model.MarshalSessionSummarySegmentContent(model.SessionSummarySegmentContent{
		SchemaVersion: 1, Topic: topic, Points: points,
	})
	if err != nil {
		t.Fatalf("encode segment: %v", err)
	}
	return encoded
}

// storedBoard builds one board's stored body.
func storedBoard(t *testing.T, sessionID string, document model.SessionWhiteboardDocument) *model.SessionWhiteboard {
	t.Helper()
	encoded, err := model.MarshalSessionWhiteboardDocument(document)
	if err != nil {
		t.Fatalf("encode board: %v", err)
	}
	return &model.SessionWhiteboard{UserID: 7, SessionID: sessionID, Content: encoded}
}

func callCards(t *testing.T, search tool.BaseTool, session, query string) *SearchSessionCardsOutput {
	t.Helper()
	invokable, ok := search.(tool.InvokableTool)
	if !ok {
		t.Fatal("the card search tool is not invokable")
	}
	payload := `{"query":"` + query + `"`
	if session != "" {
		payload += `,"session":"` + session + `"`
	}
	payload += `}`
	raw, err := invokable.InvokableRun(context.Background(), payload)
	if err != nil {
		t.Fatal(err)
	}
	out := &SearchSessionCardsOutput{}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		t.Fatal(err)
	}
	return out
}

func callSegments(t *testing.T, search tool.BaseTool, query string) *SearchHistorySegmentsOutput {
	t.Helper()
	invokable, ok := search.(tool.InvokableTool)
	if !ok {
		t.Fatal("the segment search tool is not invokable")
	}
	raw, err := invokable.InvokableRun(context.Background(), `{"query":"`+query+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	out := &SearchHistorySegmentsOutput{}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		t.Fatal(err)
	}
	return out
}

func callWhiteboard(t *testing.T, read tool.BaseTool, session string) *ReadSessionWhiteboardOutput {
	t.Helper()
	invokable, ok := read.(tool.InvokableTool)
	if !ok {
		t.Fatal("the whiteboard read tool is not invokable")
	}
	raw, err := invokable.InvokableRun(context.Background(), `{"session":"`+session+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	out := &ReadSessionWhiteboardOutput{}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		t.Fatal(err)
	}
	return out
}

// The card search returns the conversations the question could be about, and
// opens them for the rest of the turn.
//
// It never opens the conversation being answered: a card about the present is
// not a door to the past.
func TestSearchSessionCardsReturnsCandidatesAndOpensTheirScope(t *testing.T) {
	retriever := &fakeThinCardRetriever{candidates: []episode.ThinCardCandidate{
		{SessionID: "old-session", CardID: "card-1", CardVersion: 3, Title: "RPC 拆包",
			Topics: []string{"rpc"}, Names: []string{"PacketBuffer"}, SegmentSeqs: []int{1, 2}},
		{SessionID: "current-session", CardID: "card-2", CardVersion: 1, Title: "现在这一轮"},
	}}
	registry, err := NewSearchSessionCardsRegistry(retriever)
	if err != nil {
		t.Fatal(err)
	}
	run := thinCardRun()
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	out := callCards(t, tools[0], "", "缓冲区复用")
	if out.Status != cardsStatusOK || out.Reason != cardsReasonNone {
		t.Fatalf("the search reported %q/%q", out.Status, out.Reason)
	}
	if len(out.Candidates) != 2 {
		t.Fatalf("the search found %d candidates", len(out.Candidates))
	}
	first := out.Candidates[0]
	if first.SessionID != "old-session" || first.CardID != "card-1" || first.CardVersion != 3 {
		t.Fatalf("the first candidate is %+v", first)
	}
	if first.Title != "RPC 拆包" || len(first.SegmentSeqs) != 2 {
		t.Fatalf("the first candidate lost the card's own words: %+v", first)
	}
	if out.ObservationTokens <= 0 {
		t.Fatal("the search did not report what it cost")
	}
	// The question was read into terms.
	if len(retriever.terms) == 0 {
		t.Fatal("the search passed no terms to the index")
	}
	// The found conversation is reachable for the rest of the turn.
	if !run.GrantSet.AuthorisesSession(run, "old-session") {
		t.Fatal("the conversation the search found was not opened")
	}
	cardID, version, _, ok := run.GrantSet.CardAuthorisation("old-session")
	if !ok || cardID != "card-1" || version != 3 {
		t.Fatalf("the opened conversation carries %q/%d", cardID, version)
	}
	// The run's own conversation was not opened as an earlier one.
	for _, session := range run.GrantSet.CardSessions() {
		if session == "current-session" {
			t.Fatal("the run's own conversation was added to the card scopes")
		}
	}
}

// A search that found nothing says so, and opens nothing.
func TestSearchSessionCardsWithNoMatchReportsNoEvidence(t *testing.T) {
	registry, err := NewSearchSessionCardsRegistry(&fakeThinCardRetriever{})
	if err != nil {
		t.Fatal(err)
	}
	run := thinCardRun()
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	out := callCards(t, tools[0], "", "根本没有这场对话")
	if out.Status != cardsStatusNoEvidence || out.Reason != cardsReasonNoMatch {
		t.Fatalf("the search reported %q/%q", out.Status, out.Reason)
	}
	if len(run.GrantSet.CardSessions()) != 0 {
		t.Fatal("a search that found nothing opened a conversation")
	}
}

// The segment search finds the stretch of turns AND the conversation it is in.
//
// A hit in a conversation no card describes is shown, but it is marked as one
// that cannot be opened, and nothing about that conversation is opened.
func TestSearchHistorySegmentsFindsASegmentAndOpensOnlyTheConversationWithACard(t *testing.T) {
	oldBody := storedSegment(t, "缓冲区复用", []model.SessionSummarySegmentPoint{
		{Text: "决定使用长度字段拆包", EvidenceMessageIDs: []uint{12}},
	})
	otherBody := storedSegment(t, "别的会话", []model.SessionSummarySegmentPoint{
		{Text: "另一段记录", EvidenceMessageIDs: []uint{20}},
	})
	finder := &fakeSegmentFinder{hits: []summarysegmentdao.UserSegmentHit{
		{SessionID: "old-session", Seq: 2, Topic: "缓冲区复用", Content: oldBody,
			EndMessageID: 14, Score: 3.5},
		{SessionID: "no-card-session", Seq: 1, Topic: "别的会话", Content: otherBody,
			EndMessageID: 21},
	}}
	reader := &fakeSegmentReader{segments: map[string]*model.SessionSummarySegment{
		"old-session:2": {UserID: 7, SessionID: "old-session", Seq: 2,
			StartMessageID: 11, EndMessageID: 14, Content: oldBody},
		"no-card-session:1": {UserID: 7, SessionID: "no-card-session", Seq: 1,
			StartMessageID: 19, EndMessageID: 21, Content: otherBody},
	}}
	cards := &fakeThinCardRetriever{candidates: []episode.ThinCardCandidate{
		{SessionID: "old-session", CardID: "card-1", CardVersion: 3},
	}}
	registry, err := NewSearchHistorySegmentsRegistry(finder, reader, cards)
	if err != nil {
		t.Fatal(err)
	}
	run := thinCardRun()
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	out := callSegments(t, tools[0], "缓冲区复用")
	if out.Status != segmentsStatusOK {
		t.Fatalf("the search reported %q/%q", out.Status, out.Reason)
	}
	if len(out.Segments) != 2 {
		t.Fatalf("the search found %d segments", len(out.Segments))
	}
	found := out.Segments[0]
	if found.SessionID != "old-session" || found.Seq != 2 {
		t.Fatalf("the hit carries %+v", found)
	}
	if found.StartMessageID != 11 || found.EndMessageID != 14 {
		t.Fatalf("the hit does not carry the stretch it covers: %+v", found)
	}
	if found.Text == "" {
		t.Fatalf("the hit carries no text: %+v", found)
	}
	if !found.Reachable {
		t.Fatalf("the conversation with a card was not reachable: %+v", found)
	}
	// The conversation with a card is opened for the rest of the turn.
	if !run.GrantSet.AuthorisesSession(run, "old-session") {
		t.Fatal("the conversation the search found was not opened")
	}
	// The message the segment cited is readable back.
	grants := run.GrantSet.Grants(12)
	if len(grants) == 0 {
		t.Fatal("the id the segment cited is not readable back")
	}
	if grants[0].SourceKind != serviceagent.HistorySourceSummarySegment ||
		grants[0].SourceSessionID != "old-session" {
		t.Fatalf("the grant carries %+v", grants[0])
	}
	// The conversation with no card is shown, and is NOT opened.
	unreachable := out.Segments[1]
	if unreachable.SessionID != "no-card-session" {
		t.Fatalf("the second hit carries %+v", unreachable)
	}
	if unreachable.Reachable {
		t.Fatalf("a conversation with no card was called reachable: %+v", unreachable)
	}
	if unreachable.UnreachableReason != segmentsReasonNoCard {
		t.Fatalf("the unreachable hit says %q", unreachable.UnreachableReason)
	}
	if run.GrantSet.AuthorisesSession(run, "no-card-session") {
		t.Fatal("a conversation with no card was opened")
	}
	// The search says that it did not run the semantic channel.
	if out.VectorChannel {
		t.Fatal("the search claimed a vector channel it does not have")
	}
	if out.Notes == "" {
		t.Fatal("the search did not say that it runs the lexical channel only")
	}
}

// The board of a conversation no card opened is not read.
//
// The check is made before the read, so the board is never even fetched.
func TestReadSessionWhiteboardRefusesAConversationNoCardOpened(t *testing.T) {
	reader := &fakeWhiteboardReader{board: storedBoard(t, "other-session", model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV1,
		Goal:          &model.SessionWhiteboardGoal{Text: "把拆包写完"},
	})}
	registry, err := NewReadSessionWhiteboardRegistry(reader)
	if err != nil {
		t.Fatal(err)
	}
	run := thinCardRun()
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	out := callWhiteboard(t, tools[0], "other-session")
	if out.Status != whiteboardStatusNoEvidence || out.Reason != whiteboardReasonNotAllowed {
		t.Fatalf("the read reported %q/%q", out.Status, out.Reason)
	}
	if reader.calls != 0 {
		t.Fatalf("the board was read %d times for a conversation the run may not open", reader.calls)
	}
}

// The board of a conversation a card opened is read, and only its current
// state is shown.
func TestReadSessionWhiteboardShowsTheBoardOfAnOpenedConversation(t *testing.T) {
	reader := &fakeWhiteboardReader{board: storedBoard(t, "old-session", model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV2,
		Goal:          &model.SessionWhiteboardGoal{Text: "把拆包写完"},
		Plan:          []model.SessionWhiteboardEntry{{Key: "step", Text: "先写协议"}},
		OpenItems:     []model.SessionWhiteboardEntry{{Key: "bench", Text: "压测还没做"}},
		Constraints:   []model.SessionWhiteboardEntry{{Key: "lang", Text: "只用 Go"}},
	})}
	registry, err := NewReadSessionWhiteboardRegistry(reader)
	if err != nil {
		t.Fatal(err)
	}
	run := thinCardRun()
	run.GrantSet.ExtendCardScope(run, serviceagent.CardScope{
		SessionID: "old-session", CardID: "card-1", CardVersion: 3,
	})
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	out := callWhiteboard(t, tools[0], "old-session")
	if out.Status != whiteboardStatusOK {
		t.Fatalf("the read reported %q/%q", out.Status, out.Reason)
	}
	if out.Goal != "把拆包写完" {
		t.Fatalf("the goal is %q", out.Goal)
	}
	if len(out.Plan) != 1 || out.Plan[0].Key != "step" || out.Plan[0].Text != "先写协议" {
		t.Fatalf("the plan is %+v", out.Plan)
	}
	if len(out.OpenItems) != 1 || out.OpenItems[0].Text != "压测还没做" {
		t.Fatalf("the open items are %+v", out.OpenItems)
	}
	if len(out.Constraints) != 1 || out.Constraints[0].Key != "lang" {
		t.Fatalf("the constraints are %+v", out.Constraints)
	}
	if reader.calls != 1 {
		t.Fatalf("the board was read %d times", reader.calls)
	}
	if out.ObservationTokens <= 0 {
		t.Fatal("the read did not report what it cost")
	}
}

// A board that does not exist is a fact about the board, not a failure.
func TestReadSessionWhiteboardReportsAMissingBoard(t *testing.T) {
	reader := &fakeWhiteboardReader{err: model.ErrWhiteboardNotFound}
	registry, err := NewReadSessionWhiteboardRegistry(reader)
	if err != nil {
		t.Fatal(err)
	}
	run := thinCardRun()
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	out := callWhiteboard(t, tools[0], "current-session")
	if out.Status != whiteboardStatusNoEvidence || out.Reason != whiteboardReasonNoBoard {
		t.Fatalf("the read reported %q/%q", out.Status, out.Reason)
	}
}

// ExtendCardScope keeps the rules the scope was built with.
//
// A scope without a card opens nothing, the run's own conversation is never
// an earlier one, and a scope that is already there is not replaced: the
// first card that opened a conversation is the one every read in it is
// checked against.
func TestExtendCardScopeKeepsTheRulesOfTheScope(t *testing.T) {
	run := thinCardRun()
	run.GrantSet.ExtendCardScope(run, serviceagent.CardScope{
		SessionID: "old-session", CardID: "", CardVersion: 1})
	run.GrantSet.ExtendCardScope(run, serviceagent.CardScope{
		SessionID: "old-session", CardID: "card-1", CardVersion: 0})
	run.GrantSet.ExtendCardScope(run, serviceagent.CardScope{
		SessionID: "current-session", CardID: "card-2", CardVersion: 1})
	run.GrantSet.ExtendCardScope(run, serviceagent.CardScope{
		SessionID: "", CardID: "card-3", CardVersion: 1})
	if len(run.GrantSet.CardSessions()) != 0 {
		t.Fatalf("a refused scope was added: %v", run.GrantSet.CardSessions())
	}
	run.GrantSet.ExtendCardScope(run, serviceagent.CardScope{
		SessionID: "old-session", CardID: "card-1", CardVersion: 3})
	if !run.GrantSet.AuthorisesSession(run, "old-session") {
		t.Fatal("a real scope did not open the conversation")
	}
	run.GrantSet.ExtendCardScope(run, serviceagent.CardScope{
		SessionID: "old-session", CardID: "card-9", CardVersion: 9})
	cardID, version, _, ok := run.GrantSet.CardAuthorisation("old-session")
	if !ok || cardID != "card-1" || version != 3 {
		t.Fatalf("the scope was replaced by a later one: %q/%d", cardID, version)
	}
}

// The three tools are capabilities of their own, and their names say what
// they mean on every surface.
func TestTheOnDemandToolsHaveTheirOwnCapabilities(t *testing.T) {
	for name, want := range map[string]serviceagent.MemoryToolCapability{
		SearchSessionCardsName:    serviceagent.CapabilitySearchSessionCards,
		SearchHistorySegmentsName: serviceagent.CapabilitySearchHistorySegments,
		ReadSessionWhiteboardName: serviceagent.CapabilityReadSessionWhiteboard,
	} {
		if got := serviceagent.CapabilityOf(name); got != want {
			t.Errorf("CapabilityOf(%q) = %q, want %q", name, got, want)
		}
	}
	for _, surface := range []serviceagent.ToolSurface{serviceagent.SurfaceLegacy,
		serviceagent.SurfaceExplicitActions, serviceagent.SurfaceExplicitSources,
		serviceagent.SurfaceSourcePriority} {
		for _, capability := range []serviceagent.MemoryToolCapability{
			serviceagent.CapabilitySearchSessionCards,
			serviceagent.CapabilitySearchHistorySegments,
			serviceagent.CapabilityReadSessionWhiteboard,
		} {
			name, err := serviceagent.ToolNameFor(capability, surface)
			if err != nil {
				t.Fatalf("%s has no name on %s: %v", capability, surface, err)
			}
			if got := serviceagent.CapabilityOf(name); got != capability {
				t.Errorf("%s on %s is called %s, which reads back as %s", capability, surface, name, got)
			}
		}
	}
}
