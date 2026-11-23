package memory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

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

// LoadUnprocessedMessages returns the messages past the watermark. The fake
// skips batching: it hands back everything after the watermark in one go,
// which is what the production adapter's batches add up to.
func (f fakeFinalizeMessages) LoadUnprocessedMessages(ctx context.Context, userID int64, sessionID string, afterID uint) ([]SessionMessage, error) {
	result := make([]SessionMessage, 0, len(f.messages))
	for _, message := range f.messages {
		if message.ID > afterID {
			result = append(result, message)
		}
	}
	return result, nil
}

// LoadSessionMessagesByID returns the named messages when they belong to the
// fake's store - the clue read.
func (f fakeFinalizeMessages) LoadSessionMessagesByID(ctx context.Context, userID int64, sessionID string, messageIDs []uint) ([]SessionMessage, error) {
	wanted := make(map[uint]struct{}, len(messageIDs))
	for _, id := range messageIDs {
		wanted[id] = struct{}{}
	}
	result := make([]SessionMessage, 0, len(messageIDs))
	for _, message := range f.messages {
		if _, ok := wanted[message.ID]; ok {
			result = append(result, message)
		}
	}
	return result, nil
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
	calls int
	// result serves every call when the queue is empty.
	result GlobalRelationResult
	// queue serves one call each, in order; a contrast pair needs the two
	// judgements to differ.
	queue []GlobalRelationResult
	err   error
}

