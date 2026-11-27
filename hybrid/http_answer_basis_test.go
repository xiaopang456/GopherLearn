package hybrid

import (
	"GopherAI/model"
	"testing"

	"GopherAI/common/rag"
)

// The basis of an answer has to survive a reload.
//
// The server keeps the basis on the message row. The history read has to
// return it, or a learner who opens the conversation again sees an answer
// with no basis at all - and a turn that was grounded in a note they picked
// looks like it came from nowhere.
//
// The row is written directly here rather than through a live turn: the point
// of the test is the serializer, and a live turn would only add a second
// thing that could fail.
func TestHistoryCarriesTheBasisOfTheAnswer(t *testing.T) {
	app, db, _, token, _ := httpFixture(t)
	session := dataHTTP(t, app.Handler, "POST", "/api/v1/sessions",
		`{"mode":"learning_agent","title":"依据读回","requestId":"basis-read-session"}`, token)["id"].(string)

	if err := db.Create(&model.Message{
		SessionID: session, UserName: "learner", Content: "现值看白板，旧值翻档案。",
		AnswerBasis:       rag.AnswerBasisTopicMemory,
		KnowledgeStatus:   rag.KnowledgeStatusNotUsed,
		KnowledgeReason:   rag.KnowledgeReasonNotCited,
		AnswerBasisTopics: model.MessageTopics{"分层摘要"},
	}).Error; err != nil {
		t.Fatal(err)
	}

	items := dataHTTP(t, app.Handler, "GET", "/api/v1/sessions/"+session+"/messages", "", token)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("history items %v", items)
	}
	row := items[0].(map[string]any)
	if row["answer_basis"] != "topic_memory" {
		t.Fatalf("the history row lost its basis: %v", row)
	}
	if row["knowledge_status"] != "not_used" || row["knowledge_reason"] != "not_cited" {
		t.Fatalf("the history row lost the retrieval state: %v", row)
	}
	topics, ok := row["answer_basis_topics"].([]any)
	if !ok || len(topics) != 1 || topics[0] != "分层摘要" {
		t.Fatalf("the history row lost the subject: %v", row)
	}
}

// A row stored before the server kept these fields must not be given a basis
// it never had.
func TestHistoryDoesNotInventABasisForAnOldRow(t *testing.T) {
	app, db, _, token, _ := httpFixture(t)
	session := dataHTTP(t, app.Handler, "POST", "/api/v1/sessions",
		`{"mode":"learning_agent","title":"旧消息","requestId":"basis-legacy-session"}`, token)["id"].(string)

	if err := db.Create(&model.Message{
		SessionID: session, UserName: "learner", Content: "很久以前的一条消息",
	}).Error; err != nil {
		t.Fatal(err)
	}

	items := dataHTTP(t, app.Handler, "GET", "/api/v1/sessions/"+session+"/messages", "", token)["items"].([]any)
	row := items[0].(map[string]any)
	if _, present := row["answer_basis"]; present {
		t.Fatalf("an old row was given a basis: %v", row)
	}
}
