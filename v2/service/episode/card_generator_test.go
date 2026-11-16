package episode

import (
	"GopherAI/model"
	"context"
	"errors"
	"testing"
	"time"

	episodedao "GopherAI/dao/episode"
	modelcomponent "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// cardChat is the fake chat model the card generator talks to.
type cardChat struct {
	responses []*schema.Message
	tools     []*schema.ToolInfo
	calls     int
	sent      []*schema.Message
}

func (m *cardChat) Generate(_ context.Context, messages []*schema.Message, _ ...modelcomponent.Option) (*schema.Message, error) {
	m.sent = append(m.sent, messages...)
	if m.calls >= len(m.responses) {
		return nil, errors.New("unexpected model call")
	}
	response := m.responses[m.calls]
	m.calls++
	return response, nil
}

func (m *cardChat) Stream(context.Context, []*schema.Message, ...modelcomponent.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, errors.New("unused")
}

func (m *cardChat) WithTools(tools []*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	m.tools = append([]*schema.ToolInfo(nil), tools...)
	return m, nil
}

func cardToolResponse(arguments string) *schema.Message {
	return schema.AssistantMessage("", []schema.ToolCall{{ID: "card-call", Type: "function",
		Function: schema.FunctionCall{Name: cardToolName, Arguments: arguments}}})
}

// cardEvidenceFor is one session's evidence: a board-covered stretch of the
// conversation, the summary segments that compressed it, and the tail nothing
// has compressed yet.
func cardEvidenceFor(messages ...model.Message) CardEvidence {
	target := messages[len(messages)-1]
	return CardEvidence{
		UserID: 7, Session: model.Session{ID: "session-card", UserID: 7, Title: "Go 并发练习"},
		TargetMessageID: target.ID, TargetMessageAt: target.CreatedAt,
		Messages: messages,
	}
}

func TestCardGeneratorKeepsTheAssistantProductAndTheAcceptance(t *testing.T) {
	now := time.Now()
	evidence := cardEvidenceFor(
		model.Message{ID: 1, SessionID: "session-card", IsUser: true, Content: "每轮9道题吧", CreatedAt: now},
		model.Message{ID: 2, SessionID: "session-card", IsUser: false, Content: "我给你三道取消机制练习", CreatedAt: now},
		model.Message{ID: 3, SessionID: "session-card", IsUser: true, Content: "好，就这样", CreatedAt: now},
	)
	chat := &cardChat{responses: []*schema.Message{cardToolResponse(`{
		"version":1,
		"title":"Go 并发练习",
		"synopsis":"讨论了每轮题量，助手给了三道取消机制练习。",
		"topics":[{"key":"go","label":"Go 并发"}],
		"arrangements":[{"text":"每轮9道题","status":"stated","message_ids":[1]}],
		"assistant_products":[{"text":"三道取消机制练习","status":"assistant_proposed","message_ids":[2]}],
		"open_items":[]}`)}}
	generator, err := NewModelCardGenerator(chat, 4096, 1200)
	if err != nil {
		t.Fatal(err)
	}
	document, err := generator.Generate(context.Background(), evidence)
	if err != nil {
		t.Fatal(err)
	}
	// One call. The card path does not spend a second one on a semantic
	// check: every reference in the card is checked by the program.
	if chat.calls != 1 {
		t.Fatalf("the card cost %d model calls, want one", chat.calls)
	}
	if len(document.AssistantProducts) != 1 {
		t.Fatalf("the assistant's product was lost: %+v", document.AssistantProducts)
	}
	if got := document.AssistantProducts[0].Status.Normalized(); got != model.SessionCardStatusAssistantProposed {
		t.Fatalf("the assistant's product is %q, want %q", got, model.SessionCardStatusAssistantProposed)
	}
	if len(document.Arrangements) != 1 {
		t.Fatalf("the arrangement was lost: %+v", document.Arrangements)
	}
	if got, want := document.Topics[0].Key, model.NormalizeLearningTopicKey("Go 并发"); got != want {
		t.Fatalf("topic key=%q want=%q", got, want)
	}
}

func TestCardGeneratorKeepsALaterChangeApartFromTheAcceptance(t *testing.T) {
	now := time.Now()
	evidence := cardEvidenceFor(
		model.Message{ID: 1, SessionID: "session-card", IsUser: true, Content: "每轮14道题", CreatedAt: now},
		model.Message{ID: 2, SessionID: "session-card", IsUser: true, Content: "还是9道吧", CreatedAt: now},
	)
	chat := &cardChat{responses: []*schema.Message{cardToolResponse(`{
		"version":1,
		"title":"题量",
		"synopsis":"题量从14道改成9道。",
		"arrangements":[
			{"text":"每轮14道题","status":"stated","message_ids":[1]},
			{"text":"每轮9道题","status":"later_changed","message_ids":[2]}],
		"assistant_products":[],"open_items":[]}`)}}
	generator, err := NewModelCardGenerator(chat, 4096, 1200)
	if err != nil {
		t.Fatal(err)
	}
	document, err := generator.Generate(context.Background(), evidence)
	if err != nil {
		t.Fatal(err)
	}
	if len(document.Arrangements) != 2 {
		t.Fatalf("the change was not kept apart from the earlier value: %+v", document.Arrangements)
	}
	if got := document.Arrangements[1].Status.Normalized(); got != model.SessionCardStatusLaterChanged {
		t.Fatalf("the changed item is %q, want %q", got, model.SessionCardStatusLaterChanged)
	}
}

// A card whose arguments stop in the middle of the JSON was cut off by the
// output budget.
//
// It is not a protocol violation: the model wrote a whole card and the
// budget stopped it. The two need different fixes - a bigger budget for the
// first, a different answer for the second - so they are told apart.
func TestACutOffCardIsReportedAsTruncatedAndNotAsAProtocolViolation(t *testing.T) {
	now := time.Now()
	evidence := cardEvidenceFor(
		model.Message{ID: 1, SessionID: "session-card", IsUser: true, Content: "题量", CreatedAt: now},
	)
	// The arguments stop in the middle of the JSON: the document is not
	// closed.
	chat := &cardChat{responses: []*schema.Message{cardToolResponse(`{
		"version":1,
		"title":"题量",
		"synopsis":"题量。",
		"arrangements":[{"text":"每轮9道题","status":"stated","message_ids":[1]}]`)}}
	generator, err := NewModelCardGenerator(chat, 4096, 1200)
	if err != nil {
		t.Fatal(err)
	}
	_, err = generator.Generate(context.Background(), evidence)
	if !errors.Is(err, ErrCardOutputTruncated) {
		t.Fatalf("a cut-off card was reported as %v, want the truncation", err)
	}
	if errors.Is(err, ErrCardToolProtocol) {
		t.Fatalf("a cut-off card was reported as a protocol violation: %v", err)
	}
}

// A card whose arguments are JSON but do not fit the document is a protocol
// violation - the model's, not the budget's.
func TestAWellFormedCardWithAnUnknownFieldIsAProtocolViolation(t *testing.T) {
	now := time.Now()
	evidence := cardEvidenceFor(
		model.Message{ID: 1, SessionID: "session-card", IsUser: true, Content: "题量", CreatedAt: now},
	)
	chat := &cardChat{responses: []*schema.Message{cardToolResponse(`{
		"version":1,
		"title":"题量",
		"synopsis":"题量。",
		"arrangements":[{"text":"每轮9道题","status":"stated","message_ids":[1]}],
		"assistant_products":[],"open_items":[],
		"summary":"多出来的一个字段"}`)}}
	generator, err := NewModelCardGenerator(chat, 4096, 1200)
	if err != nil {
		t.Fatal(err)
	}
	_, err = generator.Generate(context.Background(), evidence)
	if !errors.Is(err, ErrCardToolProtocol) {
		t.Fatalf("a card with an unknown field was reported as %v, want the protocol violation", err)
	}
	if errors.Is(err, ErrCardOutputTruncated) {
		t.Fatalf("a card with an unknown field was reported as a truncation: %v", err)
	}
}

func TestCardGeneratorRefusesACitationItWasNeverShown(t *testing.T) {
	now := time.Now()
	evidence := cardEvidenceFor(
		model.Message{ID: 1, SessionID: "session-card", IsUser: true, Content: "题量", CreatedAt: now},
	)
	chat := &cardChat{responses: []*schema.Message{cardToolResponse(`{
		"version":1,
		"title":"题量",
		"synopsis":"题量。",
		"arrangements":[{"text":"每轮9道题","status":"stated","message_ids":[99]}],
		"assistant_products":[],"open_items":[]}`)}}
	generator, err := NewModelCardGenerator(chat, 4096, 1200)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := generator.Generate(context.Background(), evidence); !errors.Is(err, ErrCardSourceBoundary) {
		t.Fatalf("an invented citation was accepted: %v", err)
	}
}

func TestCardGeneratorDropsAnEntryWithAnUnknownStatus(t *testing.T) {
	now := time.Now()
	evidence := cardEvidenceFor(
		model.Message{ID: 1, SessionID: "session-card", IsUser: true, Content: "题量", CreatedAt: now},
	)
	chat := &cardChat{responses: []*schema.Message{cardToolResponse(`{
		"version":1,
		"title":"题量",
		"synopsis":"题量。",
		"arrangements":[
			{"text":"看不懂","status":"maybe","message_ids":[1]},
			{"text":"每轮9道题","status":"stated","message_ids":[1]}],
		"assistant_products":[],"open_items":[]}`)}}
	generator, err := NewModelCardGenerator(chat, 4096, 1200)
	if err != nil {
		t.Fatal(err)
	}
	document, err := generator.Generate(context.Background(), evidence)
	if err != nil {
		t.Fatal(err)
	}
	if len(document.Arrangements) != 1 {
		t.Fatalf("the unreadable entry was kept: %+v", document.Arrangements)
	}
	if document.Arrangements[0].Text != "每轮9道题" {
		t.Fatalf("the wrong entry was kept: %+v", document.Arrangements)
	}
}

// cardLoaderFake hands the projector one prepared evidence.
type cardLoaderFake struct{ evidence CardEvidence }

func (l cardLoaderFake) LoadCardEvidence(context.Context, int64, string, uint) (CardEvidence, error) {
	return l.evidence, nil
}

// cardGeneratorFake is a generator that answers or fails.
type cardGeneratorFake struct {
	document model.SessionCardDocument
	err      error
	calls    int
}

func (g *cardGeneratorFake) Generate(context.Context, CardEvidence) (model.SessionCardDocument, error) {
	g.calls++
	if g.err != nil {
		return model.SessionCardDocument{}, g.err
	}
	return g.document, nil
}

// cardRepositoryFake remembers what was written.
type cardRepositoryFake struct {
	existing *model.EpisodeMemory
	saved    *model.EpisodeMemory
	saves    int
	version  uint64
}

func (r *cardRepositoryFake) GetBySessionKind(context.Context, int64, string,
	model.EpisodeContentKind) (*model.EpisodeMemory, error) {
	if r.existing == nil {
		return nil, episodedao.ErrEpisodeNotFound
	}
	return r.existing, nil
}

func (r *cardRepositoryFake) Save(_ context.Context, card *model.EpisodeMemory, expectedVersion uint64) error {
	r.saves++
	r.version = expectedVersion
	clone := *card
	// The real repository stamps the version before it validates and writes
	// the row, and a card that carries no version is not a stored card.
	if expectedVersion == 0 {
		clone.Version = 1
	} else {
		clone.Version = expectedVersion + 1
	}
	card.Version = clone.Version
	r.saved = &clone
	return nil
}

func validCardDocument() model.SessionCardDocument {
	return model.SessionCardDocument{
		Version:  model.SessionCardContentVersion,
		Title:    "Go 并发练习",
		Synopsis: "讨论了每轮题量。",
		Arrangements: []model.SessionCardEntry{
			{Text: "每轮9道题", Status: model.SessionCardStatusStated, MessageIDs: []uint{1}},
		},
	}
}

func TestCardProjectorKeepsThePreviousCardWhenTheGenerationFails(t *testing.T) {
	now := time.Now()
	evidence := cardEvidenceFor(model.Message{ID: 1, SessionID: "session-card", IsUser: true, Content: "题量", CreatedAt: now})
	previous := &model.EpisodeMemory{
		ID: "card-1", UserID: 7, SessionID: "session-card",
		ContentKind: model.EpisodeContentSessionCard,
		Title:       "上一张卡片", Card: validCardDocument(),
		SearchText: "上一张卡片", SourceLastMessageID: 1,
		LastMessageAt: now, Version: 3, Status: model.EpisodeStatusActive,
		IndexStatus: model.EpisodeIndexIndexed,
	}
	generator := &cardGeneratorFake{err: errors.New("the model call failed")}
	repository := &cardRepositoryFake{existing: previous}
	projector, err := NewCardProjector(cardLoaderFake{evidence: evidence}, generator, repository)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := projector.Project(context.Background(), 7, "session-card", 5); err == nil {
		t.Fatal("a failed generation was reported as a card")
	}
	// Nothing was written, so the previous card and its real coverage
	// watermark are still there.
	if repository.saves != 0 {
		t.Fatalf("a failed generation wrote %d row(s)", repository.saves)
	}
	if previous.SourceLastMessageID != 1 || previous.Version != 3 {
		t.Fatalf("the previous card moved: watermark=%d version=%d", previous.SourceLastMessageID, previous.Version)
	}
}

func TestCardProjectorDoesNotRegenerateAtAWatermarkItAlreadyCovers(t *testing.T) {
	now := time.Now()
	evidence := cardEvidenceFor(model.Message{ID: 1, SessionID: "session-card", IsUser: true, Content: "题量", CreatedAt: now})
	previous := &model.EpisodeMemory{
		ID: "card-1", UserID: 7, SessionID: "session-card",
		ContentKind: model.EpisodeContentSessionCard,
		Title:       "上一张卡片", Card: validCardDocument(),
		SearchText: "上一张卡片", SourceLastMessageID: 9,
		LastMessageAt: now, Version: 2, Status: model.EpisodeStatusActive,
		IndexStatus: model.EpisodeIndexIndexed,
	}
	generator := &cardGeneratorFake{document: validCardDocument()}
	repository := &cardRepositoryFake{existing: previous}
	projector, err := NewCardProjector(cardLoaderFake{evidence: evidence}, generator, repository)
	if err != nil {
		t.Fatal(err)
	}
	card, err := projector.Project(context.Background(), 7, "session-card", 9)
	if err != nil {
		t.Fatal(err)
	}
	if generator.calls != 0 {
		t.Fatalf("the model was asked again at a watermark the card already covers")
	}
	if repository.saves != 0 {
		t.Fatalf("the card was rewritten at the same watermark")
	}
	if card.ID != "card-1" {
		t.Fatalf("the card identity changed: %s", card.ID)
	}
}

func TestCardProjectorWritesACardRow(t *testing.T) {
	now := time.Now()
	evidence := cardEvidenceFor(model.Message{ID: 1, SessionID: "session-card", IsUser: true, Content: "题量", CreatedAt: now})
	evidence.SummaryWatermark = 0
	generator := &cardGeneratorFake{document: validCardDocument()}
	repository := &cardRepositoryFake{}
	projector, err := NewCardProjector(cardLoaderFake{evidence: evidence}, generator, repository)
	if err != nil {
		t.Fatal(err)
	}
	card, err := projector.Project(context.Background(), 7, "session-card", 1)
	if err != nil {
		t.Fatal(err)
	}
	if card.ContentKind.Normalized() != model.EpisodeContentSessionCard {
		t.Fatalf("the row says %q", card.ContentKind)
	}
	if card.SourceLastMessageID != 1 {
		t.Fatalf("the card covers %d, want 1", card.SourceLastMessageID)
	}
	if card.Title == "" || card.SearchText == "" {
		t.Fatalf("the card is not searchable: %+v", card)
	}
	if err := card.Validate(); err != nil {
		t.Fatalf("the written card is invalid: %v", err)
	}
	if repository.saved == nil || repository.saved.ContentKind.Normalized() != model.EpisodeContentSessionCard {
		t.Fatalf("the row was not written as a card: %+v", repository.saved)
	}
}

func TestCardDocumentRejectsAnUnknownField(t *testing.T) {
	if _, err := model.DecodeSessionCardDocument([]byte(`{"version":1,"title":"a","synopsis":"b","surprise":1}`)); err == nil {
		t.Fatal("a document with an unknown field was accepted")
	}
	if _, err := model.DecodeSessionCardDocument([]byte(`{"version":1,"title":"a","synopsis":"b"}{"version":1}`)); err == nil {
		t.Fatal("a document with trailing JSON was accepted")
	}
}

func TestSessionCardEntryMustCiteSomething(t *testing.T) {
	_, err := model.DecodeSessionCardDocument([]byte(`{"version":1,"title":"a","synopsis":"b","arrangements":[{"text":"没有来源"}]}`))
	if err == nil {
		t.Fatal("an entry that cites nothing was accepted")
	}
}

// The worker is shared by both paths, so the job row is the only place that
// says which path produced it.
func TestCardFailuresGetTheirOwnJobCodes(t *testing.T) {
	cases := map[error]string{
		ErrCardSourceBoundary:  "card_source_boundary",
		ErrCardToolProtocol:    "card_tool_protocol",
		ErrCardOutputTruncated: "card_output_truncated",
		ErrEmptyCard:           "card_empty",
		ErrCardInputTooLarge:   "card_input_too_large",
		ErrCardSourceNotFound:  "card_source_not_found",
	}
	for err, want := range cases {
		if got := projectionErrorCode(err); got != want {
			t.Fatalf("the job code for %v is %q, want %q", err, got, want)
		}
	}
}
