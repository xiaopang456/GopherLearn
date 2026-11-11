package agenttool

import (
	"context"
	"testing"

	"GopherAI/model"
	serviceagent "GopherAI/service/agent"

	"github.com/cloudwego/eino/components/tool"
)

// The assistant's own messages may be read back, but only the ones a summary
// segment or the board cites.
//
// The grant set still decides which ids exist for the run, so an assistant
// message nobody cited is not readable in this mode either. What the mode
// changes is that a cited one may be.

// assistantMessage is one of the assistant's rows.
func assistantMessage(id uint, sessionID, content string) model.Message {
	return model.Message{ID: id, SessionID: sessionID, IsUser: false, Content: content}
}

// newAssistantHistoryTools builds the tool in the mode that also records what
// the assistant produced.
func newAssistantHistoryTools(t *testing.T, reader HistoryMessageReader, run serviceagent.RunContext) []tool.BaseTool {
	t.Helper()
	registry, err := NewHistoryMessageRegistry(reader)
	if err != nil {
		t.Fatal(err)
	}
	registry.UseEpisodeEvidenceReader(agreeingEpisodeEvidence(run))
	registry.UseAssistantContent(true)
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	return tools
}

// An assistant's message that a card cites comes back, marked as the
// assistant's.
func TestTheAssistantsCitedMessageIsReadableBack(t *testing.T) {
	reader := &fakeHistoryReader{rows: map[uint]model.Message{
		21: assistantMessage(21, historyRunSession, "1. 斐波那契 2. 背包 3. 区间 DP"),
	}}
	tools := newAssistantHistoryTools(t, reader, historyRun(summaryGrant(21, historyRunSession)))

	out := invokeRead(t, tools, 21)

	if len(out.Messages) != 1 {
		t.Fatalf("messages=%+v want the cited assistant message", out.Messages)
	}
	if out.Messages[0].Role != "assistant" {
		t.Fatalf("role=%q want assistant", out.Messages[0].Role)
	}
	if out.Messages[0].Content == "" {
		t.Fatal("the message came back empty")
	}
}

// An assistant's message that no card cited is still not readable.
//
// The grant set is what bounds the read, and the mode does not widen it.
func TestTheAssistantsUncitedMessageIsStillRefused(t *testing.T) {
	reader := &fakeHistoryReader{rows: map[uint]model.Message{
		22: assistantMessage(22, historyRunSession, "没有一条摘要引用我"),
	}}
	tools := newAssistantHistoryTools(t, reader, historyRun(summaryGrant(21, historyRunSession)))

	out := invokeRead(t, tools, 22)

	if len(out.Messages) != 0 {
		t.Fatalf("messages=%+v want nothing", out.Messages)
	}
	if len(out.Refused) != 1 || out.Refused[0] != 22 {
		t.Fatalf("refused=%v want the uncited id reported", out.Refused)
	}
	if reader.requestCount() != 0 {
		t.Fatalf("the database was queried for an uncited id: %v", reader.scopes)
	}
}

// The mode that does not record the assistant's products returns only the
// learner's own words, exactly as it always did.
func TestTheAssistantsMessageIsNotReadableInTheOffMode(t *testing.T) {
	reader := &fakeHistoryReader{rows: map[uint]model.Message{
		21: assistantMessage(21, historyRunSession, "1. 斐波那契 2. 背包 3. 区间 DP"),
	}}
	tools := newHistoryTools(t, reader, historyRun(summaryGrant(21, historyRunSession)))

	out := invokeRead(t, tools, 21)

	if len(out.Messages) != 0 {
		t.Fatalf("messages=%+v want nothing", out.Messages)
	}
	// The mode that is off behaves exactly as it did: a granted assistant
	// row is skipped rather than reported.
	if len(out.Refused) != 0 {
		t.Fatalf("refused=%v want nothing", out.Refused)
	}
}

// The two modes promise different things, and the model has to be able to
// read which one it is in.
func TestTheTwoModesHaveTwoContracts(t *testing.T) {
	base, err := ReadHistoryMessageContractDigest()
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := ReadHistoryMessageAssistantContractDigest()
	if err != nil {
		t.Fatal(err)
	}
	if base == assistant {
		t.Fatal("the two modes digest the same contract")
	}
	if readHistoryMessageDescription == readHistoryMessageAssistantDescription {
		t.Fatal("the two modes show the same description")
	}
}