func (r *scriptedRelations) Classify(ctx context.Context, incoming ExplicitMemoryInput, existing []model.LongTermMemory) (GlobalRelationResult, error) {
	r.calls++
	if len(r.queue) > 0 {
		result := r.queue[0]
		r.queue = r.queue[1:]
		return result, nil
	}
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
	scope, class := model.MemoryScopeGlobal, model.ProfileClassTeachingPreference
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
	if candidate.Scope != model.MemoryScopeGlobal || candidate.Class != model.ProfileClassTeachingPreference {
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
		EvidenceIDs: []uint{8, 3}, StatementEvidenceID: 8, TopicUnique: true,
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
	// The named primary evidence carries the statement: m08 is the learner's
	// final wording, m03 the earlier understanding kept as context.
	if !candidate.StatementTime.Equal(finalizeAt(58)) {
		t.Fatalf("statement time=%v, want the primary evidence m08's time", candidate.StatementTime)
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
		Scope: model.MemoryScopeGlobal, Class: model.ProfileClassTeachingPreference, Subject: "讲解偏好",
		Durability: model.MemoryDurabilityStable,
		Content:    "讲新概念先举例再讲定义。", EvidenceIDs: []uint{8},
	}
	unaccepted := SessionRawProposal{
		Scope: model.MemoryScopeGlobal, Class: model.ProfileClassTeachingPreference, Subject: "讲解偏好",
		Durability: model.MemoryDurabilityStable,
		Content:    "讲新概念先举例再讲定义。", EvidenceIDs: []uint{3}, AssistantIDs: []uint{8},
	}
	hallucinated := SessionRawProposal{
		Scope: model.MemoryScopeGlobal, Class: model.ProfileClassTeachingPreference, Subject: "讲解偏好",
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
		Scope: model.MemoryScopeGlobal, Class: model.ProfileClassTeachingPreference, Subject: "复习偏好",
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

	// A later trigger sees one more message and proposes the same content
	// resting on that new message: covered words are only visible through
	// the board, which cites m03 here - but the content is what repeats,
	// so the fingerprint is what suppresses it.
	messages := append(ext01Messages(), SessionMessage{ID: 12, IsUser: true, Content: "以后讲新概念还是先举例再讲定义，接着做 select 的题。", CreatedAt: finalizeAt(60)})
	retryService := newFinalizeService(t, fakeFinalizeBoards{content: &board}, fakeFinalizeMessages{messages: messages},
		nil, known, fakeFinalizeGlobalMind{}, nil,
		&scriptedProposer{proposals: []SessionRawProposal{preferenceProposal([]uint{12}, nil)}}, relations, 50)
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
			EvidenceIDs: []uint{1, 3}, StatementEvidenceID: 1, TopicUnique: true,
		},
		{
			Scope: model.MemoryScopeGlobal, Class: model.ProfileClassConstraint, Subject: "学习时间",
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
	if result.Proposals[1].Class != model.ProfileClassConstraint {
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
		{"over-long content", SessionRawProposal{Scope: model.MemoryScopeGlobal, Class: model.ProfileClassGoal, Subject: "s", Durability: model.MemoryDurabilityStable, Content: string(make([]rune, 401)), EvidenceIDs: []uint{3}}, FinalizeSkipInvalidShape},
		{"global with topic", SessionRawProposal{Scope: model.MemoryScopeGlobal, Class: model.ProfileClassGoal, Subject: "s", Content: "c", TopicKey: "go-basics", Durability: model.MemoryDurabilityStable, EvidenceIDs: []uint{3}}, FinalizeSkipInvalidShape},
		{"time bound without expiry", SessionRawProposal{Scope: model.MemoryScopeGlobal, Class: model.ProfileClassGoal, Subject: "s", Content: "c", Durability: model.MemoryDurabilityTimeBound, EvidenceIDs: []uint{3}}, FinalizeSkipInvalidShape},
		{"empty evidence", SessionRawProposal{Scope: model.MemoryScopeGlobal, Class: model.ProfileClassGoal, Subject: "s", Content: "c", Durability: model.MemoryDurabilityStable}, FinalizeSkipEvidenceMissing},
		{"too many evidence ids", func() SessionRawProposal {
			ids := make([]uint, 0, 9)
			for id := uint(1); id <= 9; id++ {
				ids = append(ids, id)
			}
			return SessionRawProposal{Scope: model.MemoryScopeGlobal, Class: model.ProfileClassGoal, Subject: "s", Content: "c", Durability: model.MemoryDurabilityStable, EvidenceIDs: ids}
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
			Scope: model.MemoryScopeGlobal, Class: model.ProfileClassGoal, Subject: "背诵目标",
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

	// Several messages and no primary named: the program cannot know which
	// utterance the value came from, and guessing is what stamped an
	// updated value with a replaced utterance's time. The proposal is
	// refused instead of guessed.
	unnamed := &scriptedProposer{proposals: []SessionRawProposal{valueProposal(0)}}
	unnamedService := newFinalizeService(t, fakeFinalizeBoards{}, fakeFinalizeMessages{messages: messages},
		nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{}, nil, unnamed,
		&scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "x"}}, 50)
	refusedUnnamed, err := unnamedService.Extract(context.Background(), request)
	if err != nil {
		t.Fatalf("unnamed extract: %v", err)
	}
	if len(refusedUnnamed.Proposals) != 0 || len(refusedUnnamed.Skipped) != 1 ||
		refusedUnnamed.Skipped[0].Reason != FinalizeSkipStatementEvidenceRequired {
		t.Fatalf("unnamed=%+v skipped=%+v, want statement_evidence_required", refusedUnnamed.Proposals, refusedUnnamed.Skipped)
	}

	// A single-evidence proposal needs no name: there is only one message
	// the time could come from.
	single := &scriptedProposer{proposals: []SessionRawProposal{{
		Scope: model.MemoryScopeGlobal, Class: model.ProfileClassGoal, Subject: "背诵目标",
		Durability: model.MemoryDurabilityStable,
		Content:    "每天背 37 个单词。", EvidenceIDs: []uint{6},
	}}}
	singleService := newFinalizeService(t, fakeFinalizeBoards{}, fakeFinalizeMessages{messages: messages},
		nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{}, nil, single,
		&scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "new"}}, 50)
	singleResult, err := singleService.Extract(context.Background(), request)
	if err != nil {
		t.Fatalf("single extract: %v", err)
	}
	if len(singleResult.Proposals) != 1 || !singleResult.Proposals[0].StatementTime.Equal(finalizeAt(18)) {
		t.Fatalf("single=%+v, want the only evidence message's time", singleResult.Proposals)
	}
	if len(singleResult.Proposals[0].Evidence) != 1 || !singleResult.Proposals[0].Evidence[0].Statement {
		t.Fatalf("single evidence=%+v, want the only reference marked as the statement", singleResult.Proposals[0].Evidence)
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
	skipOn map[int]string
}

func (f *fakeFinalizeDrafts) ProposeDraft(ctx context.Context, request DraftRequest) (*model.MemoryDraft, bool, string, error) {
	f.calls++
	if err, ok := f.failOn[f.calls]; ok {
		return nil, false, "", err
	}
	if skip, ok := f.skipOn[f.calls]; ok {
		return nil, false, skip, nil
	}
	id := fmt.Sprintf("draft-%d", f.calls)
	f.ids = append(f.ids, id)
	return &model.MemoryDraft{ID: id, UserID: request.UserID, SessionID: request.SessionID, RunID: request.RunID}, true, "", nil
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
		{Scope: model.MemoryScopeGlobal, Class: model.ProfileClassTeachingPreference, Subject: "讲解偏好",
			Durability: model.MemoryDurabilityStable, Content: "讲新概念先举例再讲定义。", EvidenceIDs: []uint{99}},
		preferenceProposal([]uint{3}, nil),
	}}
	mixedDrafts := &fakeFinalizeDrafts{}
	mixed := handoff(&scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "new"}}, rogue, mixedDrafts)
	if len(mixed.Skipped) != 1 || len(mixed.Persisted) != 1 || !mixed.Advanced {
		t.Fatalf("mixed=%+v, want the skip decided and the watermark advanced", mixed)
	}

	// The draft entry's own skip decisions are terminal too: the candidate
	// that lost to the same dedup and conflict rules the other write paths
	// obey is recorded as an explicit skip, and the watermark moves past it.
	entrySkip := &fakeFinalizeDrafts{skipOn: map[int]string{1: SkipDuplicateExisting}}
	skipped := handoff(&scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "new"}},
		&scriptedProposer{proposals: []SessionRawProposal{preferenceProposal([]uint{3}, nil)}}, entrySkip)
	if len(skipped.Persisted) != 0 || len(skipped.Skipped) != 1 || skipped.Skipped[0].Reason != FinalizeSkipDraftDuplicateExisting {
		t.Fatalf("entry skip=%+v, want the draft-entry decision recorded", skipped)
	}
	if !skipped.Advanced || skipped.Cursor.CoveredMessageID != 11 {
		t.Fatalf("advanced=%v cursor=%+v, want the watermark to move past the skip", skipped.Advanced, skipped.Cursor)
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

// TestFinalizeSemanticDedupContrastPair is the dedup contrast pair: an
// equivalent paraphrase of an existing preference is skipped outright - the
// existing entry stands and the learner never sees a card - while a
// preference with a genuinely different scope still proposes and the two
// coexist. One judgement vocabulary, two opposite outcomes.
func TestFinalizeSemanticDedupContrastPair(t *testing.T) {
	board := ext01BoardWithPreference(t)
	existing := globalEntry("P-teach", "讲新概念时先举一个我熟悉的例子，再讲正式定义。")
	existing.ProfileClass = model.ProfileClassTeachingPreference
	relations := &scriptedRelations{queue: []GlobalRelationResult{
		{Relation: GlobalRelationDuplicate, TargetMemoryID: "P-teach", Reason: "等义改写"},
		{Relation: GlobalRelationIndependent, Reason: "适用场景不同，可并存"},
	}}
	// 排查偏好有自己的原话：它是另一条适用的场景，不是改写。
	messages := append(ext01Messages(), SessionMessage{
		ID: 12, IsUser: true,
		Content:   "以后排查报错的时候，先给我一个能跑的验证步骤，再讲原因。",
		CreatedAt: finalizeAt(45),
	})
	service := newFinalizeService(t, fakeFinalizeBoards{content: &board}, fakeFinalizeMessages{messages: messages},
		nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}},
		fakeFinalizeGlobalMind{entries: []model.LongTermMemory{existing}}, nil,
		&scriptedProposer{proposals: []SessionRawProposal{
			{
				Scope: model.MemoryScopeGlobal, Class: model.ProfileClassTeachingPreference, Subject: "讲解偏好",
				Durability:  model.MemoryDurabilityStable,
				Content:     "讲新东西先用熟悉的例子，再给定义。",
				EvidenceIDs: []uint{3}, StatementEvidenceID: 3,
			},
			{
				Scope: model.MemoryScopeGlobal, Class: model.ProfileClassTeachingPreference, Subject: "排查偏好",
				Durability:  model.MemoryDurabilityStable,
				Content:     "排查报错时，先给一个可运行的验证步骤，再讲原因。",
				EvidenceIDs: []uint{12}, StatementEvidenceID: 12,
			},
		}}, relations, 50)

	result, err := service.Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-dedup-pair"}})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(result.Skipped) != 1 || result.Skipped[0].Reason != FinalizeSkipSemanticDuplicate {
		t.Fatalf("skipped=%+v, want the paraphrase judged the same meaning and skipped", result.Skipped)
	}
	if len(result.Proposals) != 1 {
		t.Fatalf("proposals=%d, want the different-scope preference to coexist", len(result.Proposals))
	}
	coexisting := result.Proposals[0]
	if coexisting.Content != "排查报错时，先给一个可运行的验证步骤，再讲原因。" {
		t.Fatalf("coexisting body=%q", coexisting.Content)
	}
	if len(coexisting.RelatedOld) != 1 || coexisting.RelatedOld[0].ID != "P-teach" {
		t.Fatalf("related old=%+v, want the existing preference carried for the confirmation", coexisting.RelatedOld)
	}
	if existing.Status != model.LongTermMemoryActive {
		t.Fatalf("existing entry status=%s, want untouched", existing.Status)
	}
}

