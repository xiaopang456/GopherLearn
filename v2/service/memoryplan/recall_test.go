package memoryplan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	episodedao "GopherAI/dao/episode"
	memorydao "GopherAI/dao/memory"
	"GopherAI/model"

	modelcomponent "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

var clock = time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC)

// ---------------------------------------------------------------- fakes -----

// fakeStore stands in for repositories that production calls concurrently, so
// its own bookkeeping is guarded. Without the lock the race detector reports the
// fake rather than the code under test, which is noise that hides the signal.
type fakeStore struct {
	mu       sync.Mutex
	memories []model.LongTermMemory
	episodes []model.EpisodeMemory

	topicCalls   int
	byTopicCalls []string
	loadCalls    [][]string
	timeCalls    int
	failTopics   error
	failLoad     error
}

func (f *fakeStore) ListTopicKeys(_ context.Context, userID int64, limit int) ([]memorydao.TopicKeyRow, error) {
	f.mu.Lock()
	f.topicCalls++
	f.mu.Unlock()
	if f.failTopics != nil {
		return nil, f.failTopics
	}
	counts := map[string]int{}
	order := []string{}
	for _, memory := range f.memories {
		if !eligibleMemory(memory, userID, clock, 0.6) {
			continue
		}
		if _, seen := counts[memory.TopicKey]; !seen {
			order = append(order, memory.TopicKey)
		}
		counts[memory.TopicKey]++
	}
	rows := make([]memorydao.TopicKeyRow, 0, len(order))
	for _, key := range order {
		if len(rows) >= limit {
			break
		}
		rows = append(rows, memorydao.TopicKeyRow{TopicKey: key, Topic: key, Notes: counts[key]})
	}
	return rows, nil
}

func (f *fakeStore) ListByTopicKey(_ context.Context, userID int64, scope model.MemoryScope,
	topicKey string, minConfidence float64, limit int) ([]model.LongTermMemory, error) {
	f.mu.Lock()
	f.byTopicCalls = append(f.byTopicCalls, topicKey)
	f.mu.Unlock()
	out := make([]model.LongTermMemory, 0, limit)
	for _, memory := range f.memories {
		if len(out) >= limit {
			break
		}
		// The fake applies the production recall predicate, so a fixture the
		// real query would never return does not reach the code under test
		// through a fake that was more generous than MySQL.
		if memory.TopicKey != topicKey || memory.Scope != scope {
			continue
		}
		if !eligibleMemory(memory, userID, clock, minConfidence) {
			continue
		}
		out = append(out, memory)
	}
	return out, nil
}

func (f *fakeStore) LoadCandidates(_ context.Context, userID int64, currentSession string, ids []string) ([]model.EpisodeMemory, error) {
	f.mu.Lock()
	f.loadCalls = append(f.loadCalls, append([]string(nil), ids...))
	f.mu.Unlock()
	if f.failLoad != nil {
		return nil, f.failLoad
	}
	wanted := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		wanted[id] = struct{}{}
	}
	out := make([]model.EpisodeMemory, 0, len(ids))
	for _, episode := range f.episodes {
		if _, asked := wanted[episode.ID]; !asked {
			continue
		}
		if !f.eligibleEpisode(episode, userID, currentSession) {
			continue
		}
		out = append(out, episode)
	}
	return out, nil
}

func (f *fakeStore) ListByTime(_ context.Context, userID int64, currentSession string,
	from, to time.Time, limit int) ([]model.EpisodeMemory, error) {
	f.mu.Lock()
	f.timeCalls++
	f.mu.Unlock()
	out := make([]model.EpisodeMemory, 0, limit)
	for _, episode := range f.episodes {
		if len(out) >= limit {
			break
		}
		if !f.eligibleEpisode(episode, userID, currentSession) {
			continue
		}
		if !from.IsZero() && episode.LastMessageAt.Before(from) {
			continue
		}
		if !to.IsZero() && !episode.LastMessageAt.Before(to) {
			continue
		}
		out = append(out, episode)
	}
	return out, nil
}

// eligibleEpisode is the fake's copy of the production source boundary: owner,
// current session, status, index status and the transitive source closure.
func (f *fakeStore) eligibleEpisode(episode model.EpisodeMemory, userID int64, currentSession string) bool {
	if episode.UserID != userID || episode.SessionID == currentSession {
		return false
	}
	if episode.Status != model.EpisodeStatusActive || episode.IndexStatus != model.EpisodeIndexIndexed {
		return false
	}
	for _, source := range episode.SourceEpisodes {
		found := false
		for _, other := range f.episodes {
			if other.ID != source.EpisodeID {
				continue
			}
			found = other.Status == model.EpisodeStatusActive && other.Version == source.Version
		}
		if !found {
			return false
		}
	}
	return true
}

type fakeSearch struct {
	mu                     sync.Mutex
	exact, lexical, vector []string
	failExact              error
	failLexical            error
	vectorCalls            int
}

