package memory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	memorydao "GopherAI/dao/memory"
	"GopherAI/model"
)

// The finalize extraction is validated against deterministic stand-ins, not
// against a model: the proposer seam returns scripted proposals, the relation
// seam returns scripted judgements, and every store is an in-memory fake. No
// test here pays for a model call or needs a database.

var finalizeClock = time.Date(2026, 11, 22, 12, 0, 0, 0, time.FixedZone("CST", 8*3600))

func finalizeAt(minute int) time.Time {
	base := time.Date(2026, 11, 18, 20, 0, 0, 0, time.FixedZone("CST", 8*3600))
	return base.Add(time.Duration(minute) * time.Minute)
}

// ext01Messages is the P3D-EXT-01 conversation: a one-turn pace request (m01),
// the lasting teaching preference in the learner's own words (m03), a one-turn
// scope limit (m05), an assistant proposal the learner deferred (m08+m09), and
// the closing line (m11).
func ext01Messages() []SessionMessage {
	message := func(id uint, isUser bool, minute int, text string) SessionMessage {
		return SessionMessage{ID: id, IsUser: isUser, Content: text, CreatedAt: finalizeAt(minute)}
	}
	return []SessionMessage{
		message(1, true, 1, "今天继续学 channel。我今晚时间不多，想快点把基础过一遍。"),
		message(2, false, 2, "好，那按最小必要内容走。"),
		message(3, true, 6, "以后你给我讲新的概念，都先举一个我熟悉的例子再讲定义，比如讲缓冲就用排队打比方。"),
		message(4, false, 7, "明白。无缓冲 channel 就像当面交接文件……"),
		message(5, true, 18, "这节就先用排队这个例子讲，别再展开别的类比了。"),
		message(6, false, 19, "好，只保留排队这一个类比。"),
		message(7, true, 36, "我老是搞混 select 里 default 的作用，给我出两道题练练。"),
		message(8, false, 37, "出了两道题。要不要我把「讲新概念先举例再讲定义」存进你的长期偏好？"),
		message(9, true, 38, "先做题，记的事一会儿再说。"),
		message(10, false, 39, "（两道 select 题与逐题讲解）"),
		message(11, true, 54, "做完了，今天就到这。"),
	}
}

// ext01BoardWithPreference is the board of a session where the whiteboard
// engine kept the preference entry, citing the learner's message m03.
func ext01BoardWithPreference(t *testing.T) string {
	t.Helper()
	document := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV3,
		Constraints: []model.SessionWhiteboardEntry{{
			Key: "teaching-preference", Text: "讲新概念先举例再讲定义", Value: "先举例再讲定义",
			SourceMessageIDs: []uint{3}, Origin: model.SessionWhiteboardOriginUser, UpdatedAtMessageID: 3,
		}},
	}
	return marshalFinalizeBoard(t, document)
}

// ext01BoardWithoutPreference is the same session's board when the engine
// missed the preference: it holds an unrelated entry, and m03 is cited by
// nothing. The learner's message is still in the session's messages.
func ext01BoardWithoutPreference(t *testing.T) string {
	t.Helper()
	document := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV3,
		Plan: []model.SessionWhiteboardEntry{{
			Key: "practice-select", Text: "练两道 select 的题", State: "open",
			SourceMessageIDs: []uint{7}, Origin: model.SessionWhiteboardOriginUser, UpdatedAtMessageID: 7,
		}},
	}
	return marshalFinalizeBoard(t, document)
}

// ext01BoardWithAdoption is a board where the engine kept an accepted
// proposal: the assistant's message m08 and the learner's m09 after it, next
// to the learner's own preference entry. It is the board state the refusal
// tests need, because the assistant's message is visible only through the
// board.
func ext01BoardWithAdoption(t *testing.T) string {
	t.Helper()
	document := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV3,
		Constraints: []model.SessionWhiteboardEntry{
			{
				Key: "teaching-preference", Text: "讲新概念先举例再讲定义", Value: "先举例再讲定义",
				SourceMessageIDs: []uint{3}, Origin: model.SessionWhiteboardOriginUser, UpdatedAtMessageID: 3,
			},
			{
				Key: "example-first-accepted", Text: "讲新概念先举例再讲定义（采纳）", Value: "先举例再讲定义",
				SourceMessageIDs: []uint{8, 9}, Origin: model.SessionWhiteboardOriginAssistantAccepted, UpdatedAtMessageID: 9,
			},
		},
	}
	return marshalFinalizeBoard(t, document)
}

func marshalFinalizeBoard(t *testing.T, document model.SessionWhiteboardDocument) string {
	t.Helper()
	encoded, err := model.MarshalSessionWhiteboardDocument(document)
	if err != nil {
		t.Fatalf("marshal board: %v", err)
	}
	return encoded
}

type fakeFinalizeBoards struct{ content *string }

func (f fakeFinalizeBoards) GetWhiteboard(ctx context.Context, userID int64, sessionID string) (*model.SessionWhiteboard, error) {
	if f.content == nil {
		return nil, model.ErrWhiteboardNotFound
	}
	return &model.SessionWhiteboard{UserID: userID, SessionID: sessionID, Content: *f.content, Version: 1}, nil
}

type fakeFinalizeMessages struct{ messages []SessionMessage }

func (f fakeFinalizeMessages) LoadSessionMessages(ctx context.Context, userID int64, sessionID string) ([]SessionMessage, error) {
	return f.messages, nil
}

type fakeFinalizeCatalogue struct{ topics []Topic }

func (f fakeFinalizeCatalogue) ListTopics(ctx context.Context, userID int64) ([]Topic, error) {
	return f.topics, nil
}

type fakeFinalizeFingerprints struct{ known map[string]struct{} }

func (f *fakeFinalizeFingerprints) KnownCandidateFingerprints(ctx context.Context, userID int64, sessionID string) (map[string]struct{}, error) {
	return f.known, nil
}

type fakeFinalizeGlobalMind struct{ entries []model.LongTermMemory }

func (f fakeFinalizeGlobalMind) ListAllActive(ctx context.Context, userID int64, scope model.MemoryScope, minScore float64, limit int) ([]model.LongTermMemory, error) {
	return f.entries, nil
}

func (f fakeFinalizeGlobalMind) GetActiveByID(ctx context.Context, userID int64, memoryID string) (*model.LongTermMemory, error) {
	return nil, memorydao.ErrMemoryNotFound
}

type fakeFinalizeTopicMind struct {
	byTopic map[string][]model.LongTermMemory
}

func (f fakeFinalizeTopicMind) ListActiveTopicEntries(ctx context.Context, userID int64, topicKey string, limit int) ([]model.LongTermMemory, error) {
	return f.byTopic[topicKey], nil
}

// scriptedProposer records what it was handed, so a test can assert what the
// extraction request could actually see.
type scriptedProposer struct {
	calls     int
	lastInput *SessionExtractionInput
	proposals []SessionRawProposal
	err       error
}

func (p *scriptedProposer) Propose(ctx context.Context, input *SessionExtractionInput) ([]SessionRawProposal, error) {
	p.calls++
	p.lastInput = input
	if p.err != nil {
		return nil, p.err
	}
	return p.proposals, nil
}

type scriptedRelations struct {
	calls  int
	result GlobalRelationResult
	err    error
}

func (r *scriptedRelations) Classify(ctx context.Context, incoming ExplicitMemoryInput, existing []model.LongTermMemory) (GlobalRelationResult, error) {
	r.calls++
	if r.err != nil {
		return GlobalRelationResult{}, r.err
	}
	return r.result, nil
}