// TestFinalizeBodyKeepsLearnerLanguageScopeAndExcludesTemporary locks the
// body contract the extraction prompt requires: the body is in the learner's
// own language, the preference's stated scope stays in, and a one-turn
// request - even one that shaped this session's example - never appears in
// the content as a standing rule.
func TestFinalizeBodyKeepsLearnerLanguageScopeAndExcludesTemporary(t *testing.T) {
	board := ext01BoardWithPreference(t)
	// m05 is the one-turn scope limit ("这节就先用排队这个例子讲"). The
	// correct judgement keeps 排队 as the EXAMPLE the learner named in m03
	// and leaves the one-turn limit out of the body entirely.
	body := "讲新概念时，先举一个我熟悉的例子（比如讲缓冲就用排队打比方），再讲正式定义。"
	relations := &scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "new"}}
	service := newFinalizeService(t, fakeFinalizeBoards{content: &board}, fakeFinalizeMessages{messages: ext01Messages()},
		nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{}, nil,
		&scriptedProposer{proposals: []SessionRawProposal{{
			Scope: model.MemoryScopeGlobal, Class: model.ProfileClassTeachingPreference, Subject: "讲解偏好",
			Durability:  model.MemoryDurabilityStable,
			Content:     body,
			EvidenceIDs: []uint{3}, StatementEvidenceID: 3,
		}}}, relations, 0)

	result, err := service.Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-dev-EXT-01"}})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(result.Proposals) != 1 {
		t.Fatalf("proposals=%d skipped=%+v", len(result.Proposals), result.Skipped)
	}
	candidate := result.Proposals[0]
	// 学习者的语言：正文与证据同一语言，不翻译、不混杂。
	if !strings.Contains(candidate.Content, "讲新概念时") {
		t.Fatalf("body lost the learner's scope wording: %q", candidate.Content)
	}
	if strings.ContainsAny(candidate.Content, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ") &&
		!strings.Contains(candidate.Content, "select") {
		t.Fatalf("body drifted out of the learner's language: %q", candidate.Content)
	}
	// 适用范围保留在正文里。
	if !strings.Contains(candidate.Content, "先举一个我熟悉的例子") || !strings.Contains(candidate.Content, "再讲正式定义") {
		t.Fatalf("body lost the preference's scope: %q", candidate.Content)
	}
	// 临时要求不混进正文：m05 的一次性限定话术不出现在正文中。
	if strings.Contains(candidate.Content, "这节就先") || strings.Contains(candidate.Content, "别再展开别的类比") {
		t.Fatalf("body mixed a one-turn request in as a standing rule: %q", candidate.Content)
	}
	if !candidate.StatementTime.Equal(finalizeAt(6)) {
		t.Fatalf("statement time=%v, want m03's time", candidate.StatementTime)
	}
}

// TestFinalizeCandidateBoundariesReminderInsideTopicAndIndependentPreference
// is the DED-04 boundary contrast: a reminder inside one sentence that only
// serves remembering a topic difficulty stays IN that topic's card - one
// candidate, not a second "please remember" card - while an independent
// preference expressed on its own still proposes separately.
func TestFinalizeCandidateBoundariesReminderInsideTopicAndIndependentPreference(t *testing.T) {
	at := func(minute int) time.Time {
		return time.Date(2026, 11, 23, 20, minute, 0, 0, time.FixedZone("CST", 8*3600))
	}
	pitfall := SessionMessage{ID: 3, IsUser: true,
		Content:   "我发现我老忘记 Save 传指针这回事，零值字段根本不更新——这个坑记下来，属于 GORM 这个主题。",
		CreatedAt: at(35)}
	preference := SessionMessage{ID: 5, IsUser: true,
		Content:   "另外记个习惯：我每次学完一个模块喜欢自己画一张时序图，你之后讲新模块的时候，也可以先让我自己画。",
		CreatedAt: at(44)}
	pitfallProposal := SessionRawProposal{
		Scope: model.MemoryScopeTopic, Kind: FinalizeKindUnderstanding, TopicKey: "gorm",
		Subject: "Save 传指针", Durability: model.MemoryDurabilityStable,
		// 一张主题卡同时保留容易混淆的知识点和「以后讲到时提醒」。
		Content:     "我容易混淆 GORM 中 Save 的传参方式：值传递时零值字段不会更新；以后讲到 GORM 时提醒我复习这一点。",
		EvidenceIDs: []uint{pitfall.ID}, StatementEvidenceID: pitfall.ID,
	}
	preferenceProposal := SessionRawProposal{
		Scope: model.MemoryScopeGlobal, Class: model.ProfileClassTeachingPreference, Subject: "学习习惯",
		Durability:  model.MemoryDurabilityStable,
		Content:     "我学完一个模块喜欢自己画一张时序图；讲新模块时先让我自己画。",
		EvidenceIDs: []uint{preference.ID}, StatementEvidenceID: preference.ID,
	}
	topics := fakeFinalizeCatalogue{topics: []Topic{{Key: "gorm", Name: "GORM"}}}
	relations := &scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "new"}}
	newService := func(messages []SessionMessage, proposals []SessionRawProposal) *SessionFinalizeService {
		return newFinalizeService(t, fakeFinalizeBoards{}, fakeFinalizeMessages{messages: messages},
			topics, &fakeFinalizeFingerprints{known: map[string]struct{}{}},
			fakeFinalizeGlobalMind{}, fakeFinalizeTopicMind{byTopic: map[string][]model.LongTermMemory{}},
			&scriptedProposer{proposals: proposals}, relations, 50)
	}

	// 正例：只有那句难点+提醒 → 只出一张主题卡，且卡里同时保留
	// 「容易混淆的知识点」和「以后讲到时提醒」。
	merged, err := newService([]SessionMessage{pitfall}, []SessionRawProposal{pitfallProposal}).
		Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-ded04-merge"}})
	if err != nil {
		t.Fatalf("merged extract: %v", err)
	}
	if len(merged.Proposals) != 1 || merged.Proposals[0].Scope != model.MemoryScopeTopic {
		t.Fatalf("merged=%+v, want exactly one topic card", merged.Proposals)
	}
	card := merged.Proposals[0]
	if card.TopicKey != "gorm" || card.Evidence[0].MessageID != pitfall.ID {
		t.Fatalf("merged candidate=%+v", card)
	}
	if !strings.Contains(card.Content, "零值字段不会更新") {
		t.Fatalf("card lost the confusing point: %q", card.Content)
	}
	if !strings.Contains(card.Content, "提醒") {
		t.Fatalf("card lost the reminder intent: %q", card.Content)
	}

	// 反例（对照）：同一句话里另有独立、可跨主题使用的偏好 → 主题卡仍
	// 只出一张（两个要点都在），独立偏好单独提议。
	sameSentence := SessionMessage{
		ID: 3, IsUser: true, CreatedAt: at(35),
		Content: "我发现我老忘记 Save 传指针，零值字段根本不更新——这个坑记下来，属于 GORM 这个主题。" +
			"另外，我学完模块喜欢自己画时序图，以后讲新模块先让我自己画。",
	}
	sameSentencePreference := preferenceProposal
	sameSentencePreference.EvidenceIDs = []uint{sameSentence.ID}
	sameSentencePreference.StatementEvidenceID = sameSentence.ID
	split, err := newService([]SessionMessage{sameSentence}, []SessionRawProposal{pitfallProposal, sameSentencePreference}).
		Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-ded04-split"}})
	if err != nil {
		t.Fatalf("split extract: %v", err)
	}
	if len(split.Proposals) != 2 {
		t.Fatalf("split=%+v, want the topic note and the independent preference", split.Proposals)
	}
	scopes := map[model.MemoryScope]int{}
	topicCardKeptBoth := false
	for _, candidate := range split.Proposals {
		scopes[candidate.Scope]++
		if candidate.Scope == model.MemoryScopeTopic {
			topicCardKeptBoth = strings.Contains(candidate.Content, "零值字段不会更新") &&
				strings.Contains(candidate.Content, "提醒")
		}
	}
	if scopes[model.MemoryScopeTopic] != 1 || scopes[model.MemoryScopeGlobal] != 1 {
		t.Fatalf("scopes=%v, want one topic and one global", scopes)
	}
	if !topicCardKeptBoth {
		t.Fatalf("split=%+v, the topic card must keep both the confusion and the reminder", split.Proposals)
	}

	// 规则锁定：提示词写明这两条候选边界。
	for _, fragment := range []string{
		"belongs to that sentence's subject",
		"keeps BOTH the confusing point AND the wish to be reminded",
		"is still its own global candidate",
		"even when it appears in the same utterance",
	} {
		if !strings.Contains(finalizeProposerPrompt, fragment) {
			t.Fatalf("extraction prompt missing boundary rule %q", fragment)
		}
	}
}

