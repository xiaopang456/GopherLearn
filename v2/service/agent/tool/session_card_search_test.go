package agenttool

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"GopherAI/model"
	serviceagent "GopherAI/service/agent"
	"github.com/cloudwego/eino/components/tool"
)

// fakeSessionSearcher is the store the search tool reads.
type fakeSessionSearcher struct {
	messages []model.Message
	calls    int
}

func (f *fakeSessionSearcher) SearchOwnedMessagesBySession(_ context.Context, userID int64,
	sessionID string, keyword string, limit int) ([]model.Message, error) {
	f.calls++
	var out []model.Message
	for _, message := range f.messages {
		if message.SessionID != sessionID {
			continue
		}
		if !containsText(message.Content, keyword) {
			continue
		}
		out = append(out, message)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func containsText(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// cardRun is a run in a session that was shown a card about an earlier
// conversation.
func cardRun() serviceagent.RunContext {
	run := serviceagent.RunContext{UserID: 7, SessionID: "current-session", Question: "上次那道题我怎么写的"}
	run.HistoryGrants = []serviceagent.HistoryMessageGrant{{
		MessageID: 12, SourceKind: serviceagent.HistorySourceSessionCard,
		SourceSessionID: "source-session", CardID: "card-1", CardVersion: 2, CardWatermark: 144,
	}}
	run.GrantSet = serviceagent.NewHistoryGrantSet(run, run.HistoryGrants)
	// The scope is what opens the conversation. The message grants say which
	// id in it may be read; the scope says that the conversation itself is
	// reachable, which is what lets the model browse and search it.
	run.GrantSet.SetCardScope(serviceagent.CardScope{
		SessionID: "source-session", CardID: "card-1", CardVersion: 2, CardWatermark: 144,
	})
	return run
}

func callSearch(t *testing.T, search tool.BaseTool, session, query string) *SearchSessionHistoryOutput {
	t.Helper()
	invokable, ok := search.(tool.InvokableTool)
	if !ok {
		t.Fatal("the search tool is not invokable")
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
	out := &SearchSessionHistoryOutput{}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSearchSessionHistoryFindsTheWordInTheSourceSession(t *testing.T) {
	searcher := &fakeSessionSearcher{messages: []model.Message{
		{ID: 12, SessionID: "source-session", IsUser: true, Content: "我用的是长度前缀，不是分隔符"},
		{ID: 13, SessionID: "source-session", IsUser: false, Content: "那我们先看分隔符的边界"},
	}}
	registry, err := NewSearchSessionHistoryRegistry(searcher)
	if err != nil {
		t.Fatal(err)
	}
	// The deployment's default is that the summary records the assistant's
	// products, and the wiring passes that switch to this tool: the
	// assistant's own words are searchable in that mode. The mode that does
	// not record them has its own test below.
	registry.UseAssistantContent(true)
	run := cardRun()
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 {
		t.Fatalf("the tool table holds %d tools", len(tools))
	}
	out := callSearch(t, tools[0], "source-session", "边界")
	if out.Status != searchSessionStatusOK {
		t.Fatalf("the search reported %q", out.Status)
	}
	if len(out.Matches) != 1 || out.Matches[0].MessageID != 13 {
		t.Fatalf("the search found %+v", out.Matches)
	}
	// The id the search showed is now readable back in full.
	if len(run.GrantSet.Grants(13)) == 0 {
		t.Fatal("the searched message is not readable back")
	}
}

// A session no card pointed at is not reachable, and the reason says so.
func TestSearchSessionHistoryRefusesASessionNoCardPointedAt(t *testing.T) {
	searcher := &fakeSessionSearcher{messages: []model.Message{
		{ID: 12, SessionID: "other-session", IsUser: true, Content: "另一段会话里的原话"},
	}}
	registry, err := NewSearchSessionHistoryRegistry(searcher)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := registry.Tools(context.Background(), cardRun())
	if err != nil {
		t.Fatal(err)
	}
	out := callSearch(t, tools[0], "other-session", "原话")
	if out.Status != searchSessionStatusNoEvidence || out.Reason != searchSessionReasonNotAllowed {
		t.Fatalf("the search reported %q/%q", out.Status, out.Reason)
	}
	if len(out.Matches) != 0 {
		t.Fatalf("a refused search returned %+v", out.Matches)
	}
	if searcher.calls != 0 {
		t.Fatal("a refused search reached the store")
	}
}

// The card's own watermark bounds the search: the card describes the session
// as far as it had reached, and a later message is not something it saw.
func TestSearchSessionHistoryStopsAtTheCardWatermark(t *testing.T) {
	searcher := &fakeSessionSearcher{messages: []model.Message{
		{ID: 200, SessionID: "source-session", IsUser: true, Content: "后来才说的另一句话"},
	}}
	registry, err := NewSearchSessionHistoryRegistry(searcher)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := registry.Tools(context.Background(), cardRun())
	if err != nil {
		t.Fatal(err)
	}
	out := callSearch(t, tools[0], "source-session", "另一句")
	if out.Status != searchSessionStatusNoEvidence || out.Reason != searchSessionReasonNoMatch {
		t.Fatalf("the search reported %q/%q", out.Status, out.Reason)
	}
	if len(out.Matches) != 0 {
		t.Fatalf("a message above the watermark was returned: %+v", out.Matches)
	}
}

// The run's own conversation is always searchable, and it needs no card.
func TestSearchSessionHistoryReadsTheRunsOwnSession(t *testing.T) {
	searcher := &fakeSessionSearcher{messages: []model.Message{
		{ID: 3, SessionID: "current-session", IsUser: true, Content: "当前会话里的原话"},
	}}
	registry, err := NewSearchSessionHistoryRegistry(searcher)
	if err != nil {
		t.Fatal(err)
	}
	run := serviceagent.RunContext{UserID: 7, SessionID: "current-session", Question: "我刚说了什么"}
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	out := callSearch(t, tools[0], "", "原话")
	if out.Status != searchSessionStatusOK || len(out.Matches) != 1 {
		t.Fatalf("the search reported %q and %+v", out.Status, out.Matches)
	}
}

// The whole third step: a hit the search found is readable back in full.
//
// The search finds the detail the card and the summary did not keep, so the
// message it found is outside the card's citation closure. Registered as a
// card citation, every hit the search exists to find would be refused at the
// read-back with "not in the card's closure" - and the chain would stop at
// the search, with the model holding a message id it could never read.
func TestASearchHitIsReadableBackInFull(t *testing.T) {
	searcher := &fakeSessionSearcher{messages: []model.Message{
		{ID: 13, SessionID: "source-session", IsUser: true, Content: "细节：我用的是长度前缀"},
	}}
	registry, err := NewSearchSessionHistoryRegistry(searcher)
	if err != nil {
		t.Fatal(err)
	}
	run := cardRun()
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	out := callSearch(t, tools[0], "source-session", "长度前缀")
	if out.Status != searchSessionStatusOK || len(out.Matches) != 1 {
		t.Fatalf("the search reported %q and %+v", out.Status, out.Matches)
	}
	// The grant the search issued is a search grant, not a card citation.
	held := run.GrantSet.Grants(13)
	if len(held) == 0 {
		t.Fatal("the searched message is not readable back")
	}
	for _, grant := range held {
		if grant.SourceKind != serviceagent.HistorySourceSessionSearch {
			t.Fatalf("the grant has the wrong kind: %+v", grant)
		}
		if grant.SourceSessionID != "source-session" {
			t.Fatalf("the grant names the session %q", grant.SourceSessionID)
		}
	}
	// And the read-back tool honours it: the store agrees that the card is
	// there, and the message is read.
	reader := &fakeHistoryReader{rows: map[uint]model.Message{
		13: userMessage(13, "source-session", "细节：我用的是长度前缀"),
	}}
	readTools := newHistoryToolsWithEvidence(t, reader, agreeingCardEvidence(), historyRun(
		serviceagent.HistoryMessageGrant{
			MessageID: 13, SourceKind: serviceagent.HistorySourceSessionSearch,
			SourceSessionID: "source-session", CardID: "card-1", CardVersion: 2, CardWatermark: 144,
		}))
	read := invokeRead(t, readTools, 13)
	if len(read.Messages) != 1 || read.Messages[0].MessageID != 13 {
		t.Fatalf("the search hit was not read back: %+v", read)
	}
}

// agreeingCardEvidence reads a card back the way the store would when
// nothing has changed since the render.
func agreeingCardEvidence() EpisodeEvidenceReader {
	return EpisodeEvidenceReaderFunc(func(_ context.Context, userID int64, episodeID string) (EpisodeEvidenceFacts, error) {
		return EpisodeEvidenceFacts{EpisodeID: episodeID, UserID: userID,
			SessionID: "source-session", Version: 2, Status: string(model.EpisodeStatusActive),
			IndexStatus: string(model.EpisodeIndexIndexed), Watermark: 144,
			ContentKind: string(model.EpisodeContentSessionCard),
			// The card cites message 12, and not the searched one. The
			// searched message is outside the closure by definition, which
			// is the whole point of the search.
			EvidenceMessageIDs: []uint{12},
		}, nil
	})
}

// The tool is a known capability, so the runner's bookkeeping finds it.
func TestSearchSessionHistoryIsAKnownCapability(t *testing.T) {
	if got := serviceagent.CapabilityOf(SearchSessionHistoryName); got != serviceagent.CapabilitySearchSessionHistory {
		t.Fatalf("the tool maps to %q", got)
	}
	name, err := serviceagent.ToolNameFor(serviceagent.CapabilitySearchSessionHistory, serviceagent.SurfaceLegacy)
	if err != nil {
		t.Fatal(err)
	}
	if name != SearchSessionHistoryName {
		t.Fatalf("the capability names the tool %q", name)
	}
}

// A search in this run's own conversation has no card behind it.
//
// The third step of the chain - card, then summary, then the original words -
// does not stop at the run's own conversation: a detail the summary of this
// very conversation did not keep is still a detail the model has to be able to
// read back. There is no card to confirm such a hit against, because the
// search looked in the conversation the run is already in; what authorises it
// is the owner and the conversation, and the read-back query binds both again.
func TestASearchHitInThisConversationIsReadableBackInFull(t *testing.T) {
	searcher := &fakeSessionSearcher{messages: []model.Message{
		{ID: 21, SessionID: historyRunSession, IsUser: true, Content: "\u7ec6\u8282\uff1a\u6211\u7528\u7684\u662f\u957f\u5ea6\u524d\u7f00"},
	}}
	registry, err := NewSearchSessionHistoryRegistry(searcher)
	if err != nil {
		t.Fatal(err)
	}
	run := serviceagent.RunContext{UserID: 7, SessionID: historyRunSession, Question: "\u6211\u521a\u624d\u600e\u4e48\u5199\u7684"}
	run.GrantSet = serviceagent.NewHistoryGrantSet(run, nil)
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	out := callSearch(t, tools[0], "", "\u957f\u5ea6\u524d\u7f00")
	if out.Status != searchSessionStatusOK || len(out.Matches) != 1 {
		t.Fatalf("the search reported %q and %+v", out.Status, out.Matches)
	}
	held := run.GrantSet.Grants(21)
	if len(held) == 0 {
		t.Fatal("the searched message is not readable back")
	}
	for _, grant := range held {
		if grant.SourceKind != serviceagent.HistorySourceSessionSearch {
			t.Fatalf("the grant has the wrong kind: %+v", grant)
		}
		if grant.SourceSessionID != historyRunSession {
			t.Fatalf("the grant names the session %q", grant.SourceSessionID)
		}
		if grant.CardID != "" {
			t.Fatalf("a hit in this run's own conversation names a card: %+v", grant)
		}
	}
	// The read-back must not go looking for a card: there is none, and a
	// verifier that is asked anyway would refuse the hit.
	reader := &fakeHistoryReader{rows: map[uint]model.Message{
		21: userMessage(21, historyRunSession, "\u7ec6\u8282\uff1a\u6211\u7528\u7684\u662f\u957f\u5ea6\u524d\u7f00"),
	}}
	evidence := EpisodeEvidenceReaderFunc(func(_ context.Context, _ int64, episodeID string) (EpisodeEvidenceFacts, error) {
		return EpisodeEvidenceFacts{}, fmt.Errorf("no card should be consulted for %q", episodeID)
	})
	readTools := newHistoryToolsWithEvidence(t, reader, evidence, historyRun(
		serviceagent.HistoryMessageGrant{
			MessageID: 21, SourceKind: serviceagent.HistorySourceSessionSearch,
			SourceSessionID: historyRunSession,
		}))
	read := invokeRead(t, readTools, 21)
	if len(read.Messages) != 1 || read.Messages[0].MessageID != 21 {
		t.Fatalf("the search hit was not read back: %+v", read)
	}
}

// A search hit may not name another conversation without a card.
//
// The card is what opens an earlier conversation, so a hit that claims to come
// from one and carries no card is refused rather than believed.
func TestASearchHitInAnotherConversationNeedsACard(t *testing.T) {
	reader := &fakeHistoryReader{rows: map[uint]model.Message{
		22: userMessage(22, "session-elsewhere", "\u522b\u4eba\u7684\u539f\u8bdd"),
	}}
	readTools := newHistoryTools(t, reader, historyRun(
		summaryGrant(5, historyRunSession),
		serviceagent.HistoryMessageGrant{
			MessageID: 22, SourceKind: serviceagent.HistorySourceSessionSearch,
			SourceSessionID: "session-elsewhere",
		}))
	read := invokeRead(t, readTools, 22)
	if len(read.Messages) != 0 {
		t.Fatalf("a hit without a card was read back: %+v", read)
	}
	if len(read.Refused) != 1 || read.Refused[0] != 22 {
		t.Fatalf("the hit was not refused: %+v", read)
	}
}

// The search and the read-back agree about the assistant's own words.
//
// A deployment that does not record the assistant's products does not get
// them back through a search either. The read-back path refuses an assistant
// row when the switch is off, so a search that showed it would hand the model
// words it may not quote - the one place where the two paths would disagree.
func TestTheSearchAndTheReadBackAgreeAboutTheAssistantsWords(t *testing.T) {
	searcher := &fakeSessionSearcher{messages: []model.Message{
		{ID: 31, SessionID: "source-session", IsUser: true, Content: "\u6211\u7528\u957f\u5ea6\u524d\u7f00\u62c6\u5305"},
		{ID: 32, SessionID: "source-session", IsUser: false, Content: "\u52a9\u624b\u7ed9\u7684\u62c6\u5305\u7ec3\u4e60"},
	}}
	registry, err := NewSearchSessionHistoryRegistry(searcher)
	if err != nil {
		t.Fatal(err)
	}
	// Off: the learner's own words come back, the assistant's do not.
	registry.UseAssistantContent(false)
	run := cardRun()
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	out := callSearch(t, tools[0], "source-session", "\u62c6\u5305")
	if out.Status != searchSessionStatusOK || len(out.Matches) != 1 {
		t.Fatalf("the search returned %q and %+v", out.Status, out.Matches)
	}
	if out.Matches[0].MessageID != 31 {
		t.Fatalf("the search returned %+v", out.Matches)
	}
	for _, grant := range run.GrantSet.Grants(32) {
		t.Fatalf("the assistant's row was authorised: %+v", grant)
	}
	// On: both come back.
	registry.UseAssistantContent(true)
	onRun := cardRun()
	onTools, err := registry.Tools(context.Background(), onRun)
	if err != nil {
		t.Fatal(err)
	}
	onOut := callSearch(t, onTools[0], "source-session", "\u62c6\u5305")
	if onOut.Status != searchSessionStatusOK || len(onOut.Matches) != 2 {
		t.Fatalf("the search returned %q and %+v", onOut.Status, onOut.Matches)
	}
}