func newFinalizeService(t *testing.T, boards FinalizeBoardSource, messages FinalizeMessageSource,
	catalogue FinalizeTopicCatalogue, known FinalizeFingerprintSource,
	globalMind GlobalMemorySnapshotSource, topicMind FinalizeTopicEntrySource,
	proposer SessionCandidateProposer, relations GlobalRelationResolver, window int) *SessionFinalizeService {
	t.Helper()
	service, err := NewSessionFinalizeService(boards, messages, catalogue, known, globalMind, topicMind, proposer, relations)
	if err != nil {
		t.Fatalf("new finalize service: %v", err)
	}
	service.now = func() time.Time { return finalizeClock }
	service.RecentMessageWindow = window
	service.RelationTargets = 10
	return service
}

func preferenceProposal(evidence []uint, assistant []uint) SessionRawProposal {
	scope, class := model.MemoryScopeGlobal, FinalizeClassTeachingPreference
	return SessionRawProposal{
		Scope: scope, Class: class, Subject: "讲解偏好", Durability: model.MemoryDurabilityStable,
		Content:     "讲新概念时先举一个我熟悉的例子，再讲正式定义，例如讲缓冲就用排队打比方。",
		EvidenceIDs: evidence, AssistantIDs: assistant,
	}
}

// TestFinalizeExtractLongTermPreference is P3D-EXT-01: the preference in the
// learner's own words proposes with its source message and its 原话时间, and
// the draft input it maps onto keeps the shape the draft entry enforces.
func TestFinalizeExtractLongTermPreference(t *testing.T) {
	board := ext01BoardWithPreference(t)
	proposer := &scriptedProposer{proposals: []SessionRawProposal{preferenceProposal([]uint{3}, nil)}}
	relations := &scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "different matter than P-02"}}
	global := fakeFinalizeGlobalMind{entries: []model.LongTermMemory{globalEntry("P-02", "出题后先等用户自己说完思路，再对答案")}}
	known := &fakeFinalizeFingerprints{known: map[string]struct{}{}}
	service := newFinalizeService(t, fakeFinalizeBoards{content: &board}, fakeFinalizeMessages{messages: ext01Messages()},
		nil, known, global, nil, proposer, relations, 0)

	result, err := service.Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-dev-EXT-01"}})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(result.Proposals) != 1 {
		t.Fatalf("proposals=%d, want 1: %+v", len(result.Proposals), result.Proposals)
	}
	candidate := result.Proposals[0]
	if candidate.Scope != model.MemoryScopeGlobal || candidate.Class != FinalizeClassTeachingPreference {
		t.Fatalf("scope=%s class=%s", candidate.Scope, candidate.Class)
	}
	if len(candidate.Evidence) != 1 || candidate.Evidence[0].MessageID != 3 {
		t.Fatalf("evidence=%+v, want m03 only", candidate.Evidence)
	}
	if !candidate.StatementTime.Equal(finalizeAt(6)) {
		t.Fatalf("statement time=%v, want the message's own time %v", candidate.StatementTime, finalizeAt(6))
	}
	if len(candidate.AssistantUtterances) != 0 {
		t.Fatalf("assistant utterances=%+v, want none", candidate.AssistantUtterances)
	}
	if candidate.Relation.Relation != GlobalRelationIndependent || len(candidate.RelatedOld) != 1 {
		t.Fatalf("relation=%+v related=%+v", candidate.Relation, candidate.RelatedOld)
	}
	if candidate.Cursor.CoveredMessageID != 11 || result.Cursor.CoveredMessageID != 11 {
		t.Fatalf("cursor=%+v result=%+v, want coverage advanced to m11", candidate.Cursor, result.Cursor)
	}
	input := ExplicitMemoryInput(candidate.DraftInput())
	if input.IdempotencyKey != "finalize:"+candidate.Fingerprint || input.IdempotencyKey == "" {
		t.Fatalf("idempotency key=%q fingerprint=%q", input.IdempotencyKey, candidate.Fingerprint)
	}
	if input.Scope != model.MemoryScopeGlobal || input.TopicKey != "" {
		t.Fatalf("draft input scope=%s topic=%q", input.Scope, input.TopicKey)
	}
	request := candidate.DraftRequest()
	if request.SessionID != "S-dev-EXT-01" || request.RunID != FinalizeRunID(FinalizeCursor{SessionID: "S-dev-EXT-01", CoveredMessageID: 11}) {
		t.Fatalf("draft request=%+v", request)
	}
	if len(FinalizeRunID(FinalizeCursor{SessionID: "S-dev-EXT-01", CoveredMessageID: 11})) > 36 {
		t.Fatalf("finalize run id exceeds the column limit")
	}
	// The evidence record the confirmation page expands carries the message
	// id and the time of the words.
	encoded, err := candidate.EvidenceJSON()
	if err != nil || encoded == "" {
		t.Fatalf("evidence json=%q err=%v", encoded, err)
	}
}

func globalEntry(id, content string) model.LongTermMemory {
	return model.LongTermMemory{
		ID: id, UserID: 7, Scope: model.MemoryScopeGlobal, Subject: "教学偏好", Content: content,
		ContentHash: model.MemoryContentHash(content), StructuredJSON: `{}`, Tags: model.StringList{"preference"},
		Durability: model.MemoryDurabilityStable, SourceRefs: model.StringList{"user_api:seed"}, Confidence: 1,
		Importance: 8, Status: model.LongTermMemoryActive, UserConfirmed: true,
	}
}

// TestFinalizeExtractTopicUnderstandingPrefill is P3D-EXT-02: the topic note
// resolves onto the learner's catalogue, and the evidence is the learner's own
// restatement, never the assistant's explanation.
func TestFinalizeExtractTopicUnderstandingPrefill(t *testing.T) {
	messages := []SessionMessage{
		{ID: 1, IsUser: true, Content: "b := a[1:3]，改 b[0] 为什么 a 也变了？", CreatedAt: finalizeAt(31)},
		{ID: 2, IsUser: false, Content: "（讲解切片共享底层数组）", CreatedAt: finalizeAt(32)},
		{ID: 3, IsUser: true, Content: "哦——append 没扩容的时候改的还是同一块底层数组，扩容之后才是新数组。", CreatedAt: finalizeAt(40)},
		{ID: 4, IsUser: false, Content: "建议把「切片共享底层数组 vs 拷贝」记到主题记忆里。", CreatedAt: finalizeAt(41)},
		{ID: 5, IsUser: true, Content: "行，记吧。", CreatedAt: finalizeAt(42)},
		{ID: 8, IsUser: true, Content: "这条先记最基础的版本：我容易混淆切片共享底层数组和拷贝，别加引用类型那一堆例外。", CreatedAt: finalizeAt(58)},
	}
	proposer := &scriptedProposer{proposals: []SessionRawProposal{{
		Scope: model.MemoryScopeTopic, Kind: FinalizeKindUnderstanding, TopicKey: "go-basics",
		Subject: "切片易错点", Durability: model.MemoryDurabilityStable,
		Content:     "我容易混淆切片共享底层数组和拷贝：append 未扩容时改的是共享底层数组，扩容后才指向新数组。",
		EvidenceIDs: []uint{8, 3},
	}}}
	relations := &scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "different matter than TM-GB-01"}}
	topics := fakeFinalizeCatalogue{topics: []Topic{
		{Key: "go-basics", Name: "Go 基础"}, {Key: "gorm", Name: "GORM"}, {Key: "redis-basic", Name: "Redis 基础"},
	}}
	topicMind := fakeFinalizeTopicMind{byTopic: map[string][]model.LongTermMemory{
		"go-basics": {topicEntry("TM-GB-01", "go-basics", "for range 遍历切片时循环变量是被反复赋值的同一个副本；Go 1.22 起每次迭代是新变量")},
	}}
	known := &fakeFinalizeFingerprints{known: map[string]struct{}{}}
	service := newFinalizeService(t, fakeFinalizeBoards{}, fakeFinalizeMessages{messages: messages},
		topics, known, fakeFinalizeGlobalMind{}, topicMind, proposer, relations, 50)

	result, err := service.Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-dev-EXT-02"}})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(result.Proposals) != 1 {
		t.Fatalf("proposals=%d want 1", len(result.Proposals))
	}
	candidate := result.Proposals[0]
	if candidate.TopicKey != "go-basics" || candidate.TopicPending {
		t.Fatalf("topic=%q pending=%v, want unique prefill go-basics", candidate.TopicKey, candidate.TopicPending)
	}
	if candidate.Kind != FinalizeKindUnderstanding {
		t.Fatalf("kind=%s", candidate.Kind)
	}
	if len(candidate.Evidence) != 2 || candidate.Evidence[0].MessageID != 3 || candidate.Evidence[1].MessageID != 8 {
		t.Fatalf("evidence=%+v, want m03+m08", candidate.Evidence)
	}
	if candidate.StatementTime.Equal(finalizeClock) {
		t.Fatalf("statement time must be the message time, not the extraction time")
	}
}