// TestFinalizeMetaInstructionIsNotProposed is the evidence-backed fix for the
// directed retest's extra card: an instruction about the proposal process
// itself (不要再问我"要不要记") is a session directive, not a durable
// preference. The contrast is in the same session: the real preference stated
// aloud still proposes, while the meta-instruction is neither a candidate nor
// folded into the preference's body.
func TestFinalizeMetaInstructionIsNotProposed(t *testing.T) {
	at := func(minute int) time.Time {
		return time.Date(2026, 11, 23, 21, minute, 0, 0, time.FixedZone("CST", 8*3600))
	}
	messages := []SessionMessage{
		{ID: 3, IsUser: true, CreatedAt: at(1),
			Content: "以后讲新模块的时候，先让我自己画一张时序图，你再讲。"},
		{ID: 4, IsUser: true, CreatedAt: at(2),
			Content: "我已经说过的偏好直接记下来即可，不要再问我\"要不要记\"。"},
	}
	preference := SessionRawProposal{
		Scope: model.MemoryScopeGlobal, Class: model.ProfileClassTeachingPreference, Subject: "学习习惯",
		Durability:  model.MemoryDurabilityStable,
		Content:     "讲新模块时先让我自己画一张时序图，再开始讲。",
		EvidenceIDs: []uint{3}, StatementEvidenceID: 3,
	}
	service := newFinalizeService(t, fakeFinalizeBoards{}, fakeFinalizeMessages{messages: messages},
		nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{}, nil,
		&scriptedProposer{proposals: []SessionRawProposal{preference}},
		&scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "new"}}, 50)
	result, err := service.Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-meta"}})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(result.Proposals) != 1 {
		t.Fatalf("proposals=%d, want only the real preference", len(result.Proposals))
	}
	card := result.Proposals[0]
	if card.Evidence[0].MessageID != 3 {
		t.Fatalf("evidence=%+v, want the preference's own words", card.Evidence)
	}
	for _, forbidden := range []string{"不要再问", "要不要记", "直接记下来即可"} {
		if strings.Contains(card.Content, forbidden) {
			t.Fatalf("the meta-instruction leaked into the card body: %q", card.Content)
		}
	}
	if !strings.Contains(finalizeProposerPrompt, "session directive, not a durable preference") {
		t.Fatalf("the extraction prompt lost the meta-instruction rule")
	}
}

// TestGlobalRelationNewValueIsReplacementNotDuplicate is the evidence-backed
// fix for CNF-01 R2: a proposal stating a different current value of the same
// matter must be a replacement proposal, never a duplicate skip. The contrast
// keeps the other side honest: the same value said again is still a duplicate
// and is skipped.
func TestGlobalRelationNewValueIsReplacementNotDuplicate(t *testing.T) {
	board := ext01BoardWithPreference(t)
	existing := globalEntry("P-go", "当前项目用 Go 1.22。")
	existing.ProfileClass = model.ProfileClassBackground
	proposal := func(content string) SessionRawProposal {
		return SessionRawProposal{
			Scope: model.MemoryScopeGlobal, Class: model.ProfileClassBackground, Subject: "Go 版本",
			Durability: model.MemoryDurabilityStable,
			Content:    content, EvidenceIDs: []uint{3}, StatementEvidenceID: 3,
		}
	}
	newService := func(relation GlobalRelationResult) *SessionFinalizeService {
		return newFinalizeService(t, fakeFinalizeBoards{content: &board}, fakeFinalizeMessages{messages: ext01Messages()},
			nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}},
			fakeFinalizeGlobalMind{entries: []model.LongTermMemory{existing}}, nil,
			&scriptedProposer{proposals: []SessionRawProposal{proposal("当前项目用 Go 1.23。")}},
			&scriptedRelations{result: relation}, 0)
	}

	// 新值：替换建议，携带旧条目供确认对照，绝不跳过。
	replacement, err := newService(GlobalRelationResult{
		Relation: GlobalRelationReplacement, TargetMemoryID: "P-go", Reason: "same matter, new value",
	}).Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-newvalue"}})
	if err != nil {
		t.Fatalf("replacement extract: %v", err)
	}
	if len(replacement.Proposals) != 1 || len(replacement.Skipped) != 0 {
		t.Fatalf("replacement=%+v skipped=%+v, want a replacement proposal", replacement.Proposals, replacement.Skipped)
	}
	if replacement.Proposals[0].Relation.Relation != GlobalRelationReplacement ||
		len(replacement.Proposals[0].RelatedOld) != 1 || replacement.Proposals[0].RelatedOld[0].ID != "P-go" {
		t.Fatalf("candidate=%+v, want replacement carrying the old value", replacement.Proposals[0])
	}

	// 对照：同一值再说一遍仍是重复并跳过。
	duplicate, err := newService(GlobalRelationResult{
		Relation: GlobalRelationDuplicate, TargetMemoryID: "P-go", Reason: "same value restated",
	}).Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-samevalue"}})
	if err != nil {
		t.Fatalf("duplicate extract: %v", err)
	}
	if len(duplicate.Proposals) != 0 || len(duplicate.Skipped) != 1 || duplicate.Skipped[0].Reason != FinalizeSkipSemanticDuplicate {
		t.Fatalf("duplicate=%+v skipped=%+v, want the same-value restatement skipped", duplicate.Proposals, duplicate.Skipped)
	}

	// 规则锁定：新值不是重复。
	for _, fragment := range []string{
		"A new value is never a duplicate; duplicate is the same value said again",
		"当前项目用 Go 1.22 and 当前项目用 Go 1.23 are replacement, not duplicate",
	} {
		if !strings.Contains(globalRelationPrompt, fragment) {
			t.Fatalf("the global relation prompt lost the new-value rule %q", fragment)
		}
	}
}

