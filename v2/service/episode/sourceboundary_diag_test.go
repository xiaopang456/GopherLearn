package episode

import (
	"GopherAI/common/aihelper"
	"GopherAI/common/mysql"
	"GopherAI/config"
	"GopherAI/model"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// TestDiagnoseSourceBoundary reproduces one projection and reports which citation
// the source-boundary check rejects.
//
// Four of the five projection jobs for the real learner fail with
// `source_boundary`, and the job row keeps only the error code: the document
// that failed is discarded, so nothing on disk says whether the model invented a
// message id, cited an assistant turn where only user turns are allowed, or
// emitted an unlisted source handle. The three have different fixes, so the
// answer has to come from a live reproduction.
//
// It lives in the package because it must use the shipped prompt and tool
// schema. Copying either into a standalone command would let the diagnostic
// drift away from the thing it is diagnosing, and a diagnostic that measures a
// copy is worthless.
//
// Read-only: it loads evidence and calls the provider, and writes nothing.
func TestDiagnoseSourceBoundary(t *testing.T) {
	sessionID := strings.TrimSpace(os.Getenv("DIAG_SESSION"))
	if sessionID == "" {
		t.Skip("set DIAG_SESSION, DIAG_USER and DIAG_TARGET_MESSAGE to run")
	}
	userID, err := strconv.ParseInt(os.Getenv("DIAG_USER"), 10, 64)
	if err != nil {
		t.Fatalf("DIAG_USER: %v", err)
	}
	target, err := strconv.ParseUint(os.Getenv("DIAG_TARGET_MESSAGE"), 10, 64)
	if err != nil {
		t.Fatalf("DIAG_TARGET_MESSAGE: %v", err)
	}

	loaded, err := config.Load(os.Getenv("DIAG_CONFIG"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	// The configuration file names the container-internal MySQL port, which is
	// not reachable from a test running on the host.
	if port, err := strconv.Atoi(os.Getenv("DIAG_MYSQL_PORT")); err == nil && port > 0 {
		loaded.MysqlConfig.MysqlHost, loaded.MysqlConfig.MysqlPort = "127.0.0.1", port
	}
	if err := config.Use(loaded); err != nil {
		t.Fatalf("install config: %v", err)
	}
	if err := mysql.InitMysql(); err != nil {
		t.Fatalf("connect mysql: %v", err)
	}

	ctx := context.Background()
	loader, err := NewDatabaseEvidenceLoader(mysql.DB, loaded.AgentConfig.EpisodeMaxMessages)
	if err != nil {
		t.Fatalf("evidence loader: %v", err)
	}
	evidence, err := loader.Load(ctx, userID, sessionID, uint(target))
	if err != nil {
		t.Fatalf("load evidence: %v", err)
	}

	userMessages := map[uint]bool{}
	t.Logf("session %s: %d messages, summary=%v", sessionID, len(evidence.Messages), evidence.Summary != nil)
	for _, message := range evidence.Messages {
		role := "assistant"
		if message.IsUser {
			role = "USER"
			userMessages[message.ID] = true
		}
		t.Logf("  id=%-5d %-9s %s", message.ID, role, snippet(message.Content, 40))
	}

	chatModel, err := aihelper.NewToolCallingChatModel(ctx)
	if err != nil {
		t.Fatalf("chat model: %v", err)
	}
	bound, err := chatModel.WithTools([]*schema.ToolInfo{episodeProjectionTool()})
	if err != nil {
		t.Fatalf("bind tool: %v", err)
	}

	// Mirror Generate's batching so the reproduction sees the same input the
	// worker did. A single batch is the common case; a split would change which
	// ids are in scope, which is itself a candidate explanation.
	batchSize := largestCompleteBatch(evidence, nil, evidence.Messages, loaded.AgentConfig.EpisodeMaxInputTokens)
	t.Logf("batch: %d of %d messages fit the input budget", batchSize, len(evidence.Messages))
	payload := buildProjectionPayload(evidence, nil, evidence.Messages[:batchSize])
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}

	response, err := bound.Generate(ctx, []*schema.Message{
		schema.SystemMessage(episodeSystemPrompt), schema.UserMessage(string(encoded)),
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	document, err := decodeEpisodeToolResponse(response)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	normalizeEpisodeTopics(&document)

	allowed := map[string]struct{}{}
	if evidence.Summary != nil {
		allowed[fmt.Sprintf("summary:%s:v%d", evidence.Session.ID, evidence.Summary.Version)] = struct{}{}
	}
	known := map[uint]bool{}
	for _, message := range evidence.Messages[:batchSize] {
		known[message.ID] = true
		for _, handle := range sourceHandlesForMessage(message) {
			allowed[handle] = struct{}{}
		}
	}

	t.Log("citations the model produced:")
	for _, group := range []struct {
		name    string
		entries []model.EpisodeEvidence
	}{
		{"decisions", document.Decisions},
		{"open_loops", document.OpenLoops},
		{"user_corrections", document.UserCorrections},
	} {
		for _, entry := range group.entries {
			for _, id := range entry.EvidenceMessageIDs {
				verdict := "ok"
				switch {
				case !known[id]:
					verdict = "REJECTED: no such message in this batch"
				case !userMessages[id]:
					verdict = "REJECTED: assistant message cited where only USER is allowed"
				}
				t.Logf("  %-16s id=%-5d %-42s %s", group.name, id, snippet(entry.Text, 40), verdict)
			}
			if len(entry.EvidenceMessageIDs) == 0 {
				t.Logf("  %-16s (no citation) %s", group.name, snippet(entry.Text, 40))
			}
		}
	}
	for _, handle := range document.SourceHandles {
		verdict := "ok"
		if _, exists := allowed[strings.TrimSpace(handle)]; !exists {
			verdict = "REJECTED: handle not supplied in the input"
		}
		t.Logf("  source_handle    %-48s %s", handle, verdict)
	}

	t.Logf("validator verdict: %v", validateGeneratedDocument(&document, evidence, evidence.Messages[:batchSize]))
}

func snippet(value string, max int) string {
	value = strings.ReplaceAll(strings.TrimSpace(value), "\n", " ")
	if runes := []rune(value); len(runes) > max {
		return string(runes[:max]) + "…"
	}
	return value
}