func topicEntry(id, topicKey, content string) model.LongTermMemory {
	return model.LongTermMemory{
		ID: id, UserID: 7, Scope: model.MemoryScopeTopic, TopicKey: topicKey, Subject: "我的理解", Content: content,
		ContentHash: model.MemoryContentHash(content), StructuredJSON: `{}`, Tags: model.StringList{"learning"},
		Durability: model.MemoryDurabilityStable, SourceRefs: model.StringList{"user_api:seed"}, Confidence: 1,
		Importance: 8, Status: model.LongTermMemoryActive, UserConfirmed: true,
	}
}

// TestFinalizeTemporaryRequestsAndAssistantSuggestionsStayOut covers the
// planted distractors of EXT-01 and EXT-04: the model plays its judgement
// correctly and proposes nothing for the one-turn requests, and when a
// misbehaving model does propose the deferred assistant suggestion, the
// program refuses it - an assistant's words are not the learner's evidence.
func TestFinalizeTemporaryRequestsAndAssistantSuggestionsStayOut(t *testing.T) {
	board := ext01BoardWithPreference(t)
	// The correct judgement: m01/m05/m11 are one-turn or closing words, and
	// the m08 suggestion was only deferred ("一会儿再说"), so nothing of them
	// is proposed.
	proposer := &scriptedProposer{proposals: []SessionRawProposal{preferenceProposal([]uint{3}, nil)}}
	relations := &scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "new"}}
	known := &fakeFinalizeFingerprints{known: map[string]struct{}{}}
	service := newFinalizeService(t, fakeFinalizeBoards{content: &board}, fakeFinalizeMessages{messages: ext01Messages()},
		nil, known, fakeFinalizeGlobalMind{}, nil, proposer, relations, 0)
	result, err := service.Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-dev-EXT-01"}})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(result.Proposals) != 1 || len(result.Skipped) != 0 {
		t.Fatalf("proposals=%d skipped=%+v, want exactly the preference candidate", len(result.Proposals), result.Skipped)
	}

	// The misbehaving model proposes the assistant's words as the learner's
	// evidence, credits a proposal the learner never accepted after it, and
	// cites a message that does not exist. The board holds an accepted
	// proposal, so the assistant's message m08 and the learner's m09 are
	// visible - the refusals below are role and acceptance refusals, not
	// visibility ones. The evidence role check refuses the first: only the
	// learner's own words can be the evidence of a fact about the learner.
	adoptionBoard := ext01BoardWithAdoption(t)
	assistantOnly := SessionRawProposal{
		Scope: model.MemoryScopeGlobal, Class: FinalizeClassTeachingPreference, Subject: "讲解偏好",
		Durability: model.MemoryDurabilityStable,
		Content:    "讲新概念先举例再讲定义。", EvidenceIDs: []uint{8},
	}
	unaccepted := SessionRawProposal{
		Scope: model.MemoryScopeGlobal, Class: FinalizeClassTeachingPreference, Subject: "讲解偏好",
		Durability: model.MemoryDurabilityStable,
		Content:    "讲新概念先举例再讲定义。", EvidenceIDs: []uint{3}, AssistantIDs: []uint{8},
	}
	hallucinated := SessionRawProposal{
		Scope: model.MemoryScopeGlobal, Class: FinalizeClassTeachingPreference, Subject: "讲解偏好",
		Durability: model.MemoryDurabilityStable,
		Content:    "讲新概念先举例再讲定义。", EvidenceIDs: []uint{99},
	}
	rogue := &scriptedProposer{proposals: []SessionRawProposal{assistantOnly, unaccepted, hallucinated}}
	rogueService := newFinalizeService(t, fakeFinalizeBoards{content: &adoptionBoard}, fakeFinalizeMessages{messages: ext01Messages()},
		nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{}, nil, rogue,
		&scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "x"}}, 0)
	rogueResult, err := rogueService.Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-dev-EXT-01"}})
	if err != nil {
		t.Fatalf("rogue extract: %v", err)
	}
	if len(rogueResult.Proposals) != 0 {
		t.Fatalf("rogue proposals=%d, want none: %+v", len(rogueResult.Proposals), rogueResult.Proposals)
	}
	reasons := map[string]int{}
	for _, skip := range rogueResult.Skipped {
		reasons[skip.Reason]++
	}
	if reasons[FinalizeSkipEvidenceRole] != 1 || reasons[FinalizeSkipAdoptionUnproven] != 1 || reasons[FinalizeSkipEvidenceMissing] != 1 {
		t.Fatalf("skip reasons=%+v, want role/acceptance/visibility refusals", reasons)
	}
}

