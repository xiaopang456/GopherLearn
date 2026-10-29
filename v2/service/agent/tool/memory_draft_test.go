package agenttool

import (
	"GopherAI/model"
	serviceagent "GopherAI/service/agent"
	memoryservice "GopherAI/service/memory"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
)

type fakeDraftProposer struct{ last memoryservice.DraftRequest }

func (f *fakeDraftProposer) ProposeDraft(_ context.Context, request memoryservice.DraftRequest) (*model.MemoryDraft, bool, error) {
	f.last = request
	return &model.MemoryDraft{ID: "draft", Scope: request.Input.Scope, Subject: request.Input.Subject, Content: request.Input.Content, Tags: request.Input.Tags, TopicKey: request.Input.TopicKey, Durability: request.Input.Durability, ConflictIDs: model.StringList{}}, true, nil
}

func TestMemoryDraftToolDescriptionProvidesClockForTimeBoundDates(t *testing.T) {
	registry, err := NewMemoryDraftRegistry(&fakeDraftProposer{})
	if err != nil {
		t.Fatal(err)
	}
	frozen := time.Date(2026, time.August, 28, 12, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	registry.UseClock(func() time.Time { return frozen })
	tools, err := registry.Tools(context.Background(), serviceagent.RunContext{
		UserID: 7, SessionID: "session", RunID: "run", Question: "请记住：9 月 15 日前完成消息队列复习",
	})
	if err != nil || len(tools) != 1 {
		t.Fatalf("tools=%d err=%v", len(tools), err)
	}
	info, err := tools[0].Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"2026-08-28T12:00:00+08:00", "next future occurrence", "future RFC3339 timestamp with timezone"} {
		if !strings.Contains(info.Desc, required) {
			t.Fatalf("description misses %q: %s", required, info.Desc)
		}
	}
}

func TestMemoryDraftToolSchemaExcludesAuthorityFields(t *testing.T) {
	registry, err := NewMemoryDraftRegistry(&fakeDraftProposer{})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := registry.Tools(context.Background(), serviceagent.RunContext{UserID: 7, SessionID: "session", RunID: "run", Question: "请记住我更喜欢先看整体链路"})
	if err != nil || len(tools) != 1 {
		t.Fatalf("tools=%d err=%v", len(tools), err)
	}
	info, err := tools[0].Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	schema, err := info.ToJSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ToLower(string(payload))
	for _, forbidden := range []string{"user_id", "status", "actor", "decision", "target_memory_id", "database"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("memory proposal schema contains authority field %q: %s", forbidden, text)
		}
	}
	for _, required := range []string{"scope", "subject", "content", "durability"} {
		if !strings.Contains(text, `"`+required+`"`) {
			t.Fatalf("memory proposal schema missing %q: %s", required, text)
		}
	}
}

func TestMemoryDraftToolDropsUnknownModelTagsBeforeStrictServiceBoundary(t *testing.T) {
	proposer := &fakeDraftProposer{}
	registry, err := NewMemoryDraftRegistry(proposer)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := registry.Tools(context.Background(), serviceagent.RunContext{UserID: 7, SessionID: "session", RunID: "run", Question: "请记住这个 HTTP 困惑"})
	if err != nil || len(tools) != 1 {
		t.Fatalf("tools=%d err=%v", len(tools), err)
	}
	if _, err = tools[0].(tool.InvokableTool).InvokableRun(context.Background(), `{"scope":"topic","subject":"HTTP","content":"我会混淆两种消息边界","tags":["network","confusion","nimbustream-913"],"topic_key":"NimbusStream-913","durability":"stable"}`); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(proposer.last.Input.Tags, ","); got != "network" {
		t.Fatalf("normalized tags=%q, want network", got)
	}
}

func TestMemoryDraftToolIsHiddenWithoutExplicitUserIntent(t *testing.T) {
	proposer := &fakeDraftProposer{}
	registry, err := NewMemoryDraftRegistry(proposer)
	if err != nil {
		t.Fatal(err)
	}
	for _, question := range []string{
		"PacketBuffer 怎么实现的",
		"结合我的笔记解释 RPC 粘包",
		"帮我出三道题",
		"你还记得我的讲解偏好吗？",
		"Do you remember my preferred explanation style?",
	} {
		tools, err := registry.Tools(context.Background(), serviceagent.RunContext{
			UserID: 7, SessionID: "session", RunID: "run", Question: question,
		})
		if err != nil || len(tools) != 0 {
			t.Fatalf("question=%q tools=%d err=%v", question, len(tools), err)
		}
	}
}

func TestExplicitMemoryIntentAcceptsDirectAndPersistentInstructions(t *testing.T) {
	registry, err := NewMemoryDraftRegistry(&fakeDraftProposer{})
	if err != nil {
		t.Fatal(err)
	}
	for _, question := range []string{
		"请记住我的讲解偏好",
		"这个踩坑帮我记下来",
		"以后讲传输层的时候请默认我已经知道这一点",
		"今后分析线上故障时，请始终先确认数据是否还能恢复",
		"From now on, always explain concurrency with a timeline.",
	} {
		tools, err := registry.Tools(context.Background(), serviceagent.RunContext{
			UserID: 7, SessionID: "session", RunID: "run", Question: question,
		})
		if err != nil || len(tools) != 1 {
			t.Fatalf("question=%q tools=%d err=%v", question, len(tools), err)
		}
	}
}

func TestResolveDraftTopicKeyUsesUniqueTopicNamedByUser(t *testing.T) {
	topics := []memoryservice.Topic{
		{Key: "tcp粘包", Name: "TCP 粘包", Graded: true},
		{Key: "jwt鉴权", Name: "JWT 鉴权", Graded: true},
	}
	if got := resolveDraftTopicKey(topics, "请记住我对 TCP 粘包的困惑", "tcp-sticky-packet"); got != "tcp粘包" {
		t.Fatalf("resolved key=%q", got)
	}
	if got := resolveDraftTopicKey(topics, "请记住我对一个全新协议的理解", "new-protocol"); got != "new-protocol" {
		t.Fatalf("new topic key=%q", got)
	}
}

func TestMemoryDraftToolDescriptionCarriesExistingTopicNamespace(t *testing.T) {
	registry, err := NewMemoryDraftRegistry(&fakeDraftProposer{})
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := memoryservice.NewTopicCatalogue(stubDraftTopics{}, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	registry.UseTopicCatalogue(catalogue)
	tools, err := registry.Tools(context.Background(), serviceagent.RunContext{UserID: 7, SessionID: "session", RunID: "run", Question: "请记住 PacketBuffer 的这个坑"})
	if err != nil || len(tools) != 1 {
		t.Fatalf("tools=%d err=%v", len(tools), err)
	}
	info, err := tools[0].Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(info.Desc, "tcp粘包") {
		t.Fatalf("description misses shared topic key: %s", info.Desc)
	}
}

type stubDraftTopics struct{}

func (stubDraftTopics) ListTopicProgress(_ context.Context, userID int64, _ int) ([]model.TopicProgress, error) {
	return []model.TopicProgress{{UserID: userID, Topic: "TCP 粘包", TopicKey: "tcp粘包"}}, nil
}