func (f *fakeSearch) results(ids []string) []episodedao.EpisodeSearchResult {
	out := make([]episodedao.EpisodeSearchResult, 0, len(ids))
	for _, id := range ids {
		out = append(out, episodedao.EpisodeSearchResult{
			EpisodeID: id, EmbeddingVersion: episodedao.CurrentEpisodeEmbeddingVersion,
		})
	}
	return out
}

func (f *fakeSearch) SearchExactTopics(_ context.Context, _ int64, _ string, _ []string, _ int) ([]episodedao.EpisodeSearchResult, error) {
	if f.failExact != nil {
		return nil, f.failExact
	}
	return f.results(f.exact), nil
}

func (f *fakeSearch) SearchByText(_ context.Context, _ int64, _ string, _ []string, _ int) ([]episodedao.EpisodeSearchResult, error) {
	if f.failLexical != nil {
		return nil, f.failLexical
	}
	return f.results(f.lexical), nil
}

func (f *fakeSearch) SearchByVector(_ context.Context, _ int64, _ string, _ []float32, _ int) ([]episodedao.EpisodeSearchResult, error) {
	f.mu.Lock()
	f.vectorCalls++
	f.mu.Unlock()
	return f.results(f.vector), nil
}

type fakeEmbedder struct {
	mu    sync.Mutex
	err   error
	calls int
}

func (f *fakeEmbedder) EmbedQuery(context.Context, string) ([]float32, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return []float32{0.1, 0.2}, nil
}

// fakeChat replays scripted replies and records what it was asked.
type fakeChat struct {
	mu       sync.Mutex
	replies  []reply
	index    int
	prompts  []string
	payloads []string
	inFlight int
	peak     int
	delay    time.Duration
}

type reply struct {
	message *schema.Message
	err     error
}

func (f *fakeChat) WithTools([]*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	return f, nil
}

func (f *fakeChat) Generate(ctx context.Context, input []*schema.Message, _ ...modelcomponent.Option) (*schema.Message, error) {
	f.mu.Lock()
	f.inFlight++
	if f.inFlight > f.peak {
		f.peak = f.inFlight
	}
	if len(input) > 0 {
		f.prompts = append(f.prompts, input[0].Content)
	}
	if len(input) > 1 {
		f.payloads = append(f.payloads, input[1].Content)
	}
	current := f.index
	if current < len(f.replies) {
		f.index++
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inFlight--
		f.mu.Unlock()
	}()
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if current >= len(f.replies) {
		return nil, errors.New("fake chat ran out of scripted replies")
	}
	return f.replies[current].message, f.replies[current].err
}

// Stream exists only to satisfy the interface. This path never streams: both
// stages are one forced tool call whose whole value is the decoded arguments,
// and a partial tool call is not a partial answer.
func (f *fakeChat) Stream(context.Context, []*schema.Message, ...modelcomponent.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, errors.New("the memory planner never streams")
}

func (f *fakeChat) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.index
}

// ------------------------------------------------------------- fixtures -----

func memoryFixture(id, topic string, status model.LongTermMemoryStatus, owner int64) model.LongTermMemory {
	return model.LongTermMemory{
		ID: id, UserID: owner, Scope: model.MemoryScopeTopic, TopicKey: topic,
		Subject: "主题 " + topic, Content: "关于 " + topic + " 的理解 " + id,
		Status: status, Confidence: 0.9, Importance: 5,
		Durability: model.MemoryDurabilityStable, UpdatedAt: clock.Add(-time.Hour),
	}
}

func expiredMemory(id, topic string, owner int64) model.LongTermMemory {
	memory := memoryFixture(id, topic, model.LongTermMemoryActive, owner)
	memory.Durability = model.MemoryDurabilityTimeBound
	past := clock.Add(-24 * time.Hour)
	memory.ValidUntil = &past
	return memory
}

func episodeFixture(id, session, topic string, owner int64, status model.EpisodeStatus, age time.Duration) model.EpisodeMemory {
	return model.EpisodeMemory{
		ID: id, UserID: owner, SessionID: session, Title: "会话 " + id,
		Document: model.EpisodeDocument{
			Synopsis:  "一次关于 " + topic + " 的讨论",
			Decisions: []model.EpisodeEvidence{{Text: "决定 " + id, EvidenceMessageIDs: []uint{4}}},
		},
		TopicKeys: model.StringList{topic}, LastMessageAt: clock.Add(-age),
		Version: 1, Status: status, IndexStatus: model.EpisodeIndexIndexed,
	}
}

const owner = int64(41)
const currentSession = "s-now"

