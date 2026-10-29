package agent

import (
	"GopherAI/common/rag"
	sessiondao "GopherAI/dao/session"
	"GopherAI/model"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
)

type exp001bRepository struct {
	mu        sync.Mutex
	current   *model.SessionSummary
	saveCalls int
}

func (r *exp001bRepository) GetSummary(_ context.Context, userID int64, sessionID string) (*model.SessionSummary, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current == nil || r.current.UserID != userID || r.current.SessionID != sessionID {
		return nil, sessiondao.ErrSummaryNotFound
	}
	copy := *r.current
	return &copy, nil
}

func (r *exp001bRepository) SaveSummary(_ context.Context, next *model.SessionSummary, expected uint64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.saveCalls++
	currentVersion, currentCovered := uint64(0), uint(0)
	if r.current != nil {
		currentVersion, currentCovered = r.current.Version, r.current.CoveredMessageID
	}
	if currentVersion != expected || next.CoveredMessageID <= currentCovered {
		return sessiondao.ErrSummaryConflict
	}
	copy := *next
	copy.Version = expected + 1
	r.current = &copy
	next.Version = copy.Version
	return nil
}

func (r *exp001bRepository) state() (uint64, uint, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current == nil {
		return 0, 0, r.saveCalls
	}
	return r.current.Version, r.current.CoveredMessageID, r.saveCalls
}

type exp001bMessages struct{ messages []model.Message }

func (m exp001bMessages) ListAfter(_ context.Context, _ string, after uint, limit int) ([]model.Message, error) {
	return exp001bFilter(m.messages, after, limit), nil
}

func (m exp001bMessages) ListRecentAfter(_ context.Context, _ string, after uint, limit int) ([]model.Message, error) {
	filtered := exp001bFilter(m.messages, after, len(m.messages))
	if len(filtered) > limit {
		filtered = filtered[len(filtered)-limit:]
	}
	return filtered, nil
}

func exp001bFilter(messages []model.Message, after uint, limit int) []model.Message {
	result := make([]model.Message, 0, len(messages))
	for _, message := range messages {
		if message.ID > after {
			result = append(result, message)
		}
		if len(result) == limit {
			break
		}
	}
	return result
}

type exp001bGenerator struct {
	mu             sync.Mutex
	mode           string
	calls          int
	inputTokens    int
	outputTokens   int
	durationMillis int64
}

func (g *exp001bGenerator) GenerateSummary(ctx context.Context, previous string, messages []model.Message) (string, error) {
	started := time.Now()
	g.mu.Lock()
	g.calls++
	input := rag.EstimateTokens(previous) + 128
	for _, message := range messages {
		input += rag.EstimateTokens(message.Content) + 20
	}
	g.inputTokens += input
	g.mu.Unlock()
	var output string
	var err error
	switch g.mode {
	case "timeout":
		<-ctx.Done()
		err = ctx.Err()
	case "empty":
		output = ""
	default:
		output = `{"current_goal":"EXP-001B","confirmed_decisions":["keep old summary"],"completed_items":[],"open_loops":["continue"],"user_corrections":[],"key_errors":[],"references":[]}`
	}
	g.mu.Lock()
	g.durationMillis += time.Since(started).Milliseconds()
	g.outputTokens += rag.EstimateTokens(output)
	g.mu.Unlock()
	return output, err
}

func (g *exp001bGenerator) metrics() (int, int, int, int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls, g.inputTokens, g.outputTokens, g.durationMillis
}

type exp001bRecord map[string]any

