package hybrid

import (
	messageDAO "GopherAI/dao/message"
	sessionDAO "GopherAI/dao/session"
	"GopherAI/model"
	"GopherAI/utils"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cloudwego/eino/schema"
	"github.com/xiaopang456/GopherLearn/conversation"
	"gorm.io/gorm"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// This replaces only model transport, after the real store/history/prompt path.
// Answers are derived from the captured current question rather than a responder
// that supplies a fabricated history. Summary output uses its actual input too.
type ordinaryModel struct {
	mu       sync.Mutex
	requests []conversation.Request
	fail     bool
	cancel   context.CancelFunc
}

func (m *ordinaryModel) Complete(ctx context.Context, r conversation.Request) (conversation.Completion, error) {
	m.mu.Lock()
	m.requests = append(m.requests, r)
	fail, cancel := m.fail, m.cancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
		return conversation.Completion{}, ctx.Err()
	}
	if fail {
		return conversation.Completion{}, errors.New("synthetic model failure")
	}
	if err := ctx.Err(); err != nil {
		return conversation.Completion{}, err
	}
	q, _ := r.LastUserContent()
	if strings.Contains(r.Messages[0].Content, "会话摘要器") {
		return conversation.Completion{Content: "摘要保留：" + q}, nil
	}
	return conversation.Completion{Content: "回答：" + q}, nil
}
func (m *ordinaryModel) Stream(ctx context.Context, r conversation.Request, emit conversation.DeltaHandler) (conversation.Completion, error) {
	answer, err := m.Complete(ctx, r)
	if err != nil {
		return answer, err
	}
	if err := emit(answer.Content); err != nil {
		return conversation.Completion{}, err
	}
	if err := ctx.Err(); err != nil {
		return conversation.Completion{}, err
	}
	return answer, nil
}
func (m *ordinaryModel) all() []conversation.Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]conversation.Request(nil), m.requests...)
}