func standardStore() *fakeStore {
	return &fakeStore{
		memories: []model.LongTermMemory{
			memoryFixture("m-1", "漏桶算法", model.LongTermMemoryActive, owner),
			memoryFixture("m-2", "漏桶算法", model.LongTermMemoryActive, owner),
			memoryFixture("m-archived", "网关限速", model.LongTermMemoryArchived, owner),
			memoryFixture("m-superseded", "信号处理", model.LongTermMemorySuperseded, owner),
			expiredMemory("m-expired", "压测窗口", owner),
			memoryFixture("m-foreign", "漏桶算法", model.LongTermMemoryActive, owner+1),
			memoryFixture("m-3", "死信队列", model.LongTermMemoryActive, owner),
		},
		episodes: []model.EpisodeMemory{
			episodeFixture("e-1", "s-1", "漏桶算法", owner, model.EpisodeStatusActive, 48*time.Hour),
			episodeFixture("e-2", "s-2", "死信队列", owner, model.EpisodeStatusActive, 24*time.Hour),
			episodeFixture("e-archived", "s-3", "日志采样", owner, model.EpisodeStatusArchived, 72*time.Hour),
			episodeFixture("e-foreign", "s-4", "漏桶算法", owner+1, model.EpisodeStatusActive, 12*time.Hour),
			episodeFixture("e-current", currentSession, "漏桶算法", owner, model.EpisodeStatusActive, time.Hour),
		},
	}
}

func newCatalogue(t *testing.T, store *fakeStore) *CatalogueBuilder {
	t.Helper()
	builder, err := NewCatalogueBuilder(store, store, 0.6)
	if err != nil {
		t.Fatalf("catalogue builder: %v", err)
	}
	return builder
}

// ------------------------------------------------------------- catalogue ----

// Everything a foreign, retired, expired or source-invalid record could be is
// absent from the rendered prompt, checked on the bytes rather than on the
// structs, because the bytes are what the model sees.
func TestTheCatalogueShowsOnlyTheOwnersOwnValidRecords(t *testing.T) {
	store := standardStore()
	snapshot, err := newCatalogue(t, store).Build(context.Background(), owner, currentSession, clock)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	rendered, err := snapshot.Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, forbidden := range []string{
		"m-archived", "m-superseded", "m-expired", "m-foreign",
		"e-archived", "e-foreign", "e-current",
		"网关限速", "信号处理", "压测窗口", "日志采样",
	} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("the catalogue leaked %q into the planner prompt", forbidden)
		}
	}
	for _, required := range []string{"m-1", "m-2", "m-3", "e-1", "e-2", "漏桶算法", "死信队列"} {
		if !strings.Contains(rendered, required) {
			t.Fatalf("the catalogue omitted %q", required)
		}
	}
}

func TestTheCatalogueCarriesNoBodies(t *testing.T) {
	store := standardStore()
	snapshot, _ := newCatalogue(t, store).Build(context.Background(), owner, currentSession, clock)
	rendered, _ := snapshot.Render()
	if strings.Contains(rendered, "关于 漏桶算法 的理解") {
		t.Fatal("a memory body reached the planner prompt; the catalogue is metadata only")
	}
	if strings.Contains(rendered, "一次关于") || strings.Contains(rendered, "决定 e-1") {
		t.Fatal("an episode body reached the planner prompt")
	}
}

func TestTheSameDataProducesTheSameCatalogueVersion(t *testing.T) {
	first, _ := newCatalogue(t, standardStore()).Build(context.Background(), owner, currentSession, clock)
	second, _ := newCatalogue(t, standardStore()).Build(context.Background(), owner, currentSession, clock)
	if first.CatalogueVersion != second.CatalogueVersion {
		t.Fatal("identical data must produce an identical catalogue version")
	}
	changed := standardStore()
	changed.memories = append(changed.memories, memoryFixture("m-4", "优雅关闭", model.LongTermMemoryActive, owner))
	third, _ := newCatalogue(t, changed).Build(context.Background(), owner, currentSession, clock)
	if third.CatalogueVersion == first.CatalogueVersion {
		t.Fatal("a new topic must change the catalogue version")
	}
}

// Overflow is announced. A truncated catalogue that called itself complete would
// make "the planner did not ask" and "the planner was never shown" the same
// observation, and those are different failures with different fixes.
func TestCatalogueOverflowIsExplicit(t *testing.T) {
	store := &fakeStore{}
	for index := 0; index < MaxCatalogueTopicGroups+5; index++ {
		store.memories = append(store.memories,
			memoryFixture(fmt.Sprintf("m-%03d", index), fmt.Sprintf("主题%03d", index), model.LongTermMemoryActive, owner))
	}
	for index := 0; index < MaxCatalogueEpisodes+5; index++ {
		store.episodes = append(store.episodes, episodeFixture(
			fmt.Sprintf("e-%03d", index), fmt.Sprintf("s-%03d", index), "主题000",
			owner, model.EpisodeStatusActive, time.Duration(index)*time.Hour))
	}
	snapshot, err := newCatalogue(t, store).Build(context.Background(), owner, currentSession, clock)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if snapshot.TopicComplete || snapshot.EpisodeComplete {
		t.Fatal("an overflowing catalogue must not describe itself as complete")
	}
	if snapshot.OldestVisible == "" {
		t.Fatal("a truncated episode catalogue must say how far back it reaches")
	}
	if len(snapshot.Topics) > MaxCatalogueTopicGroups || len(snapshot.Episodes) > MaxCatalogueEpisodes {
		t.Fatal("the catalogue exceeded its own limits")
	}
	if snapshot.Tokens() > MaxCatalogueTokens {
		t.Fatalf("the catalogue is %d tokens, over the %d limit", snapshot.Tokens(), MaxCatalogueTokens)
	}
}