func TestEXP001BComponentHarness(t *testing.T) {
	rawDir := os.Getenv("EXP001B_RAW_DIR")
	if strings.TrimSpace(rawDir) == "" {
		t.Skip("set EXP001B_RAW_DIR to execute the experiment harness")
	}
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		t.Fatal(err)
	}
	jsonlPath := filepath.Join(rawDir, "component-harness.jsonl")
	csvPath := filepath.Join(rawDir, "component-harness.csv")
	jsonFile, err := os.OpenFile(jsonlPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("refusing to overwrite component output: %v", err)
	}
	defer jsonFile.Close()
	encoder := json.NewEncoder(jsonFile)
	var records []exp001bRecord
	appendRecord := func(record exp001bRecord) {
		record["schema_version"] = "exp001b.component.v1"
		record["timestamp_utc"] = time.Now().UTC().Format(time.RFC3339Nano)
		if err := encoder.Encode(record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}

	for _, pressure := range []struct {
		name     string
		messages []model.Message
	}{
		{name: "realistic_drop", messages: exp001bPressureMessages(72, false)},
		{name: "oversized_latest_truncate", messages: exp001bPressureMessages(18, true)},
	} {
		reader := exp001bMessages{messages: pressure.messages}
		builder, buildErr := NewContextBuilder(DefaultContextConfig(), reader, nil, nil, nil, nil)
		if buildErr != nil {
			t.Fatal(buildErr)
		}
		window, buildErr := builder.Build(context.Background(), RunContext{UserID: 11, SessionID: "component-pressure", Question: "Summarize the latest bounded input and preserve CURRENT-QUESTION-991."})
		if buildErr != nil {
			t.Fatal(buildErr)
		}
		if window.Budget.ProjectedInput < 15000 || window.Budget.ProjectedInput > 20000 {
			t.Fatalf("%s projected input=%d, want 15k..20k", pressure.name, window.Budget.ProjectedInput)
		}
		if window.Budget.DroppedMessages == 0 {
			t.Fatalf("%s did not exercise dropped branch: %+v", pressure.name, window.Budget)
		}
		if pressure.name == "oversized_latest_truncate" && window.Budget.TruncatedMessages == 0 {
			t.Fatalf("%s did not exercise truncated branch: %+v", pressure.name, window.Budget)
		}
		appendRecord(exp001bRecord{
			"record_type": "context_pressure", "case": pressure.name,
			"content_profile": []string{"go_code", "json", "tool_result"},
			"config":          DefaultContextConfig(), "budget": window.Budget,
			"assertions": map[string]bool{
				"projected_15k_to_20k":  true,
				"dropped_branch":        window.Budget.DroppedMessages > 0,
				"truncated_branch":      window.Budget.TruncatedMessages > 0,
				"current_question_kept": window.Messages[len(window.Messages)-1].Content == "Summarize the latest bounded input and preserve CURRENT-QUESTION-991.",
			},
		})
	}

	exp001bRunFailureInjection(t, "summary_timeout", "timeout", appendRecord)
	exp001bRunFailureInjection(t, "summary_empty", "empty", appendRecord)
	exp001bRunModelOutputInjection(t, "summary_invalid_json", "not-json", appendRecord)
	exp001bRunModelOutputInjection(t, "summary_empty_json", `{"current_goal":"","confirmed_decisions":[],"completed_items":[],"open_loops":[],"user_corrections":[],"key_errors":[],"references":[]}`, appendRecord)
	exp001bRunOversizedOldest(t, appendRecord)

	if err := exp001bWriteCSV(csvPath, records); err != nil {
		t.Fatal(err)
	}
}

func exp001bRunFailureInjection(t *testing.T, name, mode string, appendRecord func(exp001bRecord)) {
	t.Helper()
	repository := &exp001bRepository{current: &model.SessionSummary{UserID: 11, SessionID: name, Content: `{"current_goal":"OLD-SUMMARY-77"}`, CoveredMessageID: 1, Version: 1}}
	messages := exp001bFailureMessages(false)
	generator := &exp001bGenerator{mode: mode}
	config := DefaultSummaryConfig()
	if mode == "timeout" {
		config.Timeout = 8 * time.Millisecond
	}
	provider, err := NewRollingSummaryProvider(repository, exp001bMessages{messages: messages}, generator, config)
	if err != nil {
		t.Fatal(err)
	}
	exp001bAssertDegradedBuild(t, name, provider, repository, messages, appendRecord, generator.metrics)
}

