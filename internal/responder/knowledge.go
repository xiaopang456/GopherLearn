package responder

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/retrieval"
)

// TitleSource resolves document titles for citations. It is a narrow interface so
// this package does not depend on the knowledge store.
type TitleSource interface {
	DocumentTitles(ctx context.Context, userID string, documentIDs []string) (map[string]string, error)
}

// Passage is one retrieved chunk together with the citation the client will see.
type Passage struct {
	Source domain.Source
	Text   string
}

// Generation is everything a grounded answer is built from: the question, the
// passages retrieval admitted, and the recent conversation the service trimmed to
// its budget. Passing one struct instead of loose parameters keeps the history and
// the evidence attached to the same turn they belong to.
type Generation struct {
	Query    string
	Passages []Passage
	History  []Turn
	// Summary is the session's sealed rolling summary, passed straight through from
	// the turn request: the grounded path must recall the same conversation the
	// direct and agent paths recall.
	Summary string
}

// Generator turns retrieved passages plus the question into answer text. Keeping
// it behind an interface is what lets a real model replace the local extractive
// implementation without touching retrieval or the HTTP layer.
type Generator interface {
	Generate(ctx context.Context, generation Generation) (string, error)
}

// StreamingGenerator is implemented by generators that can produce text as it
// arrives. KnowledgeStrategy.Stream prefers it, because a model that streams must
// feed the caller's SSE writer incrementally rather than after the fact; a
// generator without streaming still works and is chunked deterministically.
type StreamingGenerator interface {
	Generator
	GenerateStream(ctx context.Context, generation Generation, emit func(string) error) (string, error)
}

// NoEvidenceAnswer is the whole answer for a grounded turn that retrieved
// nothing. It is a constant rather than generator prose so that "nothing was
// found" cannot become whatever a model felt like saying that day, and so the
// message never depends on a provider being reachable.
const NoEvidenceAnswer = "知识库中没有检索到与这个问题相关的内容。"

// DefaultGroundedPassages bounds how many chunks one answer may cite.
const DefaultGroundedPassages = 5

// KnowledgeStrategy answers a knowledge_qa turn: it retrieves inside the
// collections the caller was authorized to read, cites exactly what retrieval
// returned, and delegates the wording to an injected generator.
//
// Retrieval here is real and deterministic. Generation is not a model unless a
// Generator backed by one is injected.
type KnowledgeStrategy struct {
	retriever retrieval.Retriever
	titles    TitleSource
	generator Generator
	limit     int
}

func NewKnowledgeStrategy(retriever retrieval.Retriever, titles TitleSource, generator Generator) *KnowledgeStrategy {
	return &KnowledgeStrategy{retriever: retriever, titles: titles, generator: generator, limit: DefaultGroundedPassages}
}

// WithPassageLimit overrides how many chunks one answer cites.
func (s *KnowledgeStrategy) WithPassageLimit(limit int) *KnowledgeStrategy {
	if limit > 0 {
		s.limit = limit
	}
	return s
}

// Strategy implements responder.Strategy.
func (s *KnowledgeStrategy) Strategy() domain.AnswerStrategy { return domain.AnswerStrategyKnowledgeQA }

var _ Strategy = (*KnowledgeStrategy)(nil)

var errNotConfigured = ErrStrategyNotConfigured

// Respond implements responder.Strategy.
func (s *KnowledgeStrategy) Respond(ctx context.Context, request Request) (Answer, error) {
	passages, sources, err := s.ground(ctx, request)
	if err != nil {
		return Answer{}, err
	}
	if len(passages) == 0 {
		// Nothing was admitted, so nothing is asked of the model: an unavailable
		// provider cannot block the honest answer, and a model cannot be tempted
		// to fill a gap there is no evidence for.
		return Answer{Content: NoEvidenceAnswer}, nil
	}
	content, err := s.generator.Generate(ctx, Generation{Query: request.Content, Passages: passages, History: request.History, Summary: request.Summary})
	if err != nil {
		return Answer{}, err
	}
	return Answer{Content: content, Sources: sources}, nil
}

