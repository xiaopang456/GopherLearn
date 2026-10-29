package hybrid

import (
	"GopherAI/common/code"
	"GopherAI/common/rag"
	knowledgeDAO "GopherAI/dao/knowledge"
	learningDAO "GopherAI/dao/learning"
	"GopherAI/entry"
	"GopherAI/model"
	"GopherAI/utils"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	modelcomponent "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/xiaopang456/GopherLearn/conversation"
	"gorm.io/gorm"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

const learningPath = "/api/v1/AI/agent/learning/"

type learningModelState struct {
	mu                                     sync.Mutex
	generationCalls, gradingCalls          int
	generateErr, gradeErr, waitForDeadline bool
	requests                               []string
}

func TestLearningFrontendAPIThroughLocalSocket(t *testing.T) {
	app, db, agent, _, _ := learningFixture(t, true)
	server := httptest.NewServer(app.Handler)
	defer server.Close()
	command := exec.Command("node", "../web/test/helpers/hybridLearningClient.mjs")
	command.Env = append(os.Environ(), "HYBRID_TEST_URL="+server.URL)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("frontend native learning API: %v\n%s", err, output)
	}
	var tasks, attempts int64
	db.Model(&model.LearningSession{}).Count(&tasks)
	db.Model(&model.LearningAttempt{}).Count(&attempts)
	if tasks != 1 || attempts != 1 {
		t.Fatalf("socket did not persist original records: %d %d", tasks, attempts)
	}
	agent.state.mu.Lock()
	defer agent.state.mu.Unlock()
	if agent.state.generationCalls != 1 || agent.state.gradingCalls != 1 {
		t.Fatalf("socket/replay model calls: %d %d", agent.state.generationCalls, agent.state.gradingCalls)
	}
}

func TestLearningNativeHTTPGradingFailureRecoveryAndSkip(t *testing.T) {
	app, db, agent, token, _ := learningFixture(t, true)
	sid := createAgentHTTP(t, app, token, "learning-recovery-create", "Agent")
	view := startLearningHTTP(t, app, token, sid, "Redis")
	body := answerLearningBody(view, "learning-recovery-answer")
	agent.state.mu.Lock()
	agent.state.gradeErr = true
	agent.state.mu.Unlock()
	_, failure, _ := requestHTTP(t, app.Handler, "POST", learningPath+"answer", learningJSON(body), token)
	if failure["status_code"] != float64(code.LearningGradingFail) {
		t.Fatal(failure)
	}
	var attempt model.LearningAttempt
	if err := db.Where("submission_id = ?", body["submissionId"]).First(&attempt).Error; err != nil || attempt.Status != model.LearningAttemptFailed {
		t.Fatalf("failed original attempt %+v %v", attempt, err)
	}
	_, recovered, _ := requestHTTP(t, app.Handler, "GET", learningPath+"current?sessionId="+sid, "", token)
	if recovered["learning"].(map[string]any)["status"] != "waiting_answer" {
		t.Fatal(recovered)
	}
	agent.state.mu.Lock()
	agent.state.gradeErr = false
	agent.state.mu.Unlock()
	result := learningRequest(t, app.Handler, "answer", learningJSON(body), token)["result"].(map[string]any)
	current := result["learning"].(map[string]any)
	skip := answerLearningBody(current, "learning-skip-answer")
	delete(skip, "answer")
	skip["action"] = "skip"
	skipped := learningRequest(t, app.Handler, "action", learningJSON(skip), token)["result"].(map[string]any)
	if skipped["attempt"].(map[string]any)["status"] != "skipped" || skipped["learning"].(map[string]any)["currentQuestion"] != float64(3) {
		t.Fatal(skipped)
	}
	replay := learningRequest(t, app.Handler, "action", learningJSON(skip), token)["result"].(map[string]any)
	if replay["attempt"].(map[string]any)["id"] != skipped["attempt"].(map[string]any)["id"] {
		t.Fatal("skip replay changed attempt")
	}
}