// TestFinalizeAdoptedProposalRecordsBothUtterances is P3D-EXT-03: the
// assistant proposed, the learner accepted, and the candidate records the two
// utterances apart - the learner's acceptance as the evidence, the assistant's
// proposal as the adopted origin.
func TestFinalizeAdoptedProposalRecordsBothUtterances(t *testing.T) {
	messages := []SessionMessage{
		{ID: 1, IsUser: true, Content: "复习 Redis 持久化，RDB 和 AOF 的区别我老是记不住。", CreatedAt: finalizeAt(1)},
		{ID: 2, IsUser: false, Content: "要不要试试这样：每次复习开头，先用两道旧题热身，再讲新内容？", CreatedAt: finalizeAt(2)},
		{ID: 3, IsUser: true, Content: "可以，就这样。以后每次复习都先来两道旧题。", CreatedAt: finalizeAt(4)},
		{ID: 6, IsUser: false, Content: "另外我建议把「学新章节前先让我画一张知识地图」也固定下来，要记吗？", CreatedAt: finalizeAt(100)},
		{ID: 7, IsUser: true, Content: "这个先不用。", CreatedAt: finalizeAt(101)},
	}
	proposer := &scriptedProposer{proposals: []SessionRawProposal{{
		Scope: model.MemoryScopeGlobal, Class: FinalizeClassTeachingPreference, Subject: "复习偏好",
		Durability:  model.MemoryDurabilityStable,
		Content:     "每次复习开始先用两道旧题热身，之后再讲新内容。",
		EvidenceIDs: []uint{3}, AssistantIDs: []uint{2},
	}}}
	known := &fakeFinalizeFingerprints{known: map[string]struct{}{}}
	service := newFinalizeService(t, fakeFinalizeBoards{}, fakeFinalizeMessages{messages: messages},
		nil, known, fakeFinalizeGlobalMind{}, nil, proposer,
		&scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "no related entries"}}, 50)

	result, err := service.Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-dev-EXT-03"}})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(result.Proposals) != 1 {
		t.Fatalf("proposals=%d want 1: %+v", len(result.Proposals), result.Proposals)
	}
	candidate := result.Proposals[0]
	if len(candidate.Evidence) != 1 || candidate.Evidence[0].MessageID != 3 {
		t.Fatalf("evidence=%+v, want the learner's acceptance m03", candidate.Evidence)
	}
	if len(candidate.AssistantUtterances) != 1 || candidate.AssistantUtterances[0].MessageID != 2 {
		t.Fatalf("assistant utterances=%+v, want the adopted proposal m02", candidate.AssistantUtterances)
	}
	if !candidate.StatementTime.Equal(finalizeAt(4)) {
		t.Fatalf("statement time=%v, want the acceptance's time", candidate.StatementTime)
	}
}

// TestFinalizeNoCandidatesAndRepeatedTrigger is P3D-EXT-06: a session with no
// long-term content proposes nothing, the cursor still advances, and a
// repeated trigger of the same progress calls nothing and proposes nothing.
func TestFinalizeNoCandidatesAndRepeatedTrigger(t *testing.T) {
	messages := []SessionMessage{
		{ID: 1, IsUser: true, Content: "帮我查一下 SETNX 和 SET key value NX 是不是一回事。", CreatedAt: finalizeAt(1)},
		{ID: 2, IsUser: false, Content: "（回答：等价）", CreatedAt: finalizeAt(2)},
		{ID: 3, IsUser: true, Content: "把刚才的结论按我笔记的格式整理一下，就这次。", CreatedAt: finalizeAt(10)},
		{ID: 7, IsUser: true, Content: "行，今天就到这，没有要记的东西。", CreatedAt: finalizeAt(39)},
	}
	proposer := &scriptedProposer{}
	known := &fakeFinalizeFingerprints{known: map[string]struct{}{}}
	service := newFinalizeService(t, fakeFinalizeBoards{}, fakeFinalizeMessages{messages: messages},
		nil, known, fakeFinalizeGlobalMind{}, nil, proposer, nil, 50)

	first, err := service.Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-dev-EXT-06"}})
	if err != nil {
		t.Fatalf("first extract: %v", err)
	}
	if len(first.Proposals) != 0 || len(first.Skipped) != 0 {
		t.Fatalf("first run proposals=%d skipped=%+v, want none", len(first.Proposals), first.Skipped)
	}
	if first.Cursor.CoveredMessageID != 7 {
		t.Fatalf("cursor=%+v, want advanced to m07 even with no candidates", first.Cursor)
	}
	if proposer.calls != 1 {
		t.Fatalf("proposer calls=%d, want the one real extraction", proposer.calls)
	}

	repeat, err := service.Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: first.Cursor})
	if err != nil {
		t.Fatalf("repeat extract: %v", err)
	}
	if !repeat.NoNewContent || proposer.calls != 1 {
		t.Fatalf("repeat=%+v calls=%d, want no new content and no proposer call", repeat, proposer.calls)
	}
}

// TestFinalizeSameSourceRetryKeepsOneCandidate is the repeated-trigger rule:
// progress moved, the same content is proposed again, and the fingerprint
// keeps one candidate. A duplicate inside one run collapses too.
func TestFinalizeSameSourceRetryKeepsOneCandidate(t *testing.T) {
	board := ext01BoardWithPreference(t)
	known := &fakeFinalizeFingerprints{known: map[string]struct{}{}}
	relations := &scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "new"}}
	service := newFinalizeService(t, fakeFinalizeBoards{content: &board}, fakeFinalizeMessages{messages: ext01Messages()},
		nil, known, fakeFinalizeGlobalMind{}, nil,
		&scriptedProposer{proposals: []SessionRawProposal{preferenceProposal([]uint{3}, nil)}}, relations, 0)

	first, err := service.Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-dev-EXT-01"}})
	if err != nil || len(first.Proposals) != 1 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	// The handoff registered the proposal at the draft entry, so the
	// fingerprint is now known for this session.
	known.known[first.Proposals[0].Fingerprint] = struct{}{}

	// A later trigger sees one more message but proposes the same content.
	messages := append(ext01Messages(), SessionMessage{ID: 12, IsUser: true, Content: "下次接着做 select 的题。", CreatedAt: finalizeAt(60)})
	retryService := newFinalizeService(t, fakeFinalizeBoards{content: &board}, fakeFinalizeMessages{messages: messages},
		nil, known, fakeFinalizeGlobalMind{}, nil,
		&scriptedProposer{proposals: []SessionRawProposal{preferenceProposal([]uint{3}, nil)}}, relations, 0)
	second, err := retryService.Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-dev-EXT-01", CoveredMessageID: 11}})
	if err != nil {
		t.Fatalf("second extract: %v", err)
	}
	if len(second.Proposals) != 0 || len(second.Skipped) != 1 || second.Skipped[0].Reason != FinalizeSkipDuplicateKnown {
		t.Fatalf("second=%+v skipped=%+v, want the repeat suppressed as duplicate_known", second, second.Skipped)
	}
	if second.Cursor.CoveredMessageID != 12 {
		t.Fatalf("cursor=%+v, want advanced to m12", second.Cursor)
	}

	// The same content twice in one run collapses to the first.
	twice := &scriptedProposer{proposals: []SessionRawProposal{preferenceProposal([]uint{3}, nil), preferenceProposal([]uint{3}, nil)}}
	collapseService := newFinalizeService(t, fakeFinalizeBoards{content: &board}, fakeFinalizeMessages{messages: ext01Messages()},
		nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{}, nil, twice,
		&scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "new"}}, 0)
	collapsed, err := collapseService.Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-dev-EXT-01"}})
	if err != nil {
		t.Fatalf("collapse extract: %v", err)
	}
	if len(collapsed.Proposals) != 1 || len(collapsed.Skipped) != 1 || collapsed.Skipped[0].Reason != FinalizeSkipDuplicateRun {
		t.Fatalf("collapsed=%+v skipped=%+v", collapsed, collapsed.Skipped)
	}
}