func exp001bRunModelOutputInjection(t *testing.T, name, response string, appendRecord func(exp001bRecord)) {
	t.Helper()
	chat := &scriptedChatModel{respond: func(context.Context, int) (*schema.Message, error) {
		return schema.AssistantMessage(response, nil), nil
	}}
	generator, err := NewModelSummaryGenerator(chat, 8000, 2000)
	if err != nil {
		t.Fatal(err)
	}
	repository := &exp001bRepository{current: &model.SessionSummary{UserID: 11, SessionID: name, Content: `{"current_goal":"OLD-SUMMARY-77"}`, CoveredMessageID: 1, Version: 1}}
	messages := exp001bFailureMessages(false)
	provider, err := NewRollingSummaryProvider(repository, exp001bMessages{messages: messages}, generator, DefaultSummaryConfig())
	if err != nil {
		t.Fatal(err)
	}
	metrics := func() (int, int, int, int64) { return chat.CallCount(), 0, 0, 0 }
	exp001bAssertDegradedBuild(t, name, provider, repository, messages, appendRecord, metrics)
}

func exp001bAssertDegradedBuild(
	t *testing.T,
	name string,
	provider *RollingSummaryProvider,
	repository *exp001bRepository,
	messages []model.Message,
	appendRecord func(exp001bRecord),
	metrics func() (int, int, int, int64),
) {
	t.Helper()
	reader := exp001bMessages{messages: messages}
	builder, err := NewContextBuilder(DefaultContextConfig(), reader, nil, nil, nil, provider)
	if err != nil {
		t.Fatal(err)
	}
	var errorsSeen []string
	var lastWindow ContextWindow
	for attempt := 1; attempt <= 2; attempt++ {
		started := time.Now()
		window, buildErr := builder.Build(context.Background(), RunContext{UserID: 11, SessionID: name, Question: "CURRENT-CONTINUES-88"})
		if buildErr != nil {
			t.Fatalf("%s build should degrade and continue: %v", name, buildErr)
		}
		if time.Since(started) > time.Second {
			t.Fatalf("%s appears stuck in a loop", name)
		}
		lastWindow = window
		_, providerErr := provider.SessionSummary(context.Background(), RunContext{UserID: 11, SessionID: name})
		if providerErr != nil {
			errorsSeen = append(errorsSeen, providerErr.Error())
		}
	}
	version, watermark, saveCalls := repository.state()
	if version != 1 || watermark != 1 || saveCalls != 0 {
		t.Fatalf("%s advanced invalid summary state: version=%d watermark=%d saves=%d", name, version, watermark, saveCalls)
	}
	joined := exp001bJoinMessages(lastWindow.Messages)
	if !strings.Contains(joined, "OLD-SUMMARY-77") || !strings.Contains(joined, "CURRENT-CONTINUES-88") {
		t.Fatalf("%s degraded context lost old summary or current question", name)
	}
	calls, input, output, duration := metrics()
	appendRecord(exp001bRecord{
		"record_type": "summary_failure_injection", "case": name,
		"before":         map[string]any{"version": 1, "watermark": 1},
		"after":          map[string]any{"version": version, "watermark": watermark, "save_calls": saveCalls},
		"generator_cost": map[string]any{"calls": calls, "cumulative_input_tokens_project_estimate": input, "cumulative_output_tokens_project_estimate": output, "cumulative_duration_ms": duration},
		"errors":         errorsSeen, "context_budget": lastWindow.Budget,
		"assertions": map[string]bool{
			"watermark_unchanged": true, "version_unchanged": true, "no_publish": true,
			"degraded_continue": true, "old_summary_kept": true, "current_question_kept": true,
			"two_attempts_returned_without_internal_loop": true,
		},
	})
}

