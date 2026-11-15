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