// TestFinalizeDuplicateExistingSkipsWithoutModelCall: content that is already
// an active memory of the range is skipped by hash, and the relation model is
// never called for it.
func TestFinalizeDuplicateExistingSkipsWithoutModelCall(t *testing.T) {
	board := ext01BoardWithPreference(t)
	content := "讲新概念时先举一个我熟悉的例子，再讲正式定义，例如讲缓冲就用排队打比方。"
	existing := globalEntry("P-03", content)
	relations := &scriptedRelations{}
	known := &fakeFinalizeFingerprints{known: map[string]struct{}{}}
	service := newFinalizeService(t, fakeFinalizeBoards{content: &board}, fakeFinalizeMessages{messages: ext01Messages()},
		nil, known, fakeFinalizeGlobalMind{entries: []model.LongTermMemory{existing}}, nil,
		&scriptedProposer{proposals: []SessionRawProposal{preferenceProposal([]uint{3}, nil)}}, relations, 0)

	result, err := service.Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-dev-EXT-01"}})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(result.Proposals) != 0 || len(result.Skipped) != 1 || result.Skipped[0].Reason != FinalizeSkipDuplicateExisting {
		t.Fatalf("result=%+v skipped=%+v, want the exact duplicate skipped", result, result.Skipped)
	}
	if relations.calls != 0 {
		t.Fatalf("relation calls=%d, want the free path to skip the model", relations.calls)
	}
	if result.Cursor.CoveredMessageID != 11 {
		t.Fatalf("cursor=%+v, want advanced", result.Cursor)
	}
}

// TestFinalizeReplacementRelationCarriesTarget: a replacement judgement
// travels with the target entry, which is what the confirmation page shows
// the learner before any old entry is touched.
func TestFinalizeReplacementRelationCarriesTarget(t *testing.T) {
	board := ext01BoardWithPreference(t)
	old := globalEntry("P-02", "出题后先等用户自己说完思路，再对答案")
	relations := &scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationReplacement, TargetMemoryID: "P-02", Reason: "same slot, new value"}}
	known := &fakeFinalizeFingerprints{known: map[string]struct{}{}}
	service := newFinalizeService(t, fakeFinalizeBoards{content: &board}, fakeFinalizeMessages{messages: ext01Messages()},
		nil, known, fakeFinalizeGlobalMind{entries: []model.LongTermMemory{old}}, nil,
		&scriptedProposer{proposals: []SessionRawProposal{preferenceProposal([]uint{3}, nil)}}, relations, 0)

	result, err := service.Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-dev-EXT-01"}})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(result.Proposals) != 1 || result.Proposals[0].Relation.Relation != GlobalRelationReplacement {
		t.Fatalf("result=%+v", result.Proposals)
	}
	if len(result.Proposals[0].RelatedOld) != 1 || result.Proposals[0].RelatedOld[0].ID != "P-02" {
		t.Fatalf("related old=%+v, want P-02 carried for the confirmation", result.Proposals[0].RelatedOld)
	}
	// Nothing was written: the proposal is a proposal, the old entry is
	// still active in the store the test handed in.
	if old.Status != model.LongTermMemoryActive {
		t.Fatalf("old entry status=%s, want untouched", old.Status)
	}
}

// TestFinalizeRelationFailureKeepsRetryState: a failed judgement keeps the
// candidate retryable and holds the cursor, so the next trigger re-runs the
// same progress and the fingerprint keeps one candidate.
func TestFinalizeRelationFailureKeepsRetryState(t *testing.T) {
	board := ext01BoardWithPreference(t)
	known := &fakeFinalizeFingerprints{known: map[string]struct{}{}}
	failing := &scriptedRelations{err: errors.New("classifier unavailable")}
	service := newFinalizeService(t, fakeFinalizeBoards{content: &board}, fakeFinalizeMessages{messages: ext01Messages()},
		nil, known, fakeFinalizeGlobalMind{entries: []model.LongTermMemory{globalEntry("P-02", "出题后先等用户自己说完思路，再对答案")}}, nil,
		&scriptedProposer{proposals: []SessionRawProposal{preferenceProposal([]uint{3}, nil)}}, failing, 0)

	first, err := service.Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-dev-EXT-01"}})
	if err != nil {
		t.Fatalf("first extract: %v", err)
	}
	if len(first.RetryPending) != 1 || len(first.Proposals) != 0 {
		t.Fatalf("first=%+v, want the candidate held for retry", first)
	}
	if first.Cursor.CoveredMessageID != 0 {
		t.Fatalf("cursor=%+v, want held at the old progress", first.Cursor)
	}

	// The retry proposes the same content; the fingerprint keeps it to one.
	known.known[first.RetryPending[0].Fingerprint] = struct{}{}
	retryService := newFinalizeService(t, fakeFinalizeBoards{content: &board}, fakeFinalizeMessages{messages: ext01Messages()},
		nil, known, fakeFinalizeGlobalMind{entries: []model.LongTermMemory{globalEntry("P-02", "出题后先等用户自己说完思路，再对答案")}}, nil,
		&scriptedProposer{proposals: []SessionRawProposal{preferenceProposal([]uint{3}, nil)}},
		&scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "new"}}, 0)
	second, err := retryService.Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-dev-EXT-01"}})
	if err != nil {
		t.Fatalf("retry extract: %v", err)
	}
	if len(second.RetryPending) != 0 || len(second.Skipped) != 1 || second.Skipped[0].Reason != FinalizeSkipDuplicateKnown {
		t.Fatalf("retry=%+v skipped=%+v, want the repeat suppressed", second, second.Skipped)
	}
}

// TestFinalizeTopicPendingLeavesOwnershipToConfirmation: a note whose topic
// resolves to nothing still proposes, without a topic and without a range
// judgement - the confirmation flow asks the learner where it belongs.
func TestFinalizeTopicPendingLeavesOwnershipToConfirmation(t *testing.T) {
	messages := []SessionMessage{
		{ID: 1, IsUser: true, Content: "我发现我老忘记 Save 传指针这回事，零值字段根本不更新——这个坑记下来。", CreatedAt: finalizeAt(35)},
	}
	proposer := &scriptedProposer{proposals: []SessionRawProposal{{
		Scope: model.MemoryScopeTopic, Kind: FinalizeKindUnderstanding, TopicKey: "切片", Subject: "GORM 零值更新",
		Durability: model.MemoryDurabilityStable, Content: "我老忘记 Save 要传指针，值传递时零值字段不会写库。",
		EvidenceIDs: []uint{1},
	}}}
	topicMind := fakeFinalizeTopicMind{byTopic: map[string][]model.LongTermMemory{}}
	service := newFinalizeService(t, fakeFinalizeBoards{}, fakeFinalizeMessages{messages: messages},
		fakeFinalizeCatalogue{topics: []Topic{{Key: "go-basics"}, {Key: "gorm"}}},
		&fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{}, topicMind, proposer,
		&scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "should not be called"}}, 50)

	result, err := service.Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-dev-TLD-02"}})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(result.Proposals) != 1 {
		t.Fatalf("proposals=%d want 1", len(result.Proposals))
	}
	candidate := result.Proposals[0]
	if !candidate.TopicPending || candidate.TopicKey != "" {
		t.Fatalf("topic=%q pending=%v, want the ownership left to the confirmation", candidate.TopicKey, candidate.TopicPending)
	}
	if relationsCalled(t, candidate) {
		t.Fatalf("a pending-topic candidate must not be judged against a range it does not have")
	}
}

// relationsCalled reports whether the candidate was handed a relation
// judgement. A pending-topic candidate has no range, so the recorded related
// set is the witness.
func relationsCalled(t *testing.T, candidate SessionMemoryCandidate) bool {
	t.Helper()
	return candidate.Relation.Relation != "" && candidate.Relation.Reason != "no related entries in range"
}