// TestFinalizeProposerRequestCarriesBoardClueAndUtterances is the request-level
// audit of the board wiring: with a whiteboard, the proposer's actual request
// carries both the board's clue and the cited utterance; without a board it
// still carries the recent utterances and extraction continues. The board is
// only a clue - a candidate that cites words outside the visible set is
// refused even when the board exists.
func TestFinalizeProposerRequestCarriesBoardClueAndUtterances(t *testing.T) {
	board := ext01BoardWithPreference(t)
	topicCall := `{"scope":"topic","kind":"understanding","topic_key":"go-basics","subject":"讲解偏好",` +
		`"proposed_content":"讲新概念时先举一个我熟悉的例子，再讲正式定义。","durability":"stable",` +
		`"evidence_ids":[3],"statement_evidence_id":3}`
	fabricatedCall := `{"scope":"topic","kind":"understanding","topic_key":"go-basics","subject":"讲解偏好",` +
		`"proposed_content":"讲新概念时先举一个我熟悉的例子。","durability":"stable",` +
		`"evidence_ids":[99],"statement_evidence_id":99}`
	topics := fakeFinalizeCatalogue{topics: []Topic{{Key: "go-basics", Name: "Go 基础"}}}
	newService := func(boards FinalizeBoardSource, chat *memoryChat, window int) *SessionFinalizeService {
		proposer, err := NewModelSessionProposer(chat)
		if err != nil {
			t.Fatalf("proposer: %v", err)
		}
		return newFinalizeService(t, boards, fakeFinalizeMessages{messages: ext01Messages()},
			topics, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{}, nil,
			proposer, &scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "new"}}, window)
	}

	// 有白板：请求同时包含白板线索与对应原话，候选通过原话校验。
	withBoard := &memoryChat{response: toolMessage(finalizeToolTopic, topicCall)}
	result, err := newService(fakeFinalizeBoards{content: &board}, withBoard, 0).
		Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-board"}})
	if err != nil {
		t.Fatalf("board extract: %v", err)
	}
	if len(result.Proposals) != 1 {
		t.Fatalf("proposals=%d skipped=%+v, want the candidate to pass utterance validation", len(result.Proposals), result.Skipped)
	}
	if len(withBoard.messages) != 2 {
		t.Fatalf("model messages=%d, want system plus payload", len(withBoard.messages))
	}
	payload := withBoard.messages[1].Content
	if !strings.Contains(payload, "teaching-preference") || !strings.Contains(payload, "先举例再讲定义") {
		t.Fatalf("the request lost the board clue: %s", payload)
	}
	if !strings.Contains(payload, "以后你给我讲新的概念，都先举一个我熟悉的例子再讲定义") {
		t.Fatalf("the request lost the cited utterance the board points at: %s", payload)
	}

	// 缺板：请求里白板为空，但近期原话仍在，提取继续。
	noBoard := &memoryChat{response: toolMessage(finalizeToolTopic, topicCall)}
	noBoardResult, err := newService(fakeFinalizeBoards{}, noBoard, 50).
		Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-no-board"}})
	if err != nil {
		t.Fatalf("no-board extract: %v", err)
	}
	if !noBoardResult.BoardMissing || len(noBoardResult.Proposals) != 1 {
		t.Fatalf("board missing=%v proposals=%d, want extraction from utterances alone", noBoardResult.BoardMissing, len(noBoardResult.Proposals))
	}
	noBoardPayload := noBoard.messages[1].Content
	if strings.Contains(noBoardPayload, "teaching-preference") {
		t.Fatalf("the request invented a board clue: %s", noBoardPayload)
	}
	if !strings.Contains(noBoardPayload, "以后你给我讲新的概念") {
		t.Fatalf("the no-board request lost the recent utterance: %s", noBoardPayload)
	}

	// 白板只是线索：即使板上有记录，引用可见集之外的原话照样被拒。
	fabricated := &memoryChat{response: toolMessage(finalizeToolTopic, fabricatedCall)}
	fabricatedResult, err := newService(fakeFinalizeBoards{content: &board}, fabricated, 0).
		Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-fabricated"}})
	if err != nil {
		t.Fatalf("fabricated extract: %v", err)
	}
	if len(fabricatedResult.Proposals) != 0 || len(fabricatedResult.Skipped) != 1 ||
		fabricatedResult.Skipped[0].Reason != FinalizeSkipEvidenceMissing {
		t.Fatalf("fabricated=%+v skipped=%+v, want the out-of-set citation refused", fabricatedResult.Proposals, fabricatedResult.Skipped)
	}
}

// TestFinalizeProposerNoToolCallIsRetryable is the zero-cost reproduction of
// the r8 common symptom: the extraction model answers in prose, with no tool
// call. Before the fix that produced an empty success and the watermark moved
// past a session the model never judged; now it is a retryable protocol
// failure and the progress stays put.
func TestFinalizeProposerNoToolCallIsRetryable(t *testing.T) {
	board := ext01BoardWithPreference(t)
	chat := &memoryChat{response: schema.AssistantMessage("我认为这个会话没有需要记录的内容。", nil)}
	proposer, err := NewModelSessionProposer(chat)
	if err != nil {
		t.Fatal(err)
	}
	service := newFinalizeService(t, fakeFinalizeBoards{content: &board}, fakeFinalizeMessages{messages: ext01Messages()},
		nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{}, nil,
		proposer, &scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "x"}}, 0)
	if _, err := service.Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-prose"}}); err == nil {
		t.Fatalf("a prose answer must be a retryable protocol failure, not an empty success")
	}
}

// TestFinalizeProposerRejectIsTerminalEmpty keeps the other side honest: the
// reject tool is a decision. It produces no card, it is recorded as a
// rejection, and the watermark advances.
func TestFinalizeProposerRejectIsTerminalEmpty(t *testing.T) {
	board := ext01BoardWithPreference(t)
	chat := &memoryChat{response: toolMessage(finalizeToolReject, `{"reject_reason":"one-turn logistics"}`)}
	proposer, err := NewModelSessionProposer(chat)
	if err != nil {
		t.Fatal(err)
	}
	service := newFinalizeService(t, fakeFinalizeBoards{content: &board}, fakeFinalizeMessages{messages: ext01Messages()},
		nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{}, nil,
		proposer, &scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "x"}}, 0)
	result, err := service.Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-reject"}})
	if err != nil {
		t.Fatalf("reject must not fail the run: %v", err)
	}
	if !result.ProposerRejected || len(result.Proposals) != 0 || result.Cursor.CoveredMessageID != 11 {
		t.Fatalf("result=%+v, want a recorded rejection with the watermark advanced", result)
	}
}

