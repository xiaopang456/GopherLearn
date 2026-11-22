package hybrid

import (
	"encoding/json"
	"fmt"
	"testing"

	"GopherAI/model"
	agentService "GopherAI/service/agent"
)

// The clarification reaches the client.
//
// The learner may answer the clarification minutes later, in a conversation
// they have reloaded in between. A payload that survives the store but not
// the read-back is a payload the learner can never answer: the card is gone
// from the message, and the question that was waiting for an answer is gone
// with it.
//
// The row a client reads immediately and the same row after a reload are the
// same view, so a row that carries the clarification in one carries it in
// the other.
func TestTheClarificationReachesTheClientAfterAReload(t *testing.T) {
	app, db, _, token, _ := httpFixture(t)
	session := createHTTP(t, app.Handler, token, "澄清")
	var row model.Session
	if err := db.Where("id = ?", session).First(&row).Error; err != nil {
		t.Fatalf("read the session: %v", err)
	}
	assistant := &model.Message{
		SessionID: session, UserName: row.UserName, Content: "你在问哪一次？", IsUser: false,
		Clarification: &model.SessionClarification{
			ClarificationID:    "clarification-1",
			Question:           "上次你给我排的那三道题里，第二道练的是什么？",
			AllowFreeText:      true,
			AllowHistoryBrowse: true,
			Candidates: []model.SessionClarificationCandidate{
				{SessionID: "l4r2-2026am-a1-03", Title: "洄漪"},
			},
		},
	}
	if err := db.Create(assistant).Error; err != nil {
		t.Fatalf("write the assistant message: %v", err)
	}

	items := dataHTTP(t, app.Handler, "GET", "/api/v1/sessions/"+session+"/messages", "", token)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("the history holds %d messages, want the one that was written", len(items))
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("the item is %T", items[0])
	}
	clarification, ok := item["clarification"].(map[string]any)
	if !ok {
		t.Fatalf("the message carries no clarification: %+v", item)
	}
	if clarification["question"] != "上次你给我排的那三道题里，第二道练的是什么？" {
		t.Fatalf("the clarification carries %v", clarification["question"])
	}
	candidates, ok := clarification["candidates"].([]any)
	if !ok || len(candidates) != 1 {
		t.Fatalf("the clarification carries %v", clarification["candidates"])
	}
	candidate, ok := candidates[0].(map[string]any)
	if !ok || candidate["sessionId"] != "l4r2-2026am-a1-03" {
		t.Fatalf("the candidate is %+v", candidates[0])
	}
	if candidate["title"] != "洄漪" {
		t.Fatalf("the candidate lost its title: %+v", candidate)
	}
	if clarification["allowFreeText"] != true {
		t.Fatalf("the free-text permission did not reach the client: %+v", clarification)
	}
	// The learner's own conversation list stays reachable after a reload: it
	// is the second of the clarification's three operations, and a target
	// that never made it into the candidate list is only reachable through it.
	if clarification["allowHistoryBrowse"] != true {
		t.Fatalf("the history browse did not reach the client: %+v", clarification)
	}
}

// The read-back and the live turn carry the SAME shape.
//
// Two shapes for one card is how the candidate buttons lost their titles
// after a reload: the live turn carries the product's own card, and a
// hand-written second shape on the read-back path drifted from it.
func TestTheReadBackCarriesTheSameShapeAsTheLiveTurn(t *testing.T) {
	app, db, _, token, _ := httpFixture(t)
	session := createHTTP(t, app.Handler, token, "形状")
	var row model.Session
	if err := db.Where("id = ?", session).First(&row).Error; err != nil {
		t.Fatalf("read the session: %v", err)
	}
	clarification := &model.SessionClarification{
		ClarificationID:    "clarification-1",
		Question:           "上次你给我排的那三道题里，第二道练的是什么？",
		AllowFreeText:      true,
		AllowHistoryBrowse: true,
		Candidates: []model.SessionClarificationCandidate{
			{SessionID: "l4r2-2026am-a1-03", Title: "洄漪", Topics: []string{"慢日志与锁"}},
		},
	}
	assistant := &model.Message{SessionID: session, UserName: row.UserName,
		Content: "你在问哪一次？", IsUser: false, Clarification: clarification}
	if err := db.Create(assistant).Error; err != nil {
		t.Fatalf("write the assistant message: %v", err)
	}

	items := dataHTTP(t, app.Handler, "GET", "/api/v1/sessions/"+session+"/messages", "", token)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("the history holds %d messages, want the one that was written", len(items))
	}
	readBack, ok := items[0].(map[string]any)["clarification"].(map[string]any)
	if !ok {
		t.Fatalf("the message carries no clarification: %+v", items[0])
	}
	live, err := json.Marshal(agentService.ClarificationCardOf(clarification))
	if err != nil {
		t.Fatal(err)
	}
	liveShape := map[string]any{}
	if err := json.Unmarshal(live, &liveShape); err != nil {
		t.Fatal(err)
	}
	for key, value := range liveShape {
		got, present := readBack[key]
		if !present {
			t.Fatalf("the read-back carries no %q: %+v", key, readBack)
		}
		if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", value) {
			t.Fatalf("the read-back's %q is %v, the live turn's is %v", key, got, value)
		}
	}
}

// A message with no clarification carries none.
//
// The column is added to a table that is already full of rows, and the rows
// that predate it hold nothing in it. A client that is handed a
// clarification for every message would show a card on every reply.
func TestAMessageWithoutAClarificationCarriesNone(t *testing.T) {
	app, db, _, token, _ := httpFixture(t)
	session := createHTTP(t, app.Handler, token, "没有澄清")
	var row model.Session
	if err := db.Where("id = ?", session).First(&row).Error; err != nil {
		t.Fatalf("read the session: %v", err)
	}
	older := &model.Message{
		SessionID: session, UserName: row.UserName, Content: "一句没有澄清的老消息", IsUser: false,
	}
	if err := db.Create(older).Error; err != nil {
		t.Fatalf("write the old message: %v", err)
	}
	items := dataHTTP(t, app.Handler, "GET", "/api/v1/sessions/"+session+"/messages", "", token)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("the history holds %d messages", len(items))
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("the item is %T", items[0])
	}
	if _, present := item["clarification"]; present {
		t.Fatalf("a message with no clarification carries one: %+v", item)
	}
}