// TestFinalizeSelfReportKeepsItsLimit is P3D-EXT-05: the self-report keeps
// the learner's own qualification (粗心), and the extraction never touches
// quiz mastery - there is no mastery writer anywhere in the module.
func TestFinalizeSelfReportKeepsItsLimit(t *testing.T) {
	messages := []SessionMessage{
		{ID: 1, IsUser: true, Content: "我觉得 select 多路复用我已经懂了，今天就别从零讲了，直接上难的题。", CreatedAt: finalizeAt(1)},
		{ID: 3, IsUser: true, Content: "（其中一题做错）这题是我粗心看错了题目条件，概念本身我是清楚的。", CreatedAt: finalizeAt(20)},
		{ID: 4, IsUser: true, Content: "我每天只有晚上九点到十点能学习，十点以后必须睡，以后安排内容和出题量都按这个来。", CreatedAt: finalizeAt(30)},
	}
	proposer := &scriptedProposer{proposals: []SessionRawProposal{
		{
			Scope: model.MemoryScopeTopic, Kind: FinalizeKindSelfReport, TopicKey: "go-concurrency", Subject: "select 自述",
			Durability:  model.MemoryDurabilityStable,
			Content:     "我自述已掌握 select 多路复用，但仍会因粗心看错题目条件出错。",
			EvidenceIDs: []uint{1, 3}, StatementEvidenceID: 1,
		},
		{
			Scope: model.MemoryScopeGlobal, Class: FinalizeClassConstraint, Subject: "学习时间",
			Durability:  model.MemoryDurabilityStable,
			Content:     "每天仅在 21:00-22:00 学习，22:00 后不安排内容，出题量也按此安排。",
			EvidenceIDs: []uint{4},
		},
	}}
	topics := fakeFinalizeCatalogue{topics: []Topic{{Key: "go-concurrency"}}}
	service := newFinalizeService(t, fakeFinalizeBoards{}, fakeFinalizeMessages{messages: messages},
		topics, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{},
		fakeFinalizeTopicMind{byTopic: map[string][]model.LongTermMemory{"go-concurrency": {topicEntry("TM-GC-01", "go-concurrency", "select 的 default 分支用于非阻塞收发")}}},
		proposer, &scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "different matters"}}, 50)

	result, err := service.Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-dev-EXT-05"}})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(result.Proposals) != 2 {
		t.Fatalf("proposals=%d want the self-report and the constraint", len(result.Proposals))
	}
	if result.Proposals[0].Kind != FinalizeKindSelfReport || result.Proposals[0].TopicKey != "go-concurrency" {
		t.Fatalf("self-report=%+v", result.Proposals[0])
	}
	if result.Proposals[1].Class != FinalizeClassConstraint {
		t.Fatalf("constraint=%+v", result.Proposals[1])
	}
	// The self-report names the message that carries the claim: the statement
	// time is m01's, while m03 stays as the qualification's context.
	if !result.Proposals[0].StatementTime.Equal(finalizeAt(1)) {
		t.Fatalf("self-report statement time=%v, want m01's time", result.Proposals[0].StatementTime)
	}
	// The mastery numbers live in the learning records and nowhere here: the
	// module's stores are the board, the messages, the catalogue, the draft
	// fingerprints and the memory ranges. There is no learning progress
	// writer to call, which is the structural guarantee the plan's S8 asks
	// for - a candidate cannot modify what it cannot reach.
}

// TestFinalizeReplayWhiteboardMissedUtterance is the plan §6.1 replay pair:
// the same expressed preference, once with the whiteboard keeping the record
// and once with the whiteboard missing it while the learner's own message is
// still there. The four runs say where each failure sits.
func TestFinalizeReplayWhiteboardMissedUtterance(t *testing.T) {
	withBoard := ext01BoardWithPreference(t)
	missedBoard := ext01BoardWithoutPreference(t)
	relations := &scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "new"}}
	type run struct {
		name             string
		board            *string
		window           int
		proposer         *scriptedProposer
		wantVisible      bool
		wantProposals    int
		wantSkip         string
		wantBoardMissing bool
	}
	honestProposal := func() *scriptedProposer {
		return &scriptedProposer{proposals: []SessionRawProposal{preferenceProposal([]uint{3}, nil)}}
	}
	// The honest model can only cite what it saw; the hallucinating one cites
	// the message regardless of visibility, which is what a model that
	// guessed would do.
	hallucinating := &scriptedProposer{proposals: []SessionRawProposal{preferenceProposal([]uint{3}, nil)}}

	runs := []run{
		{name: "A1-board-kept-window-off", board: &withBoard, window: 0, proposer: honestProposal(), wantVisible: true, wantProposals: 1},
		{name: "B1-board-missed-window-off", board: &missedBoard, window: 0, proposer: &scriptedProposer{}, wantVisible: false, wantProposals: 0},
		{name: "B2-board-missed-window-on", board: &missedBoard, window: 50, proposer: honestProposal(), wantVisible: true, wantProposals: 1},
		{name: "B3-board-missed-window-off-hallucinated", board: &missedBoard, window: 0, proposer: hallucinating, wantVisible: false, wantProposals: 0, wantSkip: FinalizeSkipEvidenceMissing},
	}
	for i := range runs {
		current := runs[i]
		t.Run(current.name, func(t *testing.T) {
			service := newFinalizeService(t, fakeFinalizeBoards{content: current.board}, fakeFinalizeMessages{messages: ext01Messages()},
				nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{}, nil,
				current.proposer, relations, current.window)
			result, err := service.Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-dev-EXT-01"}})
			if err != nil {
				t.Fatalf("extract: %v", err)
			}
			sawM03 := false
			if current.proposer.lastInput != nil {
				_, sawM03 = current.proposer.lastInput.Visible[3]
			}
			if sawM03 != current.wantVisible {
				t.Fatalf("extraction saw m03=%v, want %v", sawM03, current.wantVisible)
			}
			if len(result.Proposals) != current.wantProposals {
				t.Fatalf("proposals=%d want %d", len(result.Proposals), current.wantProposals)
			}
			if current.wantSkip != "" {
				if len(result.Skipped) != 1 || result.Skipped[0].Reason != current.wantSkip {
					t.Fatalf("skipped=%+v want %s", result.Skipped, current.wantSkip)
				}
				return
			}
			if current.wantProposals == 1 {
				if result.Proposals[0].Evidence[0].MessageID != 3 {
					t.Fatalf("candidate evidence=%+v, want traceable to m03", result.Proposals[0].Evidence)
				}
			}
			if result.BoardMissing != current.wantBoardMissing {
				t.Fatalf("board missing=%v want %v", result.BoardMissing, current.wantBoardMissing)
			}
		})
	}
}