// TestFinalizeDevQuestionValidation runs the four development questions the
// fix must satisfy: clear long-term information proposes; temporary requests
// and memory-operation instructions produce no card; a new value enters the
// replacement flow; a same-value restatement produces no duplicate draft.
func TestFinalizeDevQuestionValidation(t *testing.T) {
	board := ext01BoardWithPreference(t)
	preference := SessionRawProposal{
		Scope: model.MemoryScopeGlobal, Class: model.ProfileClassTeachingPreference, Subject: "讲解偏好",
		Durability:  model.MemoryDurabilityStable,
		Content:     "讲新概念时先举一个我熟悉的例子，再讲正式定义。",
		EvidenceIDs: []uint{3}, StatementEvidenceID: 3,
	}
	newValue := SessionRawProposal{
		Scope: model.MemoryScopeGlobal, Class: model.ProfileClassBackground, Subject: "Go 版本",
		Durability:  model.MemoryDurabilityStable,
		Content:     "当前项目用 Go 1.23。",
		EvidenceIDs: []uint{3}, StatementEvidenceID: 3,
	}
	newService := func(chat *memoryChat, relations GlobalRelationResolver, entries []model.LongTermMemory) *SessionFinalizeService {
		proposer, err := NewModelSessionProposer(chat)
		if err != nil {
			t.Fatal(err)
		}
		return newFinalizeService(t, fakeFinalizeBoards{content: &board}, fakeFinalizeMessages{messages: ext01Messages()},
			nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}},
			fakeFinalizeGlobalMind{entries: entries}, nil, proposer, relations, 0)
	}
	topicCall := `{"scope":"global","class":"teaching_preference","subject":"讲解偏好",` +
		`"proposed_content":"讲新概念时先举一个我熟悉的例子，再讲正式定义。","durability":"stable",` +
		`"evidence_ids":[3],"statement_evidence_id":3}`
	newValueCall := `{"scope":"global","class":"background","subject":"Go 版本",` +
		`"proposed_content":"当前项目用 Go 1.23。","durability":"stable",` +
		`"evidence_ids":[3],"statement_evidence_id":3}`

	// 1. 明确长期信息 → 出卡。
	proposed, err := newService(&memoryChat{response: toolMessage(finalizeToolProfile, topicCall)},
		&scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "new"}}, nil).
		Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-dev-1"}})
	if err != nil || len(proposed.Proposals) != 1 {
		t.Fatalf("clear preference: proposals=%d err=%v", len(proposed.Proposals), err)
	}

	// 2. 临时要求与记忆操作指令 → 模型拒绝 → 零卡、终态记录。
	rejected, err := newService(&memoryChat{response: toolMessage(finalizeToolReject, `{"reject_reason":"temporary and meta instruction"}`)},
		&scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "x"}}, nil).
		Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-dev-2"}})
	if err != nil || len(rejected.Proposals) != 0 || !rejected.ProposerRejected {
		t.Fatalf("temporary/meta: proposals=%d rejected=%v err=%v", len(rejected.Proposals), rejected.ProposerRejected, err)
	}

	// 3. 新值 → 进入替换流程（携带旧条目）。
	old := globalEntry("P-ver", "当前项目用 Go 1.22。")
	old.ProfileClass = model.ProfileClassBackground
	replaced, err := newService(&memoryChat{response: toolMessage(finalizeToolProfile, newValueCall)},
		&scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationReplacement, TargetMemoryID: "P-ver", Reason: "new value"}},
		[]model.LongTermMemory{old}).
		Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-dev-3"}})
	if err != nil || len(replaced.Proposals) != 1 || replaced.Proposals[0].Relation.Relation != GlobalRelationReplacement ||
		len(replaced.Proposals[0].RelatedOld) != 1 {
		t.Fatalf("new value: proposals=%+v err=%v", replaced.Proposals, err)
	}

	// 4. 同值重述 → 判重跳过，零新卡。
	duplicate, err := newService(&memoryChat{response: toolMessage(finalizeToolProfile, newValueCall)},
		&scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationDuplicate, TargetMemoryID: "P-ver", Reason: "same value restated"}},
		[]model.LongTermMemory{old}).
		Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-dev-4"}})
	if err != nil || len(duplicate.Proposals) != 0 || len(duplicate.Skipped) != 1 ||
		duplicate.Skipped[0].Reason != FinalizeSkipSemanticDuplicate {
		t.Fatalf("same value: proposals=%d skipped=%+v err=%v", len(duplicate.Proposals), duplicate.Skipped, err)
	}
	_ = preference
	_ = newValue
}

// TestFinalizeProposerRejectNeedsReason tightens the terminal condition: the
// reject tool name alone is not an answer. A reject without a valid reason,
// or a broken proposal call beside a reject, leaves the progress retryable.
func TestFinalizeProposerRejectNeedsReason(t *testing.T) {
	board := ext01BoardWithPreference(t)
	newService := func(chat *memoryChat) *SessionFinalizeService {
		proposer, err := NewModelSessionProposer(chat)
		if err != nil {
			t.Fatal(err)
		}
		return newFinalizeService(t, fakeFinalizeBoards{content: &board}, fakeFinalizeMessages{messages: ext01Messages()},
			nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{}, nil,
			proposer, &scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "x"}}, 0)
	}

	// 拒绝参数无效：无 reason、reason 为空、字段类型错，都不得推进水位。
	for name, arguments := range map[string]string{
		"missing reason": `{"sensitive":false}`,
		"blank reason":   `{"reject_reason":"  ","sensitive":false}`,
		"wrong type":     `{"reject_reason":42}`,
	} {
		result, err := newService(&memoryChat{response: toolMessage(finalizeToolReject, arguments)}).
			Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-bad-reject"}})
		if err == nil {
			t.Fatalf("%s: want a retryable protocol failure, got result=%+v", name, result)
		}
	}

	// 候选全解析失败且混有（合法）拒绝：仍按可重试处理，不把拒绝当终态。
	mixed := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "call-1", Type: "function", Function: schema.FunctionCall{Name: finalizeToolProfile, Arguments: "{not-json"}},
		{ID: "call-2", Type: "function", Function: schema.FunctionCall{Name: finalizeToolReject, Arguments: `{"reject_reason":"looks empty"}`}},
	})
	if _, err := newService(&memoryChat{response: mixed}).
		Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-mixed"}}); err == nil {
		t.Fatalf("a broken proposal beside a reject must stay retryable")
	}

	// 对照：合法拒绝仍是明确的终态决定。
	rejected, err := newService(&memoryChat{response: toolMessage(finalizeToolReject, `{"reject_reason":"one-turn logistics"}`)}).
		Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-good-reject"}})
	if err != nil || !rejected.ProposerRejected || len(rejected.Proposals) != 0 {
		t.Fatalf("valid reject: result=%+v err=%v", rejected, err)
	}
}

// TestFinalizeAmbiguousTopicKeepsEmptyAttribution locks the ambiguity rule:
// when several catalogue topics could plausibly hold the note and the learner
// named none, the candidate keeps an empty attribution - it proposes, no old
// memory is compared yet, and the comparison happens only after the learner
// chooses. A unique exact topic still prefills (covered by the prefill test).
func TestFinalizeAmbiguousTopicKeepsEmptyAttribution(t *testing.T) {
	topics := fakeFinalizeCatalogue{topics: []Topic{
		{Key: "go-basics", Name: "Go 基础"},
		{Key: "go-concurrency", Name: "Go 并发"},
		{Key: "gorm", Name: "GORM"},
	}}
	relations := &scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "should not run"}}
	service := newFinalizeService(t, fakeFinalizeBoards{}, fakeFinalizeMessages{messages: ext01Messages()},
		topics, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{},
		fakeFinalizeTopicMind{byTopic: map[string][]model.LongTermMemory{}},
		&scriptedProposer{proposals: []SessionRawProposal{{
			Scope: model.MemoryScopeTopic, Kind: FinalizeKindUnderstanding, TopicKey: "",
			Subject: "切片与拷贝", Durability: model.MemoryDurabilityStable,
			Content:     "我容易混淆切片共享底层数组和拷贝。",
			EvidenceIDs: []uint{3}, StatementEvidenceID: 3,
		}}}, relations, 50)
	result, err := service.Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-ambiguous"}})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(result.Proposals) != 1 {
		t.Fatalf("proposals=%d skipped=%+v, want the candidate proposed with an empty attribution", len(result.Proposals), result.Skipped)
	}
	if !result.Proposals[0].TopicPending || result.Proposals[0].TopicKey != "" {
		t.Fatalf("candidate=%+v, want the topic left to the learner", result.Proposals[0])
	}
	if relations.calls != 0 {
		t.Fatalf("relation calls=%d, want no comparison before the topic is chosen", relations.calls)
	}
	if !strings.Contains(finalizeProposerPrompt, "leave topic_key empty") ||
		!strings.Contains(finalizeProposerPrompt, "compared with old memories in its final topic only after that choice") {
		t.Fatalf("the ambiguity rule is missing from the extraction prompt")
	}
}