func TestLearningNativeHTTPGenerationFailureAndDeadline(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint("deadline=", deadline), func(t *testing.T) {
			app, db, agent, token, _ := learningFixture(t, true)
			sid := createAgentHTTP(t, app, token, "learning-failure-create", "Agent")
			agent.state.mu.Lock()
			agent.state.generateErr = !deadline
			agent.state.waitForDeadline = deadline
			agent.state.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			request := httptest.NewRequest("POST", learningPath+"start", strings.NewReader(learningJSON(map[string]string{"sessionId": sid, "topic": "Redis"}))).WithContext(ctx)
			request.Header.Set("Authorization", "Bearer "+token)
			request.Header.Set("Content-Type", "application/json")
			writer := httptest.NewRecorder()
			app.Handler.ServeHTTP(writer, request)
			var result map[string]any
			if err := json.Unmarshal(writer.Body.Bytes(), &result); err != nil || result["status_code"] != float64(code.LearningGenerationFail) {
				t.Fatalf("failure response %s %v", writer.Body.String(), err)
			}
			var task model.LearningSession
			if err := db.First(&task).Error; err != nil || task.Status != model.LearningSessionCancelled {
				t.Fatalf("failed generation did not cancel original task: %+v %v", task, err)
			}
			var questions int64
			db.Model(&model.LearningQuestion{}).Count(&questions)
			if questions != 0 {
				t.Fatal("failed generation persisted questions")
			}
		})
	}
}

func TestLearningNativeHTTPRejectsAuthNormalModeAndMissingEvidence(t *testing.T) {
	app, _, agent, token, _ := learningFixture(t, false)
	sid := createAgentHTTP(t, app, token, "learning-no-evidence", "Agent")
	body := learningJSON(map[string]string{"sessionId": sid, "topic": "Redis"})
	_, evidence, _ := requestHTTP(t, app.Handler, "POST", learningPath+"start", body, token)
	if evidence["status_code"] != float64(code.LearningNoEvidence) {
		t.Fatal(evidence)
	}
	for _, auth := range []string{"", "invalid-token"} {
		_, rejected, _ := requestHTTP(t, app.Handler, "POST", learningPath+"start", body, auth)
		if rejected["status_code"] == float64(1000) {
			t.Fatal("invalid auth succeeded")
		}
	}
	normal := createHTTP(t, app.Handler, token, "normal")
	_, rejected, _ := requestHTTP(t, app.Handler, "POST", learningPath+"start", learningJSON(map[string]string{"sessionId": normal, "topic": "Redis"}), token)
	if rejected["status_code"] == float64(1000) {
		t.Fatal("normal session accepted")
	}
	agent.state.mu.Lock()
	defer agent.state.mu.Unlock()
	if agent.state.generationCalls != 0 || agent.state.gradingCalls != 0 {
		t.Fatal("rejected requests called model")
	}
}

type learningHTTPModel struct {
	*scriptedChatModel
	state *learningModelState
}

