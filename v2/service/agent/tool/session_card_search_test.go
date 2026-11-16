package agenttool

import (
	"context"
	"encoding/json"
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