// TestFinalizeOneOffVersusNewConfiguration is the paired development sample:
// an instruction scoped to this operation produces no card, while a
// configuration adopted from now on stays a candidate and enters the
// relationship judgement, which may propose a replacement.
func TestFinalizeOneOffVersusNewConfiguration(t *testing.T) {
	board := ext01BoardWithPreference(t)
	newService := func(chat *memoryChat, relations GlobalRelationResolver, entries []model.LongTermMemory) *SessionFinalizeService {
		proposer, err := NewModelSessionProposer(chat)
		if err != nil {
			t.Fatal(err)
		}
		return newFinalizeService(t, fakeFinalizeBoards{content: &board}, fakeFinalizeMessages{messages: ext01Messages()},
			nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}},
			fakeFinalizeGlobalMind{entries: entries}, nil, proposer, relations, 0)
	}

	// 只在这次操作：模型拒绝，零卡、终态记录。
	oneOff, err := newService(&memoryChat{response: toolMessage(finalizeToolReject, `{"reject_reason":"one-off formatting request for this answer only"}`)},
		&scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "x"}}, nil).
		Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-one-off"}})
	if err != nil || len(oneOff.Proposals) != 0 || !oneOff.ProposerRejected {
		t.Fatalf("one-off: proposals=%d rejected=%v err=%v", len(oneOff.Proposals), oneOff.ProposerRejected, err)
	}

	// 从此采用的新配置：保留为候选，关系判定给出替换建议。
	old := globalEntry("P-fmt", "示例代码统一用 Go 1.22 语法。")
	old.ProfileClass = model.ProfileClassConstraint
	newConfigCall := `{"scope":"global","class":"constraint","subject":"示例代码版本",` +
		`"proposed_content":"示例代码从此统一按 Go 1.23 语法写。","durability":"stable",` +
		`"evidence_ids":[3],"statement_evidence_id":3}`
	adopted, err := newService(&memoryChat{response: toolMessage(finalizeToolProfile, newConfigCall)},
		&scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationReplacement, TargetMemoryID: "P-fmt", Reason: "same matter, new configuration"}},
		[]model.LongTermMemory{old}).
		Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-new-config"}})
	if err != nil || len(adopted.Proposals) != 1 {
		t.Fatalf("new configuration: proposals=%d err=%v", len(adopted.Proposals), err)
	}
	if adopted.Proposals[0].Relation.Relation != GlobalRelationReplacement || len(adopted.Proposals[0].RelatedOld) != 1 {
		t.Fatalf("candidate=%+v, want the replacement suggestion carrying the old value", adopted.Proposals[0])
	}

	for _, fragment := range []string{
		"Separate a one-off operation from a new standing configuration",
		"An instruction scoped to this operation only",
		"the relationship judgement decides whether it replaces the older value",
	} {
		if !strings.Contains(finalizeProposerPrompt, fragment) {
			t.Fatalf("the one-off/new-configuration rule is missing %q", fragment)
		}
	}
}

// TestFinalizePrefillRequiresNamedTopic is the attribution-step reproduction:
// the model may propose an exact catalogue key, but the program prefills it
// today without checking that the learner named the topic. With several topics
// plausible and no name in the learner's words, the candidate must stay
// unattributed for the learner to choose; a named topic (by the learner, or by
// the assistant and accepted) still prefills.
func TestFinalizePrefillRequiresNamedTopic(t *testing.T) {
	topics := fakeFinalizeCatalogue{topics: []Topic{
		{Key: "go-concurrency", Name: "Go 并发"},
		{Key: "go-basics", Name: "Go 基础"},
	}}
	topicProposal := func(evidence uint, assistant []uint) SessionRawProposal {
		return SessionRawProposal{
			Scope: model.MemoryScopeTopic, Kind: FinalizeKindUnderstanding, TopicKey: "go-concurrency",
			Subject: "我的理解", Durability: model.MemoryDurabilityStable,
			Content:     "我容易混淆有缓冲与无缓冲 channel 的阻塞时机。",
			EvidenceIDs: []uint{evidence}, StatementEvidenceID: evidence, AssistantIDs: assistant,
		}
	}
	newService := func(messages []SessionMessage, proposal SessionRawProposal) *SessionFinalizeService {
		return newFinalizeService(t, fakeFinalizeBoards{}, fakeFinalizeMessages{messages: messages},
			topics, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{},
			fakeFinalizeTopicMind{byTopic: map[string][]model.LongTermMemory{}},
			&scriptedProposer{proposals: []SessionRawProposal{proposal}},
			&scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "x"}}, 50)
	}

	// 反例：学习者没点名、模型也没声明唯一 → 必须留空待归属。
	unnamed := []SessionMessage{
		{ID: 3, IsUser: true, Content: "这个坑记下来，我老是分不清什么时候会阻塞。", CreatedAt: finalizeAt(6)},
	}
	ambiguous, err := newService(unnamed, topicProposal(3, nil)).
		Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-unnamed"}})
	if err != nil {
		t.Fatalf("unnamed extract: %v", err)
	}
	if len(ambiguous.Proposals) != 1 || !ambiguous.Proposals[0].TopicPending || ambiguous.Proposals[0].TopicKey != "" {
		t.Fatalf("candidate=%+v, want the unnamed topic left to the learner", ambiguous.Proposals)
	}

	// 正例：学习者没点名，但模型明确声明唯一匹配 → 预填。
	unique := topicProposal(3, nil)
	unique.TopicUnique = true
	uniqueResult, err := newService(unnamed, unique).
		Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-unique"}})
	if err != nil {
		t.Fatalf("unique extract: %v", err)
	}
	if len(uniqueResult.Proposals) != 1 || uniqueResult.Proposals[0].TopicPending || uniqueResult.Proposals[0].TopicKey != "go-concurrency" {
		t.Fatalf("candidate=%+v, want the declared-unique topic prefilled", uniqueResult.Proposals)
	}

	// 正例：学习者点名 → 预填。
	named := []SessionMessage{
		{ID: 3, IsUser: true, Content: "这个坑记下来，属于 Go 并发 这个主题。", CreatedAt: finalizeAt(6)},
	}
	explicit, err := newService(named, topicProposal(3, nil)).
		Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-named"}})
	if err != nil {
		t.Fatalf("named extract: %v", err)
	}
	if len(explicit.Proposals) != 1 || explicit.Proposals[0].TopicPending || explicit.Proposals[0].TopicKey != "go-concurrency" {
		t.Fatalf("candidate=%+v, want the named topic prefilled", explicit.Proposals)
	}

	// 正例：助手点名、学习者明确接受 → 同样预填。
	accepted := []SessionMessage{
		{ID: 2, IsUser: false, Content: "建议记到 Go 基础 的主题记忆里。", CreatedAt: finalizeAt(5)},
		{ID: 3, IsUser: true, Content: "行，记吧。", CreatedAt: finalizeAt(6)},
	}
	acceptedProposal := topicProposal(3, []uint{2})
	acceptedProposal.TopicKey = "go-basics"
	viaAssistant, err := newService(accepted, acceptedProposal).
		Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-accepted"}})
	if err != nil {
		t.Fatalf("accepted extract: %v", err)
	}
	if len(viaAssistant.Proposals) != 1 || viaAssistant.Proposals[0].TopicPending || viaAssistant.Proposals[0].TopicKey != "go-basics" {
		t.Fatalf("candidate=%+v, want the assistant-named accepted topic prefilled", viaAssistant.Proposals)
	}
}