func TestCatalogueOrderIsStable(t *testing.T) {
	var previous string
	for attempt := 0; attempt < 5; attempt++ {
		snapshot, _ := newCatalogue(t, standardStore()).Build(context.Background(), owner, currentSession, clock)
		rendered, _ := snapshot.Render()
		if attempt > 0 && rendered != previous {
			t.Fatal("the catalogue must be byte-identical across builds over the same data")
		}
		previous = rendered
	}
}

// ------------------------------------------------------------ candidates ----

func newCandidates(t *testing.T, store *fakeStore, search EpisodeSearcher, embedder QueryEmbedder) *CandidateBuilder {
	t.Helper()
	builder, err := NewCandidateBuilder(store, search, store, embedder, 0.6)
	if err != nil {
		t.Fatalf("candidate builder: %v", err)
	}
	return builder
}

func candidateRequest(readPlan MemoryReadPlan) CandidateRequest {
	return CandidateRequest{
		UserID: owner, SessionID: currentSession, Question: "问题", Now: clock,
		Plan: readPlan, CandidateDeep: 20,
	}
}

// The four channels union by id in a fixed order, so the pool does not depend on
// which channel happened to answer first.
func TestTheFourEpisodeChannelsDeduplicateInAStableOrder(t *testing.T) {
	store := standardStore()
	search := &fakeSearch{exact: []string{"e-2", "e-1"}, lexical: []string{"e-1"}, vector: []string{"e-2"}}
	readPlan := plan(DecisionEpisodic, []string{"漏桶算法"}, []string{"e-1"}, "关于死信的讨论")
	readPlan.TimeScope = TimeScope{Kind: TimeLatest}
	set, err := newCandidates(t, store, search, &fakeEmbedder{}).Build(context.Background(), candidateRequest(readPlan))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	ids := make([]string, 0, len(set.Episodes))
	for _, candidate := range set.Episodes {
		ids = append(ids, candidate.ID)
	}
	if len(ids) != 2 || ids[0] != "e-1" || ids[1] != "e-2" {
		t.Fatalf("expected the explicit id first and no duplicates, got %v", ids)
	}
	if len(store.loadCalls) != 1 {
		t.Fatalf("the pool must be reloaded once, not %d times", len(store.loadCalls))
	}
	if channels := set.Episodes[0].Channels; len(channels) < 2 {
		t.Fatalf("a candidate found by several channels must record all of them, got %v", channels)
	}
}

// Every lifecycle and tenancy failure is refused before the Verifier can see it.
// A foreign or retired candidate reaching the second call is an implementation
// error, not an input the model is expected to reject.
func TestNoForeignOrRetiredCandidateReachesTheVerifier(t *testing.T) {
	store := standardStore()
	store.episodes = append(store.episodes,
		episodeFixture("e-stale-source", "s-9", "漏桶算法", owner, model.EpisodeStatusActive, 6*time.Hour))
	// A derived episode whose source no longer carries the version it names.
	stale := &store.episodes[len(store.episodes)-1]
	stale.SourceEpisodes = model.EpisodeSourceRefs{{EpisodeID: "e-1", Version: 99}}

	search := &fakeSearch{exact: []string{
		"e-foreign", "e-archived", "e-current", "e-stale-source", "e-1",
	}}
	readPlan := plan(DecisionMixed, []string{"漏桶算法"}, nil, "")
	set, err := newCandidates(t, store, search, nil).Build(context.Background(), candidateRequest(readPlan))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, candidate := range set.Episodes {
		if candidate.ID != "e-1" {
			t.Fatalf("candidate %q reached the verifier and must not have", candidate.ID)
		}
	}
	if set.ReloadRejections != 4 {
		t.Fatalf("expected four reload rejections, got %d", set.ReloadRejections)
	}
	for _, candidate := range set.Semantic {
		if candidate.Memory.UserID != owner || candidate.Memory.Status != model.LongTermMemoryActive {
			t.Fatalf("semantic candidate %q is not the owner's own active record", candidate.ID)
		}
	}
}

func TestTheCandidateCapsAreEnforced(t *testing.T) {
	store := &fakeStore{}
	for index := 0; index < 6; index++ {
		store.memories = append(store.memories,
			memoryFixture(fmt.Sprintf("m-a-%d", index), "甲", model.LongTermMemoryActive, owner))
		store.memories = append(store.memories,
			memoryFixture(fmt.Sprintf("m-b-%d", index), "乙", model.LongTermMemoryActive, owner))
		store.memories = append(store.memories,
			memoryFixture(fmt.Sprintf("m-c-%d", index), "丙", model.LongTermMemoryActive, owner))
	}
	ids := []string{}
	for index := 0; index < 20; index++ {
		id := fmt.Sprintf("e-%02d", index)
		store.episodes = append(store.episodes, episodeFixture(id, "s"+id, "甲", owner,
			model.EpisodeStatusActive, time.Duration(index)*time.Hour))
		ids = append(ids, id)
	}
	readPlan := plan(DecisionMixed, []string{"甲", "乙", "丙"}, nil, "")
	set, err := newCandidates(t, store, &fakeSearch{exact: ids}, nil).Build(context.Background(), candidateRequest(readPlan))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(set.Semantic) != MaxTopicMemories {
		t.Fatalf("semantic pool is %d, the cap is %d", len(set.Semantic), MaxTopicMemories)
	}
	perTopic := map[string]int{}
	for _, candidate := range set.Semantic {
		perTopic[candidate.Memory.TopicKey]++
	}
	for topic, count := range perTopic {
		if count > MaxPerTopicMemory {
			t.Fatalf("topic %q contributed %d memories, the cap is %d", topic, count, MaxPerTopicMemory)
		}
	}
	if len(set.Episodes) != MaxEpisodes {
		t.Fatalf("episode pool is %d, the cap is %d", len(set.Episodes), MaxEpisodes)
	}
}