// TestFinalizeInvalidShapesRefused: the field rules the program owns refuse a
// proposal before any source is read - an unknown class, an over-long body, a
// global proposal that names a topic, an impossible validity.
func TestFinalizeInvalidShapesRefused(t *testing.T) {
	cases := []struct {
		name     string
		proposal SessionRawProposal
		want     string
	}{
		{"unknown class", SessionRawProposal{Scope: model.MemoryScopeGlobal, Class: "心情", Subject: "s", Content: "c", Durability: model.MemoryDurabilityStable, EvidenceIDs: []uint{3}}, FinalizeSkipInvalidShape},
		{"unknown kind", SessionRawProposal{Scope: model.MemoryScopeTopic, Kind: "quiz_score", Subject: "s", Content: "c", Durability: model.MemoryDurabilityStable, TopicKey: "go-basics", EvidenceIDs: []uint{3}}, FinalizeSkipInvalidShape},
		{"over-long content", SessionRawProposal{Scope: model.MemoryScopeGlobal, Class: FinalizeClassGoal, Subject: "s", Durability: model.MemoryDurabilityStable, Content: string(make([]rune, 401)), EvidenceIDs: []uint{3}}, FinalizeSkipInvalidShape},
		{"global with topic", SessionRawProposal{Scope: model.MemoryScopeGlobal, Class: FinalizeClassGoal, Subject: "s", Content: "c", TopicKey: "go-basics", Durability: model.MemoryDurabilityStable, EvidenceIDs: []uint{3}}, FinalizeSkipInvalidShape},
		{"time bound without expiry", SessionRawProposal{Scope: model.MemoryScopeGlobal, Class: FinalizeClassGoal, Subject: "s", Content: "c", Durability: model.MemoryDurabilityTimeBound, EvidenceIDs: []uint{3}}, FinalizeSkipInvalidShape},
		{"empty evidence", SessionRawProposal{Scope: model.MemoryScopeGlobal, Class: FinalizeClassGoal, Subject: "s", Content: "c", Durability: model.MemoryDurabilityStable}, FinalizeSkipEvidenceMissing},
		{"too many evidence ids", func() SessionRawProposal {
			ids := make([]uint, 0, 9)
			for id := uint(1); id <= 9; id++ {
				ids = append(ids, id)
			}
			return SessionRawProposal{Scope: model.MemoryScopeGlobal, Class: FinalizeClassGoal, Subject: "s", Content: "c", Durability: model.MemoryDurabilityStable, EvidenceIDs: ids}
		}(), FinalizeSkipInvalidShape},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			service := newFinalizeService(t, fakeFinalizeBoards{}, fakeFinalizeMessages{messages: ext01Messages()},
				nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{}, nil,
				&scriptedProposer{proposals: []SessionRawProposal{testCase.proposal}},
				&scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "x"}}, 0)
			result, err := service.Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-dev-EXT-01"}})
			if err != nil {
				t.Fatalf("extract: %v", err)
			}
			if len(result.Proposals) != 0 || len(result.Skipped) != 1 || result.Skipped[0].Reason != testCase.want {
				t.Fatalf("result=%+v skipped=%+v want %s", result.Proposals, result.Skipped, testCase.want)
			}
		})
	}
}

// TestFinalizeProposerFailureRetriesSameProgress: a proposer failure fails
// the run, the cursor stays, and the caller retries the same progress.
func TestFinalizeProposerFailureRetriesSameProgress(t *testing.T) {
	known := &fakeFinalizeFingerprints{known: map[string]struct{}{}}
	service := newFinalizeService(t, fakeFinalizeBoards{}, fakeFinalizeMessages{messages: ext01Messages()},
		nil, known, fakeFinalizeGlobalMind{}, nil,
		&scriptedProposer{err: errors.New("model unavailable")}, nil, 0)
	result, err := service.Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-dev-EXT-01"}})
	if err == nil {
		t.Fatalf("want the proposer failure to surface, got %+v", result)
	}
	if result.Cursor.CoveredMessageID != 0 {
		t.Fatalf("cursor=%+v, want held for the retry", result.Cursor)
	}
}

// TestFinalizeWithoutBoardExtractsFromWindow: a session without a board is
// still a session the learner spoke in; extraction runs from the window alone
// and reports the missing board.
func TestFinalizeWithoutBoardExtractsFromWindow(t *testing.T) {
	service := newFinalizeService(t, fakeFinalizeBoards{}, fakeFinalizeMessages{messages: ext01Messages()},
		nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{}, nil,
		&scriptedProposer{proposals: []SessionRawProposal{preferenceProposal([]uint{3}, nil)}},
		&scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "new"}}, 50)
	result, err := service.Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-dev-EXT-01"}})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if !result.BoardMissing || len(result.Proposals) != 1 {
		t.Fatalf("board missing=%v proposals=%d", result.BoardMissing, len(result.Proposals))
	}
}

// TestFinalizeStatementTimeFollowsValueEvidence is the statement-time rule:
// the 原话时间 is the time of the message that directly supports the
// candidate's current value. A candidate that cites both the value it
// replaces and the value that replaced it must not carry the older
// utterance's time.
func TestFinalizeStatementTimeFollowsValueEvidence(t *testing.T) {
	messages := []SessionMessage{
		{ID: 3, IsUser: true, Content: "先定成每天背 22 个单词。", CreatedAt: finalizeAt(6)},
		{ID: 6, IsUser: true, Content: "后来改成 37 个，按 37 来。", CreatedAt: finalizeAt(18)},
	}
	valueProposal := func(statement uint) SessionRawProposal {
		return SessionRawProposal{
			Scope: model.MemoryScopeGlobal, Class: FinalizeClassGoal, Subject: "背诵目标",
			Durability: model.MemoryDurabilityStable,
			Content:    "每天背 37 个单词。", EvidenceIDs: []uint{3, 6}, StatementEvidenceID: statement,
		}
	}
	request := FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-goal"}}

	// The proposer names the message that carries the value: the statement
	// time is that message's time, and the older citation stays context.
	named := &scriptedProposer{proposals: []SessionRawProposal{valueProposal(6)}}
	service := newFinalizeService(t, fakeFinalizeBoards{}, fakeFinalizeMessages{messages: messages},
		nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{}, nil, named,
		&scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "new"}}, 50)
	result, err := service.Extract(context.Background(), request)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(result.Proposals) != 1 {
		t.Fatalf("proposals=%d want 1", len(result.Proposals))
	}
	candidate := result.Proposals[0]
	if !candidate.StatementTime.Equal(finalizeAt(18)) {
		t.Fatalf("statement time=%v, want the value-bearing message's time %v, not the replaced value's %v",
			candidate.StatementTime, finalizeAt(18), finalizeAt(6))
	}
	marked := 0
	for _, ref := range candidate.Evidence {
		if ref.Statement {
			marked++
			if ref.MessageID != 6 {
				t.Fatalf("statement marked on m%d, want m06", ref.MessageID)
			}
		}
	}
	if marked != 1 {
		t.Fatalf("statement marks=%d, want exactly one", marked)
	}
	encoded, err := candidate.EvidenceJSON()
	if err != nil || !bytes.Contains([]byte(encoded), []byte(`"statement_message_id":6`)) {
		t.Fatalf("evidence json=%q err=%v, want the statement message named", encoded, err)
	}

	// Unnamed, the newest evidence message stands in: the failure this
	// default avoids is stamping an updated value with a superseded
	// utterance's time.
	unnamed := &scriptedProposer{proposals: []SessionRawProposal{valueProposal(0)}}
	fallbackService := newFinalizeService(t, fakeFinalizeBoards{}, fakeFinalizeMessages{messages: messages},
		nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{}, nil, unnamed,
		&scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "new"}}, 50)
	fallback, err := fallbackService.Extract(context.Background(), request)
	if err != nil {
		t.Fatalf("fallback extract: %v", err)
	}
	if len(fallback.Proposals) != 1 || !fallback.Proposals[0].StatementTime.Equal(finalizeAt(18)) {
		t.Fatalf("fallback=%+v, want the newest evidence message's time", fallback.Proposals)
	}

	// Naming a message outside the proposal's own evidence set is
	// incoherent about where the value came from: shape failure.
	incoherent := &scriptedProposer{proposals: []SessionRawProposal{valueProposal(9)}}
	incoherentService := newFinalizeService(t, fakeFinalizeBoards{}, fakeFinalizeMessages{messages: messages},
		nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{}, nil, incoherent,
		&scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "x"}}, 50)
	refused, err := incoherentService.Extract(context.Background(), request)
	if err != nil {
		t.Fatalf("incoherent extract: %v", err)
	}
	if len(refused.Proposals) != 0 || len(refused.Skipped) != 1 || refused.Skipped[0].Reason != FinalizeSkipInvalidShape {
		t.Fatalf("refused=%+v skipped=%+v", refused.Proposals, refused.Skipped)
	}
}