func fixture(t *testing.T, opts conversation.Options) (*Application, *gorm.DB, *ordinaryModel, *scriptedChatModel) {
	t.Helper()
	db := sqliteDatabase(t)
	m := &ordinaryModel{}
	agent := &scriptedChatModel{respond: func(context.Context, int) (*schema.Message, error) {
		return schema.AssistantMessage("agent response", nil), nil
	}}
	migrations := 0
	app, err := Assemble(context.Background(), testSettings(t), dependencies(t, db, agent, &testChunks{}, &migrations), m, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	if migrations != 0 {
		t.Fatal("business migration executed")
	}
	hash, err := utils.HashPassword("synthetic-password")
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range []model.User{{ID: 1, Username: "learner", Name: "learner", Password: hash}, {ID: 2, Username: "other", Name: "other"}} {
		if err := db.Create(&user).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Create(&model.Session{ID: "ordinary", UserID: 1, UserName: "learner", Mode: model.SessionModeNormal, Title: "ordinary"}).Error; err != nil {
		t.Fatal(err)
	}
	return app, db, m, agent
}
func messages(t *testing.T, db *gorm.DB) []model.Message {
	t.Helper()
	page, err := messageDAO.NewRepository(db).ListOwnedHistoryPage(context.Background(), 1, "ordinary", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	return page.Messages
}
func TestAssembleOrdinarySendStreamAndRestartHistory(t *testing.T) {
	app, db, m, agent := fixture(t, conversation.Options{})
	pair, err := app.Chat.Send(context.Background(), "1", "ordinary", "第一问")
	if err != nil {
		t.Fatal(err)
	}
	rows := messages(t, db)
	if len(rows) != 2 || !rows[0].IsUser || rows[1].IsUser || pair.User.ID != fmt.Sprint(rows[0].ID) || pair.Assistant.ID != fmt.Sprint(rows[1].ID) {
		t.Fatalf("original DAO records: %#v; pair=%#v", rows, pair)
	}
	var events []string
	if err := app.Chat.Stream(context.Background(), "1", "ordinary", "第二问", func(e conversation.Event) error { events = append(events, e.Name); return nil }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(events, ",") != "message.started,message.delta,message.completed" {
		t.Fatalf("events %v", events)
	}
	requests := m.all()
	checkHistory(t, requests[1], "第一问", "第二问")
	fresh, err := conversation.New(&Store{db: db}, m, conversation.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = fresh.Send(context.Background(), "1", "ordinary", "第三问"); err != nil {
		t.Fatal(err)
	}
	checkHistory(t, m.all()[2], "第二问", "第三问")
	if agent.CallCount() != 0 {
		t.Fatal("ordinary Chat called Agent")
	}
	for _, r := range m.all() {
		if len(r.Tools) != 0 {
			t.Fatal("ordinary tools offered")
		}
		for _, msg := range r.Messages {
			if msg.Role == "tool" || strings.Contains(msg.Content, "跨会话经历") || strings.Contains(msg.Content, "已确认记忆") {
				t.Fatalf("Agent context injected: %+v", r)
			}
		}
	}
	var tables []string
	if err := db.Raw("SELECT name FROM sqlite_master WHERE type='table'").Scan(&tables).Error; err != nil {
		t.Fatal(err)
	}
	for _, name := range tables {
		if strings.HasPrefix(name, "clean_") {
			t.Fatal("clean table created")
		}
	}
}
func checkHistory(t *testing.T, r conversation.Request, prior, current string) {
	t.Helper()
	count := 0
	found := false
	for _, m := range r.Messages {
		if m.Role == "user" && m.Content == current {
			count++
		}
		if m.Role == "user" && m.Content == prior {
			found = true
		}
	}
	if !found || count != 1 {
		t.Fatalf("actual model history: %+v", r.Messages)
	}
}
func TestAssembleSummaryStoredAndRecoveredVerbatim(t *testing.T) {
	opts := conversation.Options{SummaryEnabled: true, SummaryPolicy: conversation.SummaryPolicy{TriggerMessages: 4, KeepRecentMessages: 2, MaxInputMessages: 16, MaxInputRunes: 10000, MaxOutputTokens: 300}}
	app, db, m, _ := fixture(t, opts)
	for _, q := range []string{"甲", "乙", "丙"} {
		if _, err := app.Chat.Send(context.Background(), "1", "ordinary", q); err != nil {
			t.Fatal(err)
		}
	}
	stored, err := sessionDAO.NewRepository(db).GetSummary(context.Background(), 1, "ordinary")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Version != 1 || stored.CoveredMessageID != 2 || !strings.Contains(stored.Content, "用户：甲") || !strings.Contains(stored.Content, "助手：回答：甲") || strings.HasPrefix(stored.Content, "{") {
		t.Fatalf("original prose summary: %+v", stored)
	}
	requests := m.all()
	summaryRequest := requests[2]
	if summaryRequest.MaxOutputTokens != 300 || strings.Contains(summaryRequest.Messages[0].Content, "schema_version") {
		t.Fatalf("summary prompt changed: %+v", summaryRequest)
	}
	// A newly constructed service reads both raw history and the stored prose.
	fresh, err := conversation.New(&Store{db: db}, m, opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = fresh.Send(context.Background(), "1", "ordinary", "丁"); err != nil {
		t.Fatal(err)
	}
	last := m.all()[len(m.all())-1]
	joined := ""
	for _, msg := range last.Messages {
		joined += msg.Content + "\n"
	}
	if !strings.Contains(joined, stored.Content) || !strings.Contains(joined, "回答：丙") {
		t.Fatalf("summary/history not recovered: %+v", last)
	}
	for _, msg := range last.Messages {
		if msg.Role == "user" && msg.Content == "甲" {
			t.Fatal("covered history duplicated verbatim")
		}
	}
}
func TestAssembleFailurePersistenceAndTerminalContract(t *testing.T) {
	for _, name := range []string{"model-send", "model-stream", "user-store", "assistant-store", "touch-store", "cancel-model", "cancel-delta", "write-started", "write-delta", "write-completed"} {
		t.Run(name, func(t *testing.T) {
			app, db, m, _ := fixture(t, conversation.Options{})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sentinel := errors.New("synthetic stream write failure")
			expectedRows := 1
			expectedCalls := 1
			switch name {
			case "model-send", "model-stream":
				m.fail = true
			case "cancel-model":
				m.cancel = cancel
			case "user-store", "assistant-store":
				if err := db.Callback().Create().Before("gorm:create").Register("test:fail-message", func(tx *gorm.DB) {
					if row, ok := tx.Statement.Dest.(*model.Message); ok && ((name == "user-store" && row.IsUser) || (name == "assistant-store" && !row.IsUser)) {
						tx.AddError(errors.New("synthetic SQL create failure"))
					}
				}); err != nil {
					t.Fatal(err)
				}
				if name == "user-store" {
					expectedRows = 0
					expectedCalls = 0
				}
			case "touch-store":
				if err := db.Callback().Update().Before("gorm:update").Register("test:fail-touch", func(tx *gorm.DB) {
					if tx.Statement.Table == "sessions" {
						tx.AddError(errors.New("synthetic SQL touch failure"))
					}
				}); err != nil {
					t.Fatal(err)
				}
				expectedRows = 0
				expectedCalls = 0
			case "write-started":
				expectedCalls = 0
			case "write-completed":
				expectedRows = 2
			}
			var events []string
			emit := func(e conversation.Event) error {
				events = append(events, e.Name)
				if name == "cancel-delta" && e.Name == "message.delta" {
					cancel()
				}
				if name == "write-started" && e.Name == "message.started" || name == "write-delta" && e.Name == "message.delta" || name == "write-completed" && e.Name == "message.completed" {
					return sentinel
				}
				return nil
			}
			var err error
			if name == "model-send" {
				_, err = app.Chat.Send(ctx, "1", "ordinary", "失败问题")
			} else {
				err = app.Chat.Stream(ctx, "1", "ordinary", "失败问题", emit)
			}
			if err == nil {
				t.Fatal("expected failure")
			}
			switch {
			case name == "write-started" || name == "write-completed":
				if !errors.Is(err, sentinel) {
					t.Fatalf("write identity lost: %v", err)
				}
			case strings.HasPrefix(name, "cancel-"):
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel identity lost: %v", err)
				}
			default:
				if !errors.Is(err, conversation.ErrInternal) {
					t.Fatalf("internal mapping: %v", err)
				}
			}
			rows := messages(t, db)
			if len(rows) != expectedRows || len(m.all()) != expectedCalls {
				t.Fatalf("rows=%d calls=%d want %d/%d; err=%v events=%v", len(rows), len(m.all()), expectedRows, expectedCalls, err, events)
			}
			if name == "model-stream" || name == "assistant-store" {
				if events[len(events)-1] != "message.failed" {
					t.Fatalf("failure terminal: %v", events)
				}
			}
			if strings.HasPrefix(name, "cancel-") {
				for _, event := range events {
					if event == "message.completed" || event == "message.error" {
						t.Fatalf("cancel terminal fabricated: %v", events)
					}
				}
			}
		})
	}
}
func TestAssembleOwnershipStoredModeAndNumericPagination(t *testing.T) {
	app, db, m, _ := fixture(t, conversation.Options{Budget: conversation.Budget{MaxMessages: 300, MaxRunes: 30000}})
	if _, err := app.Chat.Send(context.Background(), "2", "ordinary", "foreign"); err == nil {
		t.Fatal("cross user accepted")
	}
	if _, err := app.Chat.Send(context.Background(), "01", "ordinary", "alias"); err == nil {
		t.Fatal("noncanonical user ID accepted")
	}
	if err := db.Create(&model.Session{ID: "agent", UserID: 1, UserName: "learner", Mode: model.SessionModeAgent}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := app.Chat.Send(context.Background(), "1", "agent", "switch mode"); err == nil {
		t.Fatal("stored Agent mode entered ordinary Chat")
	}
	if len(m.all()) != 0 || len(messages(t, db)) != 0 {
		t.Fatal("rejected calls reached model/store")
	}
	for i := 0; i < 55; i++ {
		if _, err := app.Chat.Send(context.Background(), "1", "ordinary", fmt.Sprintf("q%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	requests := m.all()
	last := requests[len(requests)-1]
	var users []string
	for _, msg := range last.Messages {
		if msg.Role == "user" {
			users = append(users, msg.Content)
		}
	}
	if len(users) != 55 {
		t.Fatalf("pagination truncated history: %d", len(users))
	}
	for i, q := range users {
		if q != fmt.Sprintf("q%d", i) {
			t.Fatalf("numeric order %d: %s", i, q)
		}
	}
}
func TestAssembleRequiresExplicitDependenciesAndPreservesNativeAgentRoute(t *testing.T) {
	app, db, ordinary, agent := fixture(t, conversation.Options{})
	post := func(path, body, token string) map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		app.Agent.Handler.ServeHTTP(w, req)
		var result map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
			t.Fatalf("native %s: %d %s", path, w.Code, w.Body.String())
		}
		return result
	}
	login := post("/api/v1/user/login", `{"username":"learner","password":"synthetic-password"}`, "")
	token, _ := login["token"].(string)
	if token == "" {
		t.Fatalf("original User authentication: %v", login)
	}
	created := post("/api/v1/AI/chat/session", `{"modelType":"agent","requestId":"hybrid-create-agent"}`, token)
	sid, _ := created["sessionId"].(string)
	if sid == "" {
		t.Fatalf("original Session creation: %v", created)
	}
	result := post("/api/v1/AI/agent/run", fmt.Sprintf(`{"sessionId":%q,"requestId":"hybrid-agent-run","question":"你好","knowledgePolicy":"knowledge_preferred"}`, sid), token)
	if result["answer"] != "agent response" {
		t.Fatalf("original Agent execution: %v", result)
	}
	page, err := messageDAO.NewRepository(db).ListOwnedHistoryPage(context.Background(), 1, sid, 0, 100)
	if err != nil || len(page.Messages) != 2 || agent.CallCount() == 0 || len(ordinary.all()) != 0 {
		t.Fatalf("shared V2 storage/isolated execution: %+v err=%v", page, err)
	}
	if _, err := app.Chat.Send(context.Background(), "1", "ordinary", "same learner"); err != nil {
		t.Fatal(err)
	}
	if len(messages(t, db)) != 2 {
		t.Fatal("ordinary and Agent not on same database")
	}
}

func TestAssembleSummaryFailureUsesFrozenOrdinarySemantics(t *testing.T) {
	for _, name := range []string{"model", "store", "cancel"} {
		t.Run(name, func(t *testing.T) {
			opts := conversation.Options{SummaryEnabled: true, SummaryPolicy: conversation.SummaryPolicy{TriggerMessages: 4, KeepRecentMessages: 2}}
			app, db, m, _ := fixture(t, opts)
			for _, q := range []string{"old-one", "old-two"} {
				if _, err := app.Chat.Send(context.Background(), "1", "ordinary", q); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch name {
			case "model":
				m.fail = true
			case "cancel":
				m.cancel = cancel
			case "store":
				if err := db.Callback().Create().Before("gorm:create").Register("test:summary-store-fail", func(tx *gorm.DB) {
					if _, ok := tx.Statement.Dest.(*model.SessionSummary); ok {
						tx.AddError(errors.New("synthetic summary storage failure"))
					}
				}); err != nil {
					t.Fatal(err)
				}
			}
			var events []string
			err := app.Chat.Stream(ctx, "1", "ordinary", "new-question", func(e conversation.Event) error { events = append(events, e.Name); return nil })
			if name == "cancel" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("summary cancel %v", err)
				}
			} else {
				if !errors.Is(err, conversation.ErrInternal) {
					t.Fatalf("summary failure %v", err)
				}
			}
			if len(messages(t, db)) != 5 || len(m.all()) != 3 {
				t.Fatalf("summary failure answered/degraded: rows=%d calls=%d", len(messages(t, db)), len(m.all()))
			}
			if _, err := sessionDAO.NewRepository(db).GetSummary(context.Background(), 1, "ordinary"); !errors.Is(err, sessionDAO.ErrSummaryNotFound) {
				t.Fatalf("failed summary persisted: %v", err)
			}
			expected := "message.started,message.failed"
			if name == "cancel" {
				expected = "message.started"
			}
			if strings.Join(events, ",") != expected {
				t.Fatalf("summary terminal %v", events)
			}
		})
	}
}

func TestOriginalV2SummaryWatermarkIsNotWeakened(t *testing.T) {
	opts := conversation.Options{SummaryEnabled: true, SummaryPolicy: conversation.SummaryPolicy{TriggerMessages: 4, KeepRecentMessages: 2}}
	app, db, _, _ := fixture(t, opts)
	for _, q := range []string{"one", "two", "three"} {
		if _, err := app.Chat.Send(context.Background(), "1", "ordinary", q); err != nil {
			t.Fatal(err)
		}
	}
	store := &Store{db: db}
	current, err := store.GetSessionSummary(context.Background(), "1", "ordinary")
	if err != nil {
		t.Fatal(err)
	}
	candidate := *current
	candidate.Content = "same watermark overwrite must remain unsupported"
	if _, err := store.CommitSessionSummary(context.Background(), &candidate, current.Version); !errors.Is(err, conversation.ErrConflict) {
		t.Fatalf("V2 forward-watermark rule changed: %v", err)
	}
	after, err := store.GetSessionSummary(context.Background(), "1", "ordinary")
	if err != nil || after.Content != current.Content || after.Version != current.Version {
		t.Fatalf("refused overwrite changed facts: %+v %v", after, err)
	}
	if _, err := store.GetSessionSummary(context.Background(), "2", "ordinary"); !errors.Is(err, conversation.ErrNotFound) {
		t.Fatalf("foreign summary exposed: %v", err)
	}
}