// Stream emits the same content Respond returns. A generator that can stream
// drives the SSE writer directly, so a real model's deltas reach the client as
// they arrive; the extractive generator has no deltas to offer, so its text is
// chunked deterministically the way it always was.
func (s *KnowledgeStrategy) Stream(ctx context.Context, request Request, emit func(string) error) (Answer, error) {
	passages, sources, err := s.ground(ctx, request)
	if err != nil {
		return Answer{}, err
	}
	generation := Generation{Query: request.Content, Passages: passages, History: request.History, Summary: request.Summary}
	if len(passages) == 0 {
		if err := emitChunks(ctx, NoEvidenceAnswer, emit); err != nil {
			return Answer{}, err
		}
		return Answer{Content: NoEvidenceAnswer}, nil
	}
	if streaming, ok := s.generator.(StreamingGenerator); ok {
		content, err := streaming.GenerateStream(ctx, generation, emit)
		if err != nil {
			return Answer{}, err
		}
		return Answer{Content: content, Sources: sources}, nil
	}
	content, err := s.generator.Generate(ctx, generation)
	if err != nil {
		return Answer{}, err
	}
	if err := emitChunks(ctx, content, emit); err != nil {
		return Answer{}, err
	}
	return Answer{Content: content, Sources: sources}, nil
}

func (s *KnowledgeStrategy) ground(ctx context.Context, request Request) ([]Passage, []domain.Source, error) {
	if s.retriever == nil || s.generator == nil {
		return nil, nil, errNotConfigured
	}
	if !request.KnowledgeEnabled || len(request.KnowledgeCollectionIDs) == 0 {
		// The service resolves the strategy from the stored session and this
		// turn's options, so reaching this method without a scope is a bug, not
		// a bad request.
		return nil, nil, errors.New("responder: knowledge strategy invoked without an authorized collection scope")
	}
	results, err := s.retriever.Search(ctx, retrieval.Query{
		UserID:        request.UserID,
		Text:          request.Content,
		CollectionIDs: request.KnowledgeCollectionIDs,
		TopK:          s.limit,
	})
	if err != nil {
		if errors.Is(err, retrieval.ErrNoCandidates) || errors.Is(err, retrieval.ErrEmptyQuery) {
			// An owned but empty collection is a legitimate state, and so is a
			// collection whose chunks nothing in it matched the question: the
			// honest answer is that nothing was found, with no citations
			// attached. Anything else is a real retrieval failure and must
			// surface instead of answering from thin air.
			return nil, nil, nil
		}
		return nil, nil, err
	}
	documentIDs := make([]string, 0, len(results))
	for _, result := range results {
		documentIDs = append(documentIDs, result.Chunk.DocumentID)
	}
	titles := map[string]string{}
	if s.titles != nil {
		resolved, err := s.titles.DocumentTitles(ctx, request.UserID, documentIDs)
		if err != nil {
			return nil, nil, err
		}
		titles = resolved
	}
	passages := make([]Passage, 0, len(results))
	sources := make([]domain.Source, 0, len(results))
	for _, result := range results {
		source := domain.BuildSource(titles[result.Chunk.DocumentID], result.Chunk)
		passages = append(passages, Passage{Source: source, Text: result.Chunk.Text})
		sources = append(sources, source)
	}
	return passages, sources, nil
}

func emitChunks(ctx context.Context, content string, emit func(string) error) error {
	if content == "" {
		return nil
	}
	runes := []rune(content)
	for offset := 0; offset < len(runes); {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		end := offset + 16
		if end > len(runes) {
			end = len(runes)
		}
		if err := emit(string(runes[offset:end])); err != nil {
			return err
		}
		offset = end
	}
	return nil
}

// ExcerptGenerator is the deterministic local Generator. It quotes retrieved
// passages and attributes them; it never writes prose that the sources do not
// contain.
//
// It stays in the codebase for tests and offline development, where a reproducible
// answer matters more than a fluent one. It is not the production generator for a
// deployment that configured a model: ModelGenerator replaces it there.
type ExcerptGenerator struct {
	// PerPassageRunes caps each quoted passage. Zero selects 160 runes.
	PerPassageRunes int
}

func (g ExcerptGenerator) Generate(ctx context.Context, generation Generation) (string, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return "", err
		}
	}
	passages := generation.Passages
	if len(passages) == 0 {
		return NoEvidenceAnswer, nil
	}
	limit := g.PerPassageRunes
	if limit <= 0 {
		limit = 160
	}
	var builder strings.Builder
	builder.WriteString("检索到 " + strconv.Itoa(len(passages)) + " 条知识库片段（抽取式回答，本阶段未接入生成模型）：\n")
	for index, passage := range passages {
		builder.WriteString(strconv.Itoa(index+1) + ". " + passage.Source.Title)
		if passage.Source.Section != "" {
			builder.WriteString(" › " + passage.Source.Section)
		}
		builder.WriteString("\n   ")
		builder.WriteString(strings.ReplaceAll(domain.Snippet(passage.Text, limit), "\n", "\n   "))
		builder.WriteString("\n")
	}
	builder.WriteString("问题：" + strings.TrimSpace(generation.Query))
	return builder.String(), nil
}

var _ Generator = ExcerptGenerator{}