// TestFinalizeSemanticDuplicateSkipped: the plan's "同一意思" row - a
// candidate the relation judgement finds already covered is skipped outright,
// the existing entry stands, and the learner never sees a card for it.
func TestFinalizeSemanticDuplicateSkipped(t *testing.T) {
	board := ext01BoardWithPreference(t)
	existing := globalEntry("P-02", "出题后先等用户自己说完思路，再对答案")
	relations := &scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationDuplicate, TargetMemoryID: "P-02", Reason: "same meaning"}}
	service := newFinalizeService(t, fakeFinalizeBoards{content: &board}, fakeFinalizeMessages{messages: ext01Messages()},
		nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{entries: []model.LongTermMemory{existing}}, nil,
		&scriptedProposer{proposals: []SessionRawProposal{preferenceProposal([]uint{3}, nil)}}, relations, 0)

	result, err := service.Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-dev-EXT-01"}})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(result.Proposals) != 0 {
		t.Fatalf("proposals=%d, want the duplicate skipped, not proposed", len(result.Proposals))
	}
	if len(result.Skipped) != 1 || result.Skipped[0].Reason != FinalizeSkipSemanticDuplicate {
		t.Fatalf("skipped=%+v, want semantic_duplicate", result.Skipped)
	}
	if relations.calls != 1 {
		t.Fatalf("relation calls=%d, want the judgement that decided the skip", relations.calls)
	}
	// The skip is terminal: the watermark advances past it.
	if result.Cursor.CoveredMessageID != 11 {
		t.Fatalf("cursor=%+v, want advanced - a skip is a decided state", result.Cursor)
	}
	if existing.Status != model.LongTermMemoryActive {
		t.Fatalf("existing entry status=%s, want untouched", existing.Status)
	}
}

// fakeFinalizeDrafts is the draft entry stand-in: it records the requests it
// received and can fail selected calls.
type fakeFinalizeDrafts struct {
	calls  int
	ids    []string
	failOn map[int]error
}

func (f *fakeFinalizeDrafts) ProposeDraft(ctx context.Context, request DraftRequest) (*model.MemoryDraft, bool, error) {
	f.calls++
	if err, ok := f.failOn[f.calls]; ok {
		return nil, false, err
	}
	id := fmt.Sprintf("draft-%d", f.calls)
	f.ids = append(f.ids, id)
	return &model.MemoryDraft{ID: id, UserID: request.UserID, SessionID: request.SessionID, RunID: request.RunID}, true, nil
}

// TestFinalizeHandoffWatermarkRules: the processing watermark advances only
// when every candidate reached a terminal state - persisted at the draft
// entry, or explicitly skipped. A failed draft write or a relation judgement
// that never completed holds the watermark; extraction-layer skips do not.
func TestFinalizeHandoffWatermarkRules(t *testing.T) {
	board := ext01BoardWithPreference(t)
	newService := func(relations GlobalRelationResolver, proposer SessionCandidateProposer) *SessionFinalizeService {
		return newFinalizeService(t, fakeFinalizeBoards{content: &board}, fakeFinalizeMessages{messages: ext01Messages()},
			nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}},
			fakeFinalizeGlobalMind{entries: []model.LongTermMemory{globalEntry("P-02", "出题后先等用户自己说完思路，再对答案")}}, nil,
			proposer, relations, 0)
	}
	handoff := func(relations GlobalRelationResolver, proposer SessionCandidateProposer, drafts *fakeFinalizeDrafts) HandoffResult {
		t.Helper()
		outcome, err := newService(relations, proposer).Handoff(
			context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-dev-EXT-01"}}, drafts)
		if err != nil {
			t.Fatalf("handoff: %v", err)
		}
		return outcome
	}

	// Every candidate persisted: the watermark advances.
	persisted := &fakeFinalizeDrafts{}
	happy := handoff(&scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "new"}},
		&scriptedProposer{proposals: []SessionRawProposal{preferenceProposal([]uint{3}, nil)}}, persisted)
	if len(happy.Persisted) != 1 || len(happy.Failed) != 0 || !happy.Advanced || happy.Cursor.CoveredMessageID != 11 {
		t.Fatalf("happy=%+v advanced=%v", happy, happy.Advanced)
	}
	if persisted.calls != 1 || persisted.ids[0] == "" {
		t.Fatalf("draft calls=%d ids=%v", persisted.calls, persisted.ids)
	}

	// The draft write fails: the candidate is undecided, the watermark holds,
	// and the next trigger retries the same progress with the same key.
	failing := &fakeFinalizeDrafts{failOn: map[int]error{1: errors.New("draft store unavailable")}}
	held := handoff(&scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "new"}},
		&scriptedProposer{proposals: []SessionRawProposal{preferenceProposal([]uint{3}, nil)}}, failing)
	if len(held.Failed) != 1 || held.Advanced || held.Cursor.CoveredMessageID != 0 {
		t.Fatalf("held=%+v, want the failure to hold the watermark", held)
	}

	// A relation judgement that never completed is undecided too.
	retryHeld := handoff(&scriptedRelations{err: errors.New("judge unavailable")},
		&scriptedProposer{proposals: []SessionRawProposal{preferenceProposal([]uint{3}, nil)}}, &fakeFinalizeDrafts{})
	if len(retryHeld.RetryPending) != 1 || retryHeld.Advanced || retryHeld.Cursor.CoveredMessageID != 0 {
		t.Fatalf("retry=%+v, want the unfinished judgement to hold the watermark", retryHeld)
	}

	// Extraction-layer skips are decided states: they do not block the
	// watermark, and the decided candidate beside them still persists.
	rogue := &scriptedProposer{proposals: []SessionRawProposal{
		{Scope: model.MemoryScopeGlobal, Class: FinalizeClassTeachingPreference, Subject: "讲解偏好",
			Durability: model.MemoryDurabilityStable, Content: "讲新概念先举例再讲定义。", EvidenceIDs: []uint{99}},
		preferenceProposal([]uint{3}, nil),
	}}
	mixedDrafts := &fakeFinalizeDrafts{}
	mixed := handoff(&scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "new"}}, rogue, mixedDrafts)
	if len(mixed.Skipped) != 1 || len(mixed.Persisted) != 1 || !mixed.Advanced {
		t.Fatalf("mixed=%+v, want the skip decided and the watermark advanced", mixed)
	}

	// A session with nothing new never advances, and never calls the draft
	// entry again.
	emptyDrafts := &fakeFinalizeDrafts{}
	idle, err := newService(&scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "new"}},
		&scriptedProposer{}).Handoff(
		context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-dev-EXT-01", CoveredMessageID: 11}}, emptyDrafts)
	if err != nil {
		t.Fatalf("idle handoff: %v", err)
	}
	if !idle.NoNewContent || idle.Advanced || emptyDrafts.calls != 0 {
		t.Fatalf("idle=%+v draft calls=%d, want nothing new and no draft call", idle, emptyDrafts.calls)
	}
}