// A Redis or embedding outage costs the channel it belongs to and nothing else.
// It never turns into "return whatever was most recent", which is the fallback
// the design forbids by name.
func TestASearchOutageDoesNotBecomeARecentHistoryFallback(t *testing.T) {
	store := standardStore()
	search := &fakeSearch{failExact: errors.New("redis down"), failLexical: errors.New("redis down")}
	readPlan := plan(DecisionEpisodic, []string{"漏桶算法"}, nil, "关于漏桶的讨论")
	set, err := newCandidates(t, store, search, &fakeEmbedder{err: errors.New("embedding down")}).
		Build(context.Background(), candidateRequest(readPlan))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(set.Episodes) != 0 {
		t.Fatalf("a search outage produced %d episodes; it must produce none", len(set.Episodes))
	}
	if store.timeCalls != 0 {
		t.Fatal("a search outage must not fall back to a time query")
	}
	for _, wanted := range []string{"exact_channel_unavailable", "lexical_channel_unavailable", "embedding_unavailable"} {
		if !containsReason(set.Reasons, wanted) {
			t.Fatalf("reason %q was not recorded, got %v", wanted, set.Reasons)
		}
	}
}

func TestANonePlanBuildsNothingAndAsksNothing(t *testing.T) {
	store := standardStore()
	search := &fakeSearch{exact: []string{"e-1"}}
	set, err := newCandidates(t, store, search, nil).Build(context.Background(), candidateRequest(plan(DecisionNone, nil, nil, "")))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(set.Semantic) != 0 || len(set.Episodes) != 0 {
		t.Fatal("a none plan must build no candidates")
	}
	if len(store.byTopicCalls) != 0 || len(store.loadCalls) != 0 || store.timeCalls != 0 {
		t.Fatal("a none plan must not query the store at all")
	}
}

func TestEvidenceKindsFilterAgainstTheProjectionNotTheText(t *testing.T) {
	store := standardStore()
	// e-2 has a decision and no corrections; asking for corrections must drop it.
	readPlan := plan(DecisionEpisodic, nil, []string{"e-2"}, "")
	readPlan.EvidenceKinds = []EvidenceKind{EvidenceCorrection}
	set, err := newCandidates(t, store, &fakeSearch{}, nil).Build(context.Background(), candidateRequest(readPlan))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(set.Episodes) != 0 {
		t.Fatal("an episode with no correction must not satisfy a correction-only plan")
	}
	if set.ReloadRejections != 1 {
		t.Fatalf("the drop must be counted, got %d rejections", set.ReloadRejections)
	}
}

func containsReason(reasons []string, wanted string) bool {
	for _, reason := range reasons {
		if reason == wanted {
			return true
		}
	}
	return false
}

// --------------------------------------------------------------- service ----

func planReply(t *testing.T, readPlan MemoryReadPlan) reply {
	t.Helper()
	data, err := json.Marshal(readPlan)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	return reply{message: planMessage(string(data))}
}

func verdictReply(t *testing.T, entries ...CandidateVerdict) reply {
	t.Helper()
	for index := range entries {
		if entries[index].FieldRefs == nil {
			entries[index].FieldRefs = []string{}
		}
	}
	data, err := json.Marshal(VerdictSet{SchemaVersion: VerdictSchemaVersion, Verdicts: entries})
	if err != nil {
		t.Fatalf("marshal verdicts: %v", err)
	}
	return reply{message: verdictMessage(string(data))}
}

func newService(t *testing.T, chat PlannerCaller, store *fakeStore, search EpisodeSearcher) *Service {
	t.Helper()
	config := DefaultConfig()
	config.Model, config.BaseURL = "qwen-plus", "https://example.invalid/v1"
	service, err := NewService(chat, newCatalogue(t, store), newCandidates(t, store, search, nil), config)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	return service
}

func request() Request {
	return Request{UserID: owner, SessionID: currentSession, Question: "漏桶我拿什么打的比方？", Now: clock}
}