func (m *learningHTTPModel) WithTools(infos []*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	if _, err := m.scriptedChatModel.WithTools(infos); err != nil {
		return nil, err
	}
	return m, nil
}
func (m *learningHTTPModel) Generate(ctx context.Context, messages []*schema.Message, _ ...modelcomponent.Option) (*schema.Message, error) {
	var rendered strings.Builder
	for _, msg := range messages {
		rendered.WriteString(msg.Content)
		rendered.WriteByte('\n')
	}
	content := rendered.String()
	m.state.mu.Lock()
	m.state.requests = append(m.state.requests, content)
	genErr, gradeErr, wait := m.state.generateErr, m.state.gradeErr, m.state.waitForDeadline
	generation := strings.Contains(content, "You generate assessment questions")
	grading := strings.Contains(content, "You grade one answer")
	if generation {
		m.state.generationCalls++
	}
	if grading {
		m.state.gradingCalls++
	}
	m.state.mu.Unlock()
	if wait && (generation || grading) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if generation {
		if genErr {
			return nil, errors.New("offline generation failure")
		}
		// This model substitute checks the real production request; the service
		// still validates evidence IDs, parses, persists and transitions itself.
		if !strings.Contains(content, "Generate exactly 3 questions") || !strings.Contains(content, "Evidence JSON:") || !strings.Contains(content, "惰性删除") {
			return nil, fmt.Errorf("missing original generation/evidence request")
		}
		return schema.AssistantMessage(`{"questions":[{"type":"short_answer","question":"Redis 有哪些过期删除策略？","assessment_goal":"识别删除策略","reference_answer":"惰性删除和定期删除","rubric":["说出惰性删除","说出定期删除"],"evidence_ids":[1]},{"type":"concept_comparison","question":"比较惰性删除与定期删除的触发时机。","assessment_goal":"比较触发时机","reference_answer":"访问时删除与周期扫描删除","rubric":["说明触发时机"],"evidence_ids":[1]},{"type":"short_answer","question":"为何需要结合两种策略平衡资源消耗？","assessment_goal":"解释资源权衡","reference_answer":"平衡内存和 CPU","rubric":["说明资源权衡"],"evidence_ids":[1]}]}`, nil), nil
	}
	if grading {
		if gradeErr {
			return nil, errors.New("offline grading failure")
		}
		if !strings.Contains(content, "student_answer") || !strings.Contains(content, "reference_answer") || !strings.Contains(content, "snapshot") {
			return nil, errors.New("missing immutable grading request")
		}
		return schema.AssistantMessage(`{"correctness":50,"completeness":20,"clarity":10,"feedback":"依据原证据覆盖两种删除策略。","missing_points":["补充资源权衡"],"confidence":0.9}`, nil), nil
	}
	return schema.AssistantMessage("离线 Agent 响应", nil), nil
}
func (m *learningHTTPModel) Stream(ctx context.Context, messages []*schema.Message, opts ...modelcomponent.Option) (*schema.StreamReader[*schema.Message], error) {
	response, err := m.Generate(ctx, messages, opts...)
	if err != nil {
		return nil, err
	}
	response.ResponseMeta = &schema.ResponseMeta{FinishReason: "stop"}
	return schema.StreamReaderFromArray([]*schema.Message{response}), nil
}

