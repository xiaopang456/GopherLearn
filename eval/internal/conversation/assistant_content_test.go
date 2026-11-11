package conversation

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"GopherAI/model"
	serviceagent "GopherAI/service/agent"
	agentTool "GopherAI/service/agent/tool"
)

// The read-back tool's contract is not the same in the two modes.
//
// The summary that records what the assistant produced cites the assistant's
// own messages, so the tool that reads a cited message back has to promise
// something more: it may return a line that is the assistant's words. A tool
// whose contract changed under a measurement binds the number to bytes nobody
// measured, so the two contracts are separate strings with separate digests.
//
// The harness has to know which one a run used. The test reads the tool the
// model is offered, in both modes, and says what it promises.
func TestTheReadBackToolOffersTheContractOfTheModeItRunsIn(t *testing.T) {
	base, err := agentTool.ReadHistoryMessageContractDigest()
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := agentTool.ReadHistoryMessageAssistantContractDigest()
	if err != nil {
		t.Fatal(err)
	}
	if base == assistant {
		t.Fatal("the two contracts have the same digest, so a run cannot say which one it used")
	}
	for _, mode := range []string{"off", "on"} {
		description := descriptionOfTheReadBackTool(t, mode == "on")
		if mode == "on" {
			if !strings.Contains(description, "the assistant's own words") {
				t.Fatalf("the assistant-content mode offers a contract that does not mention the assistant's own words: %q", description)
			}
			if !strings.Contains(description, "cannot prove what the learner said") {
				t.Fatalf("the assistant-content mode offers a contract that does not say what an assistant message proves: %q", description)
			}
			continue
		}
		if strings.Contains(description, "the assistant's own words") {
			t.Fatalf("the mode that records nothing but the learner's statements offers the assistant's own words: %q", description)
		}
		if !strings.Contains(description, "the learner's own words") {
			t.Fatalf("the base contract does not say whose words it reads: %q", description)
		}
	}
}

// descriptionOfTheReadBackTool builds the tool table the way the wiring does
// and reads the description of the read-back tool out of it.
//
// The wiring is not copied here: the registry is the product's own, and the
// mode is set on it the way the wiring sets it.
func descriptionOfTheReadBackTool(t *testing.T, assistantContent bool) string {
	t.Helper()
	registry, err := agentTool.NewHistoryMessageRegistry(stubHistoryReader{})
	if err != nil {
		t.Fatal(err)
	}
	registry.UseAssistantContent(assistantContent)
	run := serviceagent.RunContext{
		UserID:    7,
		SessionID: "synthetic-evaluation",
		HistoryGrants: []serviceagent.HistoryMessageGrant{{
			MessageID:        1,
			SourceKind:       serviceagent.HistorySourceSummary,
			SourceSessionID:  "synthetic-evaluation",
			SummaryVersion:   1,
			SummaryWatermark: 10,
		}},
	}
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		info, err := tool.Info(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if info.Name == agentTool.ReadHistoryMessageName {
			return info.Desc
		}
	}
	t.Fatal("the read-back tool is not in the table")
	return ""
}

// stubHistoryReader reads nothing.
//
// The test reads the tool table, not what the tool would return, so the
// reader has nothing to answer.
type stubHistoryReader struct{}

func (stubHistoryReader) ReadOwnedMessages(context.Context, int64, []uint) ([]model.Message, error) {
	return nil, nil
}

func (stubHistoryReader) ReadOwnedMessagesBySession(context.Context, int64, map[string][]uint) ([]model.Message, error) {
	return nil, nil
}

// TestOfflineProductRunsInBothSummaryModes is the switch the deployment has
// to be able to measure in both positions.
//
// The summary that records what the assistant produced answers under its own
// prompts, and the harness classifies a model call by its first system
// message: a prompt the table did not hold was refused as unclassified, the
// turn ended before the answer existed, and the run reported a failure where
// the product had done nothing wrong.
//
// The test runs the offline product once in each mode and says that every call
// was classified and that the answer came back.
func TestOfflineProductRunsInBothSummaryModes(t *testing.T) {
	if os.Getenv("GL_SANDBOX_TEST") != "1" {
		t.Skip("requires local Docker MySQL image")
	}
	for _, mode := range []string{"off", "on"} {
		t.Run(mode, func(t *testing.T) {
			previous, had := os.LookupEnv(summaryAssistantContentEnv)
			if err := os.Setenv(summaryAssistantContentEnv, mode); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if had {
					os.Setenv(summaryAssistantContentEnv, previous)
					return
				}
				os.Unsetenv(summaryAssistantContentEnv)
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
			defer cancel()
			repository := filepath.Join("..", "..", "..")
			turns := make([]Turn, 42)
			for i := range turns {
				turns[i] = testTurn(i)
			}
			warmup, err := PrepareOfflineProductReport(ctx, repository, turns, AttemptInput{})
			if err != nil {
				t.Fatalf("warmup: %v", err)
			}
			for _, call := range warmup.Calls {
				if call.Role == "unknown" {
					t.Fatalf("a model call was not classified: %+v", call)
				}
			}
			result, err := RunOfflineProduct(ctx, repository, warmup.Snapshot, AttemptInput{
				Questions: []string{"请回顾当前会话里的练习安排。"},
			})
			if err != nil {
				t.Fatalf("run: %v; response=%s", err, result.Response)
			}
			if result.Answer != "synthetic offline answer" {
				t.Fatalf("the run produced no answer: %+v", result)
			}
			for _, call := range result.Calls {
				if call.Role == "unknown" {
					t.Fatalf("a model call was not classified: %+v", call)
				}
			}
			t.Logf("mode=%s warmup_calls=%d answer_calls=%d", mode, len(warmup.Calls), len(result.Calls))
		})
	}
}