func TestAWholeTurnInjectsOnlyWhatTheVerifierSupported(t *testing.T) {
	store := standardStore()
	chat := &fakeChat{replies: []reply{
		planReply(t, plan(DecisionSemantic, []string{"漏桶算法"}, nil, "")),
		verdictReply(t,
			CandidateVerdict{CandidateID: "m-1", Relation: RelationAnswer, FieldRefs: []string{"content"}},
			CandidateVerdict{CandidateID: "m-2", Relation: RelationRelated},
		),
	}}
	result, err := newService(t, chat, store, &fakeSearch{}).Recall(context.Background(), request())
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(result.Topic) != 1 || !strings.Contains(result.Topic[0], "m-1") {
		t.Fatalf("expected only the supported memory, got %v", result.Topic)
	}
	if result.Diagnostics.RelatedOnly != 1 || result.Diagnostics.AnswerEvidence != 1 {
		t.Fatalf("relations were not counted: %+v", result.Diagnostics)
	}
	if result.Diagnostics.PlannerCalls != 1 || result.Diagnostics.VerifierCalls != 1 {
		t.Fatal("one successful turn is one planner call and one verifier call")
	}
	if result.Diagnostics.Stage != StageComplete || result.Diagnostics.FailedClosed() {
		t.Fatalf("a completed turn must not be recorded as degraded: %+v", result.Diagnostics)
	}
}

// A protocol failure is worth exactly one retry, and the retry costs a call.
func TestOneProtocolRetryCostsTwoCalls(t *testing.T) {
	store := standardStore()
	chat := &fakeChat{replies: []reply{
		{message: planMessage(`{"decision":"semantic"`)},
		planReply(t, plan(DecisionSemantic, []string{"漏桶算法"}, nil, "")),
		verdictReply(t,
			CandidateVerdict{CandidateID: "m-1", Relation: RelationRelated},
			CandidateVerdict{CandidateID: "m-2", Relation: RelationRelated},
		),
	}}
	result, err := newService(t, chat, store, &fakeSearch{}).Recall(context.Background(), request())
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if result.Diagnostics.PlannerCalls != 2 {
		t.Fatalf("a retried planner stage costs two calls, got %d", result.Diagnostics.PlannerCalls)
	}
	if len(result.Topic) != 0 {
		t.Fatal("nothing was supported, so nothing may be injected")
	}
}

// A second protocol failure ends the stage. There is no third attempt: spending
// another call to hope for a different sample of the same instruction is not a
// retry policy.
func TestTheStageStopsAfterTheSecondFailure(t *testing.T) {
	store := standardStore()
	chat := &fakeChat{replies: []reply{
		{message: planMessage(`{`)}, {message: planMessage(`{`)},
		planReply(t, plan(DecisionSemantic, []string{"漏桶算法"}, nil, "")),
	}}
	result, err := newService(t, chat, store, &fakeSearch{}).Recall(context.Background(), request())
	if err != nil {
		t.Fatalf("a failed stage must fail closed, not error: %v", err)
	}
	if chat.calls() != 2 {
		t.Fatalf("the planner made %d calls, the cap is 2", chat.calls())
	}
	if result.Diagnostics.Stage != StagePlanner || !result.Diagnostics.FailedClosed() {
		t.Fatalf("the failure must be attributed to the planner: %+v", result.Diagnostics)
	}
	if len(result.Topic) != 0 || len(result.Episodes) != 0 {
		t.Fatal("a failed planner injects nothing")
	}
}

// A plan the server refuses is a protocol failure, not a smaller plan. An id the
// catalogue never carried must not become a lookup.
func TestAPlanNamingSomethingUnknownIsRefusedWholesale(t *testing.T) {
	store := standardStore()
	invented := plan(DecisionSemantic, []string{"从未出现过的主题"}, nil, "")
	chat := &fakeChat{replies: []reply{planReply(t, invented), planReply(t, invented)}}
	result, err := newService(t, chat, store, &fakeSearch{}).Recall(context.Background(), request())
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	// The catalogue legitimately reads the topics it lists; what must never
	// happen is a lookup of the key the model made up.
	for _, key := range store.byTopicCalls {
		if key == "从未出现过的主题" {
			t.Fatalf("an invented topic key was looked up: %v", store.byTopicCalls)
		}
	}
	if result.Diagnostics.Stage != StagePlanner {
		t.Fatalf("expected a planner-stage failure, got %s", result.Diagnostics.Stage)
	}
}

func TestAVerifierFailureInjectsNothingAtAll(t *testing.T) {
	store := standardStore()
	chat := &fakeChat{replies: []reply{
		planReply(t, plan(DecisionSemantic, []string{"漏桶算法"}, nil, "")),
		{message: verdictMessage(`{"schema_version":"memory.verdict.v1","verdicts":[]}`)},
		{message: verdictMessage(`{"schema_version":"memory.verdict.v1","verdicts":[]}`)},
	}}
	result, err := newService(t, chat, store, &fakeSearch{}).Recall(context.Background(), request())
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(result.Topic) != 0 || len(result.Episodes) != 0 {
		t.Fatal("a verifier failure must inject nothing")
	}
	if result.Diagnostics.Stage != StageVerifier || !result.Diagnostics.FailedClosed() {
		t.Fatalf("the failure must be attributed to the verifier: %+v", result.Diagnostics)
	}
	if result.Diagnostics.VerifierCalls != 2 {
		t.Fatalf("the verifier made %d calls, expected 2", result.Diagnostics.VerifierCalls)
	}
}