func learningFixture(t *testing.T, withEvidence bool) (*Application, *gorm.DB, *learningHTTPModel, string, string) {
	t.Helper()
	db := sqliteDatabase(t)
	settings := testSettings(t)
	settings.Config.KnowledgeConfig.ScanIntervalSeconds = 3600
	chunks := &testChunks{}
	script := &scriptedChatModel{respond: func(context.Context, int) (*schema.Message, error) {
		return schema.AssistantMessage("offline", nil), nil
	}}
	agent := &learningHTTPModel{scriptedChatModel: script, state: &learningModelState{}}
	migrations := 0
	deps := dependencies(t, db, script, chunks, &migrations)
	deps.ChatModel = func(context.Context, *entry.Settings) (modelcomponent.ToolCallingChatModel, error) { return agent, nil }
	app, err := Assemble(context.Background(), settings, deps, &ordinaryModel{}, conversation.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	hash, err := utils.HashPassword("synthetic-password")
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range []model.User{{ID: 1, Username: "learner", Name: "learner", Email: "learner@invalid.example", Password: hash}, {ID: 2, Username: "other", Name: "other", Email: "other@invalid.example", Password: hash}} {
		if err := db.Create(&user).Error; err != nil {
			t.Fatal(err)
		}
	}
	token := loginHTTP(t, app.Handler, "learner@invalid.example")
	other := loginHTTP(t, app.Handler, "other@invalid.example")
	if withEvidence {
		note := "Redis 使用惰性删除和定期删除；访问时惰性删除，周期扫描定期删除，结合两者平衡内存和 CPU。"
		status, uploaded := knowledgeRequest(t, app.Handler, "POST", "/api/v1/knowledge/documents", token, "redis.md", note)
		if status != 201 {
			t.Fatalf("original source upload %d %v", status, uploaded)
		}
		id := uploaded["document"].(map[string]any)["id"].(string)
		repository := knowledgeDAO.NewRepository(db)
		_, err := repository.ClaimForIndexing(context.Background(), id, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.CompleteIndexing(context.Background(), id, 1, 1, model.KnowledgeStatusReady, 1, ""); err != nil {
			t.Fatal(err)
		}
		chunks.rows = []rag.RedisChunkSearchResult{{DocID: id, DocVersion: 1, ChunkID: "offline-external-vector-chunk", SourceFile: "redis.md", RawContent: note, EmbeddingContent: note, ContentHash: model.MemoryContentHash(note), StartLine: 1, EndLine: 1, ChunkType: "prose", Distance: 0.01, LexicalScore: 1}}
	}
	return app, db, agent, token, other
}
func learningRequest(t *testing.T, h http.Handler, action, body, token string) map[string]any {
	t.Helper()
	status, result, _ := requestHTTP(t, h, "POST", learningPath+action, body, token)
	if status != 200 || result["status_code"] != float64(1000) {
		t.Fatalf("native learning %s: %d %v", action, status, result)
	}
	return result
}
func learningJSON(value any) string { body, _ := json.Marshal(value); return string(body) }
func startLearningHTTP(t *testing.T, app *Application, token, sid, topic string) map[string]any {
	return learningRequest(t, app.Handler, "start", learningJSON(map[string]string{"sessionId": sid, "topic": topic}), token)["learning"].(map[string]any)
}
func answerLearningBody(view map[string]any, submission string) map[string]string {
	return map[string]string{"sessionId": view["sessionId"].(string), "learningSessionId": view["id"].(string), "questionId": view["question"].(map[string]any)["id"].(string), "submissionId": submission, "answer": "惰性删除与定期删除结合平衡内存与 CPU。"}
}

func TestLearningNativeHTTPGenerationAnswerReplayCorrectionAndCompletion(t *testing.T) {
	app, db, agent, token, other := learningFixture(t, true)
	sid := createAgentHTTP(t, app, token, "create-learning-native-001", "Agent")
	view := startLearningHTTP(t, app, token, sid, "Redis")
	if view["status"] != "waiting_answer" || view["totalQuestions"] != float64(3) {
		t.Fatal(view)
	}
	question := view["question"].(map[string]any)
	if len(question["sources"].([]any)) != 1 {
		t.Fatal("original source view lost")
	}
	for _, private := range []string{"referenceAnswer", "rubric", "evidence", "assessmentGoal"} {
		if _, exists := question[private]; exists {
			t.Fatal("private question field exposed")
		}
	}
	status, current, _ := requestHTTP(t, app.Handler, "GET", learningPath+"current?sessionId="+sid, "", token)
	if status != 200 || current["learning"].(map[string]any)["id"] != view["id"] {
		t.Fatal(current)
	}
	dataHTTP(t, app.Handler, "POST", "/api/v1/sessions/"+sid+"/messages", agentBody("task-context-native-001", "当前学习任务是什么？"), token)
	agent.state.mu.Lock()
	captured := strings.Join(agent.state.requests, "\n")
	agent.state.mu.Unlock()
	if !strings.Contains(captured, "Active learning task (structured server state") || !strings.Contains(captured, question["question"].(string)) {
		t.Fatal("original active-task context wiring missing")
	}
	first := answerLearningBody(view, "submission-native-001")
	result := learningRequest(t, app.Handler, "answer", learningJSON(first), token)["result"].(map[string]any)
	attempt := result["attempt"].(map[string]any)
	if attempt["effectiveTotal"] != float64(80) || attempt["feedback"] == "" {
		t.Fatal(result)
	}
	repeated := learningRequest(t, app.Handler, "answer", learningJSON(first), token)["result"].(map[string]any)
	if repeated["attempt"].(map[string]any)["id"] != attempt["id"] {
		t.Fatal("original replay identity changed")
	}
	agent.state.mu.Lock()
	gradeCalls := agent.state.gradingCalls
	agent.state.mu.Unlock()
	if gradeCalls != 1 {
		t.Fatal("duplicate submission reran grader")
	}
	for _, bad := range []struct{ body, token string }{{learningJSON(first), other}, {learningJSON(map[string]string{"sessionId": sid, "learningSessionId": view["id"].(string), "questionId": question["id"].(string), "submissionId": "submission-native-001", "answer": "不同输入"}), token}} {
		_, failure, _ := requestHTTP(t, app.Handler, "POST", learningPath+"answer", bad.body, bad.token)
		if failure["status_code"] == float64(1000) {
			t.Fatal("identity/fingerprint mismatch succeeded")
		}
	}
	corrected := learningRequest(t, app.Handler, "correct", learningJSON(map[string]any{"attemptId": attempt["id"], "effectiveTotal": 25, "reason": "用户明确纠正"}), token)
	if corrected["attempt"].(map[string]any)["effectiveTotal"] != float64(25) {
		t.Fatal(corrected)
	}
	progress, err := learningDAO.NewRepository(db).GetTopicProgress(context.Background(), 1, "redis")
	if err != nil || progress.MasteryScore != 25 {
		t.Fatalf("original progress %v %v", progress, err)
	}
	currentView := result["learning"].(map[string]any)
	for index := 2; index <= 3; index++ {
		body := answerLearningBody(currentView, fmt.Sprintf("submission-native-%03d", index))
		result = learningRequest(t, app.Handler, "answer", learningJSON(body), token)["result"].(map[string]any)
		currentView = result["learning"].(map[string]any)
	}
	if currentView["status"] != "completed" {
		t.Fatal(currentView)
	}
	_, gone, _ := requestHTTP(t, app.Handler, "GET", learningPath+"current?sessionId="+sid, "", token)
	if gone["status_code"] != float64(code.LearningSessionNotFound) {
		t.Fatal("current must not pretend completed history", gone)
	}
	var count int64
	db.Model(&model.LearningAttempt{}).Count(&count)
	if count != 3 {
		t.Fatalf("actual attempts %d", count)
	}
}

func TestLearningNativeHTTPIsolationStateConflictAndExpiry(t *testing.T) {
	app, db, agent, token, other := learningFixture(t, true)
	sid := createAgentHTTP(t, app, token, "create-learning-state-001", "Agent")
	view := startLearningHTTP(t, app, token, sid, "Redis")
	_, busy, _ := requestHTTP(t, app.Handler, "POST", learningPath+"start", learningJSON(map[string]string{"sessionId": sid, "topic": "Redis"}), token)
	if busy["status_code"] != float64(code.LearningSessionActive) {
		t.Fatal(busy)
	}
	otherSID := createAgentHTTP(t, app, token, "create-learning-state-002", "Agent")
	otherTask := startLearningHTTP(t, app, token, otherSID, "Redis 过期")
	first := answerLearningBody(view, "isolation-submission-001")
	for _, bad := range []map[string]string{{"sessionId": otherSID, "learningSessionId": view["id"].(string), "questionId": first["questionId"], "submissionId": first["submissionId"], "answer": first["answer"]}, {"sessionId": otherSID, "learningSessionId": otherTask["id"].(string), "questionId": first["questionId"], "submissionId": first["submissionId"], "answer": first["answer"]}} {
		_, failure, _ := requestHTTP(t, app.Handler, "POST", learningPath+"answer", learningJSON(bad), token)
		if failure["status_code"] == float64(1000) {
			t.Fatal("cross-session/task accepted")
		}
	}
	_, foreign, _ := requestHTTP(t, app.Handler, "GET", learningPath+"current?sessionId="+sid, "", other)
	if foreign["status_code"] == float64(1000) {
		t.Fatal("foreign task readable")
	}
	var row model.LearningSession
	db.Where("id = ?", view["id"]).First(&row)
	if _, err := learningDAO.NewRepository(db).Transition(context.Background(), 1, sid, row.ID, row.Version-1, row.Status, model.LearningSessionCancelled); !errors.Is(err, learningDAO.ErrLearningStateConflict) {
		t.Fatalf("original version CAS %v", err)
	}
	cancelled := learningRequest(t, app.Handler, "cancel", learningJSON(map[string]any{"sessionId": sid, "learningSessionId": view["id"]}), token)
	if cancelled["learning"].(map[string]any)["status"] != "cancelled" {
		t.Fatal(cancelled)
	}
	_, twice, _ := requestHTTP(t, app.Handler, "POST", learningPath+"cancel", learningJSON(map[string]any{"sessionId": sid, "learningSessionId": view["id"]}), token)
	if twice["status_code"] == float64(1000) {
		t.Fatal("cancel is not idempotent success")
	}
	if err := db.Model(&model.LearningSession{}).Where("id = ?", otherTask["id"]).Update("expires_at", time.Now().Add(-time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	_, expired, _ := requestHTTP(t, app.Handler, "GET", learningPath+"current?sessionId="+otherSID, "", token)
	if expired["status_code"] != float64(code.LearningSessionExpired) {
		t.Fatal(expired)
	}
	agent.state.mu.Lock()
	grades := agent.state.gradingCalls
	agent.state.mu.Unlock()
	if grades != 0 {
		t.Fatal("invalid operations invoked grading")
	}
}
