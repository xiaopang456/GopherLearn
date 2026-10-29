package memory

import (
	memorydao "GopherAI/dao/memory"
	"GopherAI/model"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type recallRepository struct {
	query     memorydao.RecallQuery
	items     []model.LongTermMemory
	err       error
	callCount int

	globals         []model.LongTermMemory
	globalQuery     listAllActiveQuery
	globalErr       error
	globalCallCount int
}

func (r *recallRepository) Recall(_ context.Context, query memorydao.RecallQuery) ([]model.LongTermMemory, error) {
	r.query, r.callCount = query, r.callCount+1
	if r.err != nil {
		return nil, r.err
	}
	// The real DAO filters on memory_type IN (...). Without the same filter here
	// a fixture row of the wrong type reaches the ranker, and the test ends up
	// describing the fake rather than production.
	allowed := map[model.MemoryScope]bool{}
	for _, scope := range query.Scopes {
		allowed[scope] = true
	}
	matched := make([]model.LongTermMemory, 0, len(r.items))
	for _, item := range r.items {
		if allowed[item.Scope] {
			matched = append(matched, item)
		}
	}
	return matched, nil
}

// AGENT-018 splits recall by type: `profile` is fetched whole rather than
// matched, so the fake serves it from a separate slice.
func (r *recallRepository) ListAllActive(_ context.Context, userID int64, memoryType model.MemoryScope, minConfidence float64, limit int) ([]model.LongTermMemory, error) {
	r.globalQuery = listAllActiveQuery{UserID: userID, Scope: memoryType, MinConfidence: minConfidence, Limit: limit}
	r.globalCallCount++
	if r.globalErr != nil {
		return nil, r.globalErr
	}
	owned := make([]model.LongTermMemory, 0, len(r.globals))
	for _, item := range r.globals {
		if item.UserID == userID && item.Scope == memoryType && item.Confidence >= minConfidence {
			owned = append(owned, item)
		}
	}
	if len(owned) > limit {
		owned = owned[:limit]
	}
	return owned, nil
}

func (r *recallRepository) ListTopicKeys(_ context.Context, userID int64, limit int) ([]memorydao.TopicKeyRow, error) {
	if r.err != nil {
		return nil, r.err
	}
	seen := map[string]bool{}
	rows := make([]memorydao.TopicKeyRow, 0)
	for _, item := range r.items {
		if item.UserID == userID && item.Scope == model.MemoryScopeTopic && item.Status == model.LongTermMemoryActive && item.TopicKey != "" && !seen[item.TopicKey] {
			seen[item.TopicKey] = true
			rows = append(rows, memorydao.TopicKeyRow{TopicKey: item.TopicKey, Topic: item.Subject, Notes: 1})
		}
	}
	return rows, nil
}

func (r *recallRepository) ListByTopicKey(_ context.Context, userID int64, scope model.MemoryScope, topicKey string, minConfidence float64, limit int) ([]model.LongTermMemory, error) {
	rows := make([]model.LongTermMemory, 0)
	for _, item := range r.items {
		if item.UserID == userID && item.Scope == scope && item.Status == model.LongTermMemoryActive && item.TopicKey == topicKey && item.Confidence >= minConfidence {
			rows = append(rows, item)
		}
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

type listAllActiveQuery struct {
	UserID        int64
	Scope         model.MemoryScope
	MinConfidence float64
	Limit         int
}

type activeLearning struct {
	session *model.LearningSession
	err     error
}

func (a activeLearning) GetActive(context.Context, int64, string) (*model.LearningSession, error) {
	return a.session, a.err
}

type usageCapture struct {
	userID int64
	ids    []string
}

func (u *usageCapture) Record(userID int64, ids []string, _ time.Time) bool {
	u.userID, u.ids = userID, append([]string(nil), ids...)
	return true
}

func recalledMemory(id string, userID int64, memoryType model.MemoryScope, tags []string, confidence float64) model.LongTermMemory {
	topicKey := ""
	if memoryType == model.MemoryScopeTopic && len(tags) > 0 {
		topicKey = tags[0]
	}
	return model.LongTermMemory{
		ID: id, UserID: userID, Scope: memoryType, Subject: id, Content: id + " content", ContentHash: model.MemoryContentHash(id + " content"),
		StructuredJSON: `{}`, Tags: model.StringList(tags), TopicKey: topicKey, SourceRefs: model.StringList{"source:" + id},
		Durability: model.MemoryDurabilityStable, Confidence: confidence, Importance: 7, Status: model.LongTermMemoryActive,
	}
}

func TestRetrieverRanksTopicNotesAndDefensivelyIsolates(t *testing.T) {
	deployment := recalledMemory("deployment", 7, model.MemoryScopeTopic, []string{"docker", "gopherai", "deployment"}, .8)
	debugging := recalledMemory("debugging", 7, model.MemoryScopeTopic, []string{"docker", "deployment", "debugging"}, .95)
	unrelated := recalledMemory("unrelated", 7, model.MemoryScopeTopic, []string{"rpc", "protocol", "network"}, .99)
	foreign := recalledMemory("foreign", 8, model.MemoryScopeTopic, []string{"docker", "gopherai", "deployment"}, .99)
	archived := recalledMemory("archived", 7, model.MemoryScopeTopic, []string{"docker", "gopherai", "deployment"}, .99)
	archived.Status = model.LongTermMemoryArchived
	repository := &recallRepository{items: []model.LongTermMemory{debugging, unrelated, foreign, archived, deployment}}
	usage := &usageCapture{}
	retriever, err := NewRetriever(repository, nil, usage, DefaultRetrieverConfig())
	if err != nil {
		t.Fatal(err)
	}
	recalled, err := retriever.RelevantMemories(context.Background(), RecallRequest{UserID: 7, SessionID: "s", Question: "GopherAI 的 Docker 部署方案"})
	if err != nil {
		t.Fatal(err)
	}
	if len(recalled.Topic) != 2 || !strings.Contains(recalled.Topic[0], "debugging") || !strings.Contains(recalled.Topic[1], "deployment") {
		t.Fatalf("topic notes=%v", recalled.Topic)
	}
	if usage.userID != 7 || len(usage.ids) != 2 || usage.ids[0] != "debugging" {
		t.Fatalf("usage=%+v", usage)
	}
}

// AGENT-018: a preference applies to every technical question but resembles
// none of them, so it is injected whole rather than matched. This is the case
// no similarity method can serve, which is why the type skips retrieval.
func TestRetrieverAlwaysInjectsProfileEvenWithoutTagOverlap(t *testing.T) {
	preference := recalledMemory("preference", 7, model.MemoryScopeGlobal, []string{"preference"}, .9)
	preference.Durability = model.MemoryDurabilityStable
	foreign := recalledMemory("foreign-profile", 8, model.MemoryScopeGlobal, []string{"preference"}, .99)
	repository := &recallRepository{globals: []model.LongTermMemory{preference, foreign}}
	retriever, err := NewRetriever(repository, nil, nil, DefaultRetrieverConfig())
	if err != nil {
		t.Fatal(err)
	}
	// Nothing in this question overlaps the profile's tags.
	recalled, err := retriever.RelevantMemories(context.Background(), RecallRequest{UserID: 7, SessionID: "s", Question: "RRF 融合是怎么算的"})
	if err != nil {
		t.Fatal(err)
	}
	if len(recalled.Global) == 0 || !strings.Contains(recalled.Global[0], "preference") {
		t.Fatalf("profile was not injected: %v", recalled.Global)
	}
	for _, memory := range recalled.Global {
		if strings.Contains(memory, "foreign-profile") {
			t.Fatal("another user's profile leaked")
		}
	}
	// AGENT-019: the two blocks are returned separately so neither can consume
	// the other's token budget in the context builder.
	if len(recalled.Topic) != 0 {
		t.Errorf("no topic note matches this question, got %v", recalled.Topic)
	}
	if repository.globalQuery.Scope != model.MemoryScopeGlobal || repository.globalQuery.Limit != DefaultRetrieverConfig().GlobalLimit {
		t.Fatalf("profile query=%+v", repository.globalQuery)
	}
}

func TestRetrieverProfileFailureIsReported(t *testing.T) {
	repository := &recallRepository{globalErr: errors.New("mysql down")}
	retriever, err := NewRetriever(repository, nil, nil, DefaultRetrieverConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := retriever.RelevantMemories(context.Background(), RecallRequest{UserID: 7, SessionID: "s", Question: "docker"}); err == nil {
		t.Fatal("expected the profile read failure to surface to the context builder")
	}
}

func TestRetrieverPrioritizesUserConfirmedMemoryOverHigherConfidenceInference(t *testing.T) {
	confirmed := recalledMemory("confirmed", 7, model.MemoryScopeTopic, []string{"docker", "gopherai", "project"}, .7)
	confirmed.UserConfirmed = true
	inferred := recalledMemory("inferred", 7, model.MemoryScopeTopic, []string{"docker", "gopherai", "project"}, .99)
	repository := &recallRepository{items: []model.LongTermMemory{inferred, confirmed}}
	retriever, err := NewRetriever(repository, nil, nil, DefaultRetrieverConfig())
	if err != nil {
		t.Fatal(err)
	}
	recalled, err := retriever.RelevantMemories(context.Background(), RecallRequest{UserID: 7, SessionID: "s", Question: "GopherAI Docker 项目"})
	if err != nil {
		t.Fatal(err)
	}
	if len(recalled.Topic) != 2 || !strings.Contains(recalled.Topic[0], "confirmed") {
		t.Fatalf("user-confirmed memory was not first: %#v", recalled.Topic)
	}
}

func TestRetrieverLearningModePrioritizesLearningStateAndTopic(t *testing.T) {
	learning := recalledMemory("weak-watch", 7, model.MemoryScopeTopic, []string{"rpc", "watch", "learning"}, .72)
	learning.TopicKey = "rpc"
	profile := recalledMemory("profile", 7, model.MemoryScopeGlobal, []string{"rpc", "learning", "preference"}, .99)
	project := recalledMemory("project", 7, model.MemoryScopeTopic, []string{"rpc", "watch", "project"}, .99)
	repository := &recallRepository{items: []model.LongTermMemory{project, profile, learning}}
	retriever, _ := NewRetriever(repository, activeLearning{session: &model.LearningSession{Topic: "RPC", TopicKey: "rpc"}}, nil, DefaultRetrieverConfig())
	recalled, err := retriever.RelevantMemories(context.Background(), RecallRequest{UserID: 7, SessionID: "s", Question: "继续复习 Watch"})
	if err != nil {
		t.Fatal(err)
	}
	// The profile arrives in its own block now, so the retrieved list holds the
	// two topic notes and the topic-key hit ranks first.
	if len(recalled.Topic) != 2 || !strings.Contains(strings.Join(recalled.Topic, "\n"), "weak-watch") {
		t.Fatalf("learning ranking=%v", recalled.Topic)
	}
	if len(recalled.Global) != 0 {
		t.Errorf("no profile is stored in this fixture, got %v", recalled.Global)
	}
}

func TestRetrieverReturnsEmptyWithoutTagsAndPropagatesDAOFailure(t *testing.T) {
	repository := &recallRepository{}
	retriever, _ := NewRetriever(repository, nil, nil, DefaultRetrieverConfig())
	recalled, err := retriever.RelevantMemories(context.Background(), RecallRequest{UserID: 1, SessionID: "s", Question: "你好"})
	if err != nil || len(recalled.Topic) != 0 || len(recalled.Global) != 0 || repository.callCount != 0 {
		t.Fatalf("empty recall=%+v calls=%d err=%v", recalled, repository.callCount, err)
	}
	repository.err = errors.New("mysql unavailable")
	if _, err := retriever.RelevantMemories(context.Background(), RecallRequest{UserID: 1, SessionID: "s", Question: "Redis 并发问题"}); err == nil {
		t.Fatal("DAO failure was hidden by retriever")
	}
}

func TestRetrieverRequiresStrongTopicEvidenceAndRejectsExpiredRows(t *testing.T) {
	valid := recalledMemory("rpc-valid", 7, model.MemoryScopeTopic, []string{"rpc"}, .9)
	valid.TopicKey = "rpc-粘包"
	expired := recalledMemory("rpc-expired", 7, model.MemoryScopeTopic, []string{"rpc"}, .9)
	expired.TopicKey, expired.Durability = "rpc-粘包", model.MemoryDurabilityTimeBound
	past := time.Now().Add(-time.Minute)
	expired.ValidUntil = &past
	repository := &recallRepository{items: []model.LongTermMemory{valid, expired}}
	retriever, _ := NewRetriever(repository, nil, nil, DefaultRetrieverConfig())

	unrelated, err := retriever.RelevantMemories(context.Background(), RecallRequest{UserID: 7, SessionID: "s", Question: "HTTP 连接池怎么配"})
	if err != nil || len(unrelated.Topic) != 0 {
		t.Fatalf("unrelated=%+v err=%v", unrelated, err)
	}
	matched, err := retriever.RelevantMemories(context.Background(), RecallRequest{UserID: 7, SessionID: "s", Question: "继续之前的话题", StrongTopicKeys: []string{"rpc-粘包"}})
	if err != nil || len(matched.Topic) != 1 || !strings.Contains(matched.Topic[0], "rpc-valid") {
		t.Fatalf("matched=%+v err=%v", matched, err)
	}
}