// "The question needs nothing" and "a stage broke" are different outcomes and
// are counted apart. Adding them together would let an outage read as judgement.
func TestANoneDecisionIsNotADegradation(t *testing.T) {
	store := standardStore()
	chat := &fakeChat{replies: []reply{planReply(t, plan(DecisionNone, nil, nil, ""))}}
	result, err := newService(t, chat, store, &fakeSearch{}).Recall(context.Background(), request())
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if result.Diagnostics.FailedClosed() {
		t.Fatal("a none decision is the system working, not a degradation")
	}
	if result.Diagnostics.Reason != "plan_none" || result.Diagnostics.VerifierCalls != 0 {
		t.Fatalf("a none decision must not reach the verifier: %+v", result.Diagnostics)
	}
}

// The verifier is never asked about a question the planner already answered with
// nothing, and it is never asked twice about one pool.
func TestOnePoolIsJudgedOnce(t *testing.T) {
	store := standardStore()
	chat := &fakeChat{replies: []reply{
		planReply(t, plan(DecisionSemantic, []string{"漏桶算法"}, nil, "")),
		verdictReply(t,
			CandidateVerdict{CandidateID: "m-1", Relation: RelationAnswer, FieldRefs: []string{"content"}},
			CandidateVerdict{CandidateID: "m-2", Relation: RelationAnswer, FieldRefs: []string{"content"}},
		),
	}}
	if _, err := newService(t, chat, store, &fakeSearch{}).Recall(context.Background(), request()); err != nil {
		t.Fatalf("recall: %v", err)
	}
	if chat.calls() != 2 {
		t.Fatalf("one turn is two calls, got %d", chat.calls())
	}
}

// The catalogue and the candidates are two different views and the model must
// never see a body in the first one. This checks the payloads that actually left
// the process.
func TestBodiesReachTheVerifierAndNeverThePlanner(t *testing.T) {
	store := standardStore()
	chat := &fakeChat{replies: []reply{
		planReply(t, plan(DecisionSemantic, []string{"漏桶算法"}, nil, "")),
		verdictReply(t,
			CandidateVerdict{CandidateID: "m-1", Relation: RelationRelated},
			CandidateVerdict{CandidateID: "m-2", Relation: RelationRelated},
		),
	}}
	if _, err := newService(t, chat, store, &fakeSearch{}).Recall(context.Background(), request()); err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(chat.payloads) != 2 {
		t.Fatalf("expected two payloads, got %d", len(chat.payloads))
	}
	if strings.Contains(chat.payloads[0], "关于 漏桶算法 的理解") {
		t.Fatal("the planner payload carried a memory body")
	}
	if !strings.Contains(chat.payloads[1], "关于 漏桶算法 的理解") {
		t.Fatal("the verifier payload must carry the bodies it is judging")
	}
	// Provenance never reaches the model, so a fabricated id cannot be copied.
	for _, forbidden := range []string{"s-1", "s-2", currentSession, "evidence_message_ids", "version"} {
		if strings.Contains(chat.payloads[1], forbidden) {
			t.Fatalf("the verifier payload leaked %q", forbidden)
		}
	}
}

func TestTheConcurrencyPermitIsHonoured(t *testing.T) {
	store := standardStore()
	config := DefaultConfig()
	config.Model, config.BaseURL, config.MaxConcurrency = "qwen-plus", "https://example.invalid/v1", 2
	config.PlannerTimeout, config.VerifierTimeout = 5*time.Second, 5*time.Second
	replies := make([]reply, 0, 40)
	for index := 0; index < 20; index++ {
		replies = append(replies,
			planReply(t, plan(DecisionSemantic, []string{"漏桶算法"}, nil, "")),
			verdictReply(t,
				CandidateVerdict{CandidateID: "m-1", Relation: RelationRelated},
				CandidateVerdict{CandidateID: "m-2", Relation: RelationRelated},
			))
	}
	chat := &fakeChat{replies: replies, delay: 20 * time.Millisecond}
	service, err := NewService(chat, newCatalogue(t, store), newCandidates(t, store, &fakeSearch{}, nil), config)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	var group sync.WaitGroup
	for index := 0; index < 10; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			_, _ = service.Recall(context.Background(), request())
		}()
	}
	group.Wait()
	chat.mu.Lock()
	peak := chat.peak
	chat.mu.Unlock()
	if peak > config.MaxConcurrency {
		t.Fatalf("%d calls were in flight at once, the permit is %d", peak, config.MaxConcurrency)
	}
}

