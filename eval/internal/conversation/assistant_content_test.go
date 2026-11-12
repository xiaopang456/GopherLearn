package conversation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"GopherAI/common/rag"
	"GopherAI/model"
	serviceagent "GopherAI/service/agent"
	agentTool "GopherAI/service/agent/tool"
	"github.com/cloudwego/eino/schema"
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
				Questions: []Question{{Text: "请回顾当前会话里的练习安排。"}},
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

// TestTheSummarySegmentReachesTheWindowInBothModes is the measurement the
// switch has to survive: the segment is written, the window carries it, and
// the model is asked about it.
//
// The segment is looked for in the window the answer was built from and not
// in the database. A segment that was written and never carried into the
// window is a segment nobody will ever be asked about, and a run that only
// checked that the segment was written would call that a success.
//
// The conversation is long enough that the raw turns no longer fit the
// window, so the product compresses them before the answer goes out. That is
// the path a long session takes, and it is the only one on which a segment is
// guaranteed to exist before the window is built.
//
// The two arms differ in one thing: the mode that records what the assistant
// produced carries a product of the assistant's, and the window says whose
// words it holds. The mode that does not record them carries the learner's
// points only.
func TestTheSummarySegmentReachesTheWindowInBothModes(t *testing.T) {
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
			settings, err := offlineSettings(repository, t.TempDir())
			if err != nil {
				t.Fatalf("settings: %v", err)
			}
			turns := longSegmentTurns(settings.Config.AgentConfig.SummarySegmentFallbackTokens)
			warmup, err := PrepareOfflineProductReport(ctx, repository, turns, AttemptInput{})
			if err != nil {
				t.Fatalf("warmup: %v", err)
			}
			result, err := RunOfflineProduct(ctx, repository, warmup.Snapshot, AttemptInput{
				Questions:             []Question{{Text: "请回顾当前会话里的练习安排。"}},
				WaitForSummarySegment: true,
			})
			if err != nil {
				t.Fatalf("run: %v; response=%s", err, result.Response)
			}
			// The driver refuses a call it cannot classify, so a run whose
			// calls are all classified is a run the driver accepted.
			for _, call := range result.Calls {
				if call.Role == "unknown" {
					t.Fatalf("a model call was not classified: %+v", call)
				}
			}
			window := contextText(result.ContextMessages)
			if !strings.Contains(window, syntheticSegmentTopic) {
				t.Fatalf("the segment did not reach the window: %s", window)
			}
			labelled := strings.Contains(window, "（助手提议，用户未表示同意）")
			if mode == "on" && !labelled {
				t.Fatalf("the window does not say whose words the product holds: %s", window)
			}
			if mode == "off" && labelled {
				t.Fatalf("the mode that records nothing but the learner's statements carries a proposal: %s", window)
			}
			t.Logf("mode=%s window=%d chars", mode, len(window))
		})
	}
}

// longSegmentTurns is a conversation whose raw turns are worth more than the
// product's own limit for sending them raw.
//
// The limit is the product's, read from its configuration: a test that
// hard-coded the number would go on measuring a conversation that fits after
// the product's limit was changed.
//
// The assistant's answers are lists, because that is what the summary writer
// keeps: a long answer of plain prose is compressed into nothing by the
// writer even though the window still counted it.
func longSegmentTurns(limit int) []Turn {
	turns := make([]Turn, 0, 32)
	tokens := 0
	for index := 0; tokens <= limit; index++ {
		at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(index) * time.Minute)
		turn := Turn{
			User: fmt.Sprintf("第 %d 轮：我这周主要在练导数的应用，前面学的极限老是忘，想按每组 30 分钟、一天 3 组来安排，"+
				"另外想把上周的错题再过一遍，每天大概能拿出一个半小时，麻烦给个能坚持下来的计划。", index),
			Assistant: fmt.Sprintf("第 %d 轮安排：\n1. 每天 3 组导数应用题，每组 20 分钟，做完写下用了哪一步\n"+
				"2. 每 3 天复习一次极限的定义，只看例题不看解答\n3. 周末做一套综合卷，限时 60 分钟\n"+
				"4. 把错题抄进错题本，每题写一句自己错在哪\n5. 睡前用 5 分钟回顾今天做对的题", index),
			UserAt:      at,
			AssistantAt: at.Add(time.Second),
		}
		tokens += rag.EstimateTokens(turn.User) + rag.EstimateTokens(turn.Assistant)
		turns = append(turns, turn)
	}
	return turns
}

// contextText is everything the model was shown, as one string.
func contextText(messages []*schema.Message) string {
	var out strings.Builder
	for _, message := range messages {
		if message == nil {
			continue
		}
		out.WriteString(message.Content)
		out.WriteString("\n")
	}
	return out.String()
}