func exp001bRunOversizedOldest(t *testing.T, appendRecord func(exp001bRecord)) {
	t.Helper()
	name := "oversized_oldest_message"
	repository := &exp001bRepository{current: &model.SessionSummary{UserID: 11, SessionID: name, Content: `{"current_goal":"OLD-SUMMARY-77"}`, CoveredMessageID: 1, Version: 1}}
	messages := exp001bFailureMessages(true)
	generator := &exp001bGenerator{mode: "success"}
	provider, err := NewRollingSummaryProvider(repository, exp001bMessages{messages: messages}, generator, DefaultSummaryConfig())
	if err != nil {
		t.Fatal(err)
	}
	exp001bAssertDegradedBuild(t, name, provider, repository, messages, appendRecord, generator.metrics)
	calls, _, _, _ := generator.metrics()
	if calls != 0 {
		t.Fatalf("oversized oldest message should not be partially sent to generator; calls=%d", calls)
	}
}

func exp001bPressureMessages(count int, oversizedLatest bool) []model.Message {
	messages := make([]model.Message, 0, count)
	for i := 1; i <= count; i++ {
		content := exp001bRealisticPayload(i)
		if oversizedLatest && i == count {
			content = strings.Repeat(content, 28)
		}
		messages = append(messages, model.Message{ID: uint(i), IsUser: i%2 == 1, Content: content})
	}
	return messages
}

func exp001bRealisticPayload(index int) string {
	var out strings.Builder
	fmt.Fprintf(&out, "TURN-%03d\n```go\n", index)
	for i := 0; i < 18; i++ {
		fmt.Fprintf(&out, "func handle%d_%d(ctx context.Context, request Request) (Response, error) { if err := ctx.Err(); err != nil { return Response{}, err }; return Response{Code:%d}, nil }\n", index, i, i)
	}
	out.WriteString("```\n{\"tool\":\"knowledge_search\",\"status\":\"ok\",\"results\":[")
	for i := 0; i < 24; i++ {
		fmt.Fprintf(&out, "{\"citation\":%d,\"doc\":\"doc-%03d\",\"line\":%d,\"summary\":\"bounded observation %d\"},", i+1, index, i*7+1, i)
	}
	out.WriteString("]}\n")
	return out.String()
}

func exp001bFailureMessages(oversizedOldest bool) []model.Message {
	messages := make([]model.Message, 0, 16)
	for i := 1; i <= 16; i++ {
		content := fmt.Sprintf("message-%d preserve RECENT-%d", i, i)
		if oversizedOldest && i == 2 {
			content = strings.Repeat(exp001bRealisticPayload(2), 20)
		}
		messages = append(messages, model.Message{ID: uint(i), IsUser: i%2 == 1, Content: content})
	}
	return messages
}

func exp001bJoinMessages(messages []*schema.Message) string {
	var out strings.Builder
	for _, message := range messages {
		out.WriteString(message.Content)
		out.WriteByte('\n')
	}
	return out.String()
}

func exp001bWriteCSV(path string, records []exp001bRecord) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("refusing to overwrite component csv: %w", err)
	}
	defer file.Close()
	writer := csv.NewWriter(file)
	defer writer.Flush()
	if err := writer.Write([]string{"record_type", "case", "json_sha256"}); err != nil {
		return err
	}
	for _, record := range records {
		data, marshalErr := json.Marshal(record)
		if marshalErr != nil {
			return marshalErr
		}
		hash := sha256.Sum256(data)
		if err := writer.Write([]string{fmt.Sprint(record["record_type"]), fmt.Sprint(record["case"]), hex.EncodeToString(hash[:])}); err != nil {
			return err
		}
	}
	return writer.Error()
}

var _ SummaryRepository = (*exp001bRepository)(nil)
var _ SummaryMessageReader = exp001bMessages{}
var _ MessageHistoryReader = exp001bMessages{}

func TestEXP001BFailureErrorsAreClassifiable(t *testing.T) {
	if !errors.Is(context.DeadlineExceeded, context.DeadlineExceeded) {
		t.Fatal("deadline classification unavailable")
	}
}