// A cancelled turn cancels the stage, and nothing is left running behind it.
func TestACancelledTurnLeavesNothingRunning(t *testing.T) {
	before := runtime.NumGoroutine()
	store := standardStore()
	chat := &fakeChat{replies: []reply{
		planReply(t, plan(DecisionSemantic, []string{"漏桶算法"}, nil, "")),
	}, delay: 2 * time.Second}
	service := newService(t, chat, store, &fakeSearch{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		result, err := service.Recall(ctx, request())
		if err != nil {
			return
		}
		if len(result.Topic) != 0 {
			t.Errorf("a cancelled turn injected %d memories", len(result.Topic))
		}
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the cancelled turn did not return")
	}
	time.Sleep(50 * time.Millisecond)
	if leaked := runtime.NumGoroutine() - before; leaked > 2 {
		t.Fatalf("%d goroutines outlived the cancelled turn", leaked)
	}
}

func TestAStageTimeoutFailsClosedWithoutFallingBack(t *testing.T) {
	store := standardStore()
	config := DefaultConfig()
	config.Model, config.BaseURL = "qwen-plus", "https://example.invalid/v1"
	config.PlannerTimeout, config.VerifierTimeout = 250*time.Millisecond, 250*time.Millisecond
	chat := &fakeChat{replies: []reply{
		planReply(t, plan(DecisionSemantic, []string{"漏桶算法"}, nil, "")),
		planReply(t, plan(DecisionSemantic, []string{"漏桶算法"}, nil, "")),
	}, delay: time.Second}
	service, err := NewService(chat, newCatalogue(t, store), newCandidates(t, store, &fakeSearch{}, nil), config)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	result, err := service.Recall(context.Background(), request())
	if err != nil {
		t.Fatalf("a timeout must fail closed, not error: %v", err)
	}
	if len(result.Topic) != 0 || len(result.Episodes) != 0 {
		t.Fatal("a timed-out planner must inject nothing")
	}
	if result.Diagnostics.Reason != "planner_timeout" {
		t.Fatalf("expected planner_timeout, got %q", result.Diagnostics.Reason)
	}
}

func TestAnEmptyCatalogueSkipsBothCalls(t *testing.T) {
	chat := &fakeChat{}
	result, err := newService(t, chat, &fakeStore{}, &fakeSearch{}).Recall(context.Background(), request())
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if chat.calls() != 0 {
		t.Fatalf("a learner with no memories cost %d provider calls", chat.calls())
	}
	if result.Diagnostics.Reason != "catalogue_empty" {
		t.Fatalf("expected catalogue_empty, got %q", result.Diagnostics.Reason)
	}
}

// The rendered topic line keeps the legacy shape and adds the relation tag, so a
// personalisation note cannot be read back to the learner as a technical fact.
func TestTheInjectedLineNamesItsRelation(t *testing.T) {
	memory := memoryFixture("m-1", "漏桶算法", model.LongTermMemoryActive, owner)
	line := renderTopicMemory(memory, RelationPersonalization)
	if !strings.Contains(line, "personalisation only") {
		t.Fatalf("a personalisation line must say so: %q", line)
	}
	if !strings.HasPrefix(line, "[topic;") {
		t.Fatalf("the legacy line shape must be kept: %q", line)
	}
}

func TestConfigRefusesAnIncompleteIdentity(t *testing.T) {
	base := DefaultConfig()
	base.Model, base.BaseURL = "qwen-plus", "https://example.invalid/v1"
	if err := base.Validate(); err != nil {
		t.Fatalf("a complete config must validate: %v", err)
	}
	for name, mutate := range map[string]func(*Config){
		"no model":       func(c *Config) { c.Model = "" },
		"no base url":    func(c *Config) { c.BaseURL = "" },
		"no timeout":     func(c *Config) { c.PlannerTimeout = 0 },
		"no verifier":    func(c *Config) { c.VerifierTimeout = 0 },
		"three attempts": func(c *Config) { c.MaxAttempts = 3 },
		"no concurrency": func(c *Config) { c.MaxConcurrency = 0 },
		"bad confidence": func(c *Config) { c.MinConfidence = 1.5 },
		"zero depth":     func(c *Config) { c.CandidateDepth = 0 },
	} {
		broken := base
		mutate(&broken)
		if err := broken.Validate(); err == nil {
			t.Fatalf("%s must be refused", name)
		}
	}
}

// The verdict schema asks for one entry per candidate, so a fixed output ceiling
// is a self-inflicted protocol failure on exactly the largest pools. The v1.0
// development run lost thirteen of sixty cases that way, every one of them at a
// pool of eleven to fourteen candidates while smaller pools passed.
func TestTheVerdictTokenBudgetGrowsWithThePool(t *testing.T) {
	small, large := verdictTokenBudget(1), verdictTokenBudget(MaxEpisodes+MaxTopicMemories)
	if small < 600 {
		t.Fatalf("a one-candidate pool gets %d tokens, below the floor", small)
	}
	if large <= small {
		t.Fatal("a twenty-candidate pool must be allowed more output than a one-candidate pool")
	}
	// The worst case this path can produce is the two caps together. A UUID id
	// plus a relation plus a field reference does not fit in forty tokens, so
	// the budget has to be well above that per candidate.
	if perCandidate := (large - 600) / (MaxEpisodes + MaxTopicMemories); perCandidate < 60 {
		t.Fatalf("%d tokens per candidate is not enough for a UUID, a relation and a field ref", perCandidate)
	}
}