// TestFinalizeNewConfigVersusOneOffDevSamples is the extraction-step contrast:
// a one-off operation stays out, while a standing configuration adopted from
// now on (including an environment upgrade phrased as "以后都…") is proposed and
// enters the replacement flow. A restatement of an existing preference is also
// proposed, so the duplicate judgement runs instead of being skipped at
// extraction.
func TestFinalizeNewConfigVersusOneOffDevSamples(t *testing.T) {
	board := ext01BoardWithPreference(t)
	profile := func(content string) model.LongTermMemory {
		entry := globalEntry("P-go", content)
		entry.ProfileClass = model.ProfileClassBackground
		return entry
	}
	newService := func(chat *memoryChat, relations GlobalRelationResolver, entries []model.LongTermMemory) *SessionFinalizeService {
		proposer, err := NewModelSessionProposer(chat)
		if err != nil {
			t.Fatal(err)
		}
		return newFinalizeService(t, fakeFinalizeBoards{content: &board}, fakeFinalizeMessages{messages: ext01Messages()},
			nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}},
			fakeFinalizeGlobalMind{entries: entries}, nil, proposer, relations, 0)
	}
	newConfigCall := `{"scope":"global","class":"background","subject":"示例代码的 Go 版本",` +
		`"proposed_content":"本地和测试环境都已升级到 Go 1.23，以后给的示例代码都要按 Go 1.23 来写。","durability":"stable",` +
		`"evidence_ids":[3],"statement_evidence_id":3}`
	restatementCall := `{"scope":"global","class":"background","subject":"示例代码的 Go 版本",` +
		`"proposed_content":"示例代码都按 Go 1.22 来写。","durability":"stable",` +
		`"evidence_ids":[3],"statement_evidence_id":3}`

	// 反例：一次性操作 → 模型拒绝 → 零卡。
	oneOff, err := newService(&memoryChat{response: toolMessage(finalizeToolReject, `{"reject_reason":"this answer only, not a standing rule"}`)},
		&scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "x"}}, nil).
		Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-dev-oneoff"}})
	if err != nil || len(oneOff.Proposals) != 0 || !oneOff.ProposerRejected {
		t.Fatalf("one-off: proposals=%d rejected=%v err=%v", len(oneOff.Proposals), oneOff.ProposerRejected, err)
	}

	// 正例：环境升级 + 以后都… → 保留为候选，进入替换流程。
	old := profile("示例代码统一用 Go 1.22 语法。")
	newConfig, err := newService(&memoryChat{response: toolMessage(finalizeToolProfile, newConfigCall)},
		&scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationReplacement, TargetMemoryID: "P-go", Reason: "same matter, new configuration"}},
		[]model.LongTermMemory{old}).
		Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-dev-newconfig"}})
	if err != nil || len(newConfig.Proposals) != 1 {
		t.Fatalf("new config: proposals=%d err=%v", len(newConfig.Proposals), err)
	}
	if newConfig.Proposals[0].Relation.Relation != GlobalRelationReplacement || len(newConfig.Proposals[0].RelatedOld) != 1 {
		t.Fatalf("candidate=%+v, want the replacement suggestion", newConfig.Proposals[0])
	}

	// 正例：同值复述 → 仍提议，由关系判定执行去重（跳过），不在提取端吞掉。
	restatement, err := newService(&memoryChat{response: toolMessage(finalizeToolProfile, restatementCall)},
		&scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationDuplicate, TargetMemoryID: "P-go", Reason: "same value restated"}},
		[]model.LongTermMemory{old}).
		Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-dev-restate"}})
	if err != nil || len(restatement.Proposals) != 0 || len(restatement.Skipped) != 1 ||
		restatement.Skipped[0].Reason != FinalizeSkipSemanticDuplicate {
		t.Fatalf("restatement: proposals=%d skipped=%+v err=%v", len(restatement.Proposals), restatement.Skipped, err)
	}

	for _, fragment := range []string{
		"never reject it as a temporary instruction",
		"Never reject a statement because it resembles something already known",
		"the relationship judgement decides whether it is a duplicate",
	} {
		if !strings.Contains(finalizeProposerPrompt, fragment) {
			t.Fatalf("the extraction prompt lost the rule %q", fragment)
		}
	}
}

// TestFinalizeTopicNamingNeedsAWholeWord tightens the naming match: "RAG" as a
// word names the topic, while "storage" - which contains "rag" as a substring -
// does not. The unique-attribution positive case is kept: a model-declared
// unique match still prefills even when the learner never named it.
func TestFinalizeTopicNamingNeedsAWholeWord(t *testing.T) {
	topics := fakeFinalizeCatalogue{topics: []Topic{{Key: "rag", Name: "RAG"}}}
	topicProposal := func(evidence uint, unique bool) SessionRawProposal {
		return SessionRawProposal{
			Scope: model.MemoryScopeTopic, Kind: FinalizeKindUnderstanding, TopicKey: "rag",
			Subject: "分片与召回", Durability: model.MemoryDurabilityStable,
			Content:     "我容易混淆分片和召回的责任边界。",
			EvidenceIDs: []uint{evidence}, StatementEvidenceID: evidence, TopicUnique: unique,
		}
	}
	newService := func(messages []SessionMessage, proposal SessionRawProposal) *SessionFinalizeService {
		return newFinalizeService(t, fakeFinalizeBoards{}, fakeFinalizeMessages{messages: messages},
			topics, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{},
			fakeFinalizeTopicMind{byTopic: map[string][]model.LongTermMemory{}},
			&scriptedProposer{proposals: []SessionRawProposal{proposal}},
			&scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "x"}}, 50)
	}

	// 正例：明确点名 RAG → 预填。
	named := []SessionMessage{
		{ID: 3, IsUser: true, Content: "这个坑记下来，属于 RAG 这个主题。", CreatedAt: finalizeAt(6)},
	}
	explicit, err := newService(named, topicProposal(3, false)).
		Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-rag-named"}})
	if err != nil {
		t.Fatalf("named extract: %v", err)
	}
	if len(explicit.Proposals) != 1 || explicit.Proposals[0].TopicPending || explicit.Proposals[0].TopicKey != "rag" {
		t.Fatalf("candidate=%+v, want the named RAG topic prefilled", explicit.Proposals)
	}

	// 反例：只出现 storage（内含 "rag" 子串）→ 不算点名，留空待归属。
	storageOnly := []SessionMessage{
		{ID: 3, IsUser: true, Content: "storage 层怎么设计我还没想清楚，这个坑记下来。", CreatedAt: finalizeAt(6)},
	}
	ambiguous, err := newService(storageOnly, topicProposal(3, false)).
		Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-storage"}})
	if err != nil {
		t.Fatalf("storage extract: %v", err)
	}
	if len(ambiguous.Proposals) != 1 || !ambiguous.Proposals[0].TopicPending || ambiguous.Proposals[0].TopicKey != "" {
		t.Fatalf("candidate=%+v, want storage not to count as naming RAG", ambiguous.Proposals)
	}

	// 正例保留：未点名但模型声明唯一 → 仍预填。
	unique, err := newService(storageOnly, topicProposal(3, true)).
		Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-rag-unique"}})
	if err != nil {
		t.Fatalf("unique extract: %v", err)
	}
	if len(unique.Proposals) != 1 || unique.Proposals[0].TopicPending || unique.Proposals[0].TopicKey != "rag" {
		t.Fatalf("candidate=%+v, want the declared-unique topic prefilled", unique.Proposals)
	}
}
