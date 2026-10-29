package memory

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/embedding"
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// EXP-002B B1 tried four ways to improve candidate generation without a model
// call — character n-grams, field weighting, identifier splitting, and topic-key
// boosting. All were null: nine of the twelve paired comparisons did not even
// move in the right direction. Only expansion helped, and the reason is visible
// in the failures it fixed: questions name an identifier the note never uses.
// Asking `HS256` cannot reach a note titled "JWT 身份链路" by any amount of
// tokenizing, because the bridging term is in neither text. A model supplies it.
//
// The prompt is the one B1 measured. Changing its wording changes the result it
// was validated against, so it is kept verbatim rather than tidied.
const expansionPrompt = `你是检索关键词扩写器。用户提出一个问题，你要输出这个问题的答案最可能包含的技术关键词。

规则：
1. 只输出关键词，用空格分隔，不要解释，不要标点。
2. 输出 5 到 10 个词。
3. 包含：问题中出现的专有名词原样保留；该名词所属的技术领域词；同义或等价的中英文写法。
4. 不要输出"问题""答案""什么""为什么"这类无检索价值的词。

问题：%s`

// ModelQueryExpander adds one model call to the request path. Two limits keep
// that affordable: a hard timeout well below the agent's own budget, and a cap
// on how much text is returned, since the output is concatenated into a search
// query rather than shown to anyone.
type ModelQueryExpander struct {
	model   einomodel.BaseChatModel
	timeout time.Duration
	maxRune int
}

func NewModelQueryExpander(chatModel einomodel.BaseChatModel, timeout time.Duration) (*ModelQueryExpander, error) {
	if chatModel == nil {
		return nil, fmt.Errorf("query expander requires a chat model")
	}
	if timeout <= 0 || timeout > 10*time.Second {
		return nil, fmt.Errorf("query expander timeout must be positive and at most 10s")
	}
	return &ModelQueryExpander{model: chatModel, timeout: timeout, maxRune: 200}, nil
}

func (e *ModelQueryExpander) Expand(ctx context.Context, question string) (string, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return "", fmt.Errorf("expand empty question")
	}
	// The timeout is enforced here rather than left to the caller because this
	// call sits between the user pressing send and any retrieval starting.
	callCtx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	out, err := e.model.Generate(callCtx, []*schema.Message{
		schema.UserMessage(fmt.Sprintf(expansionPrompt, question)),
	})
	if err != nil {
		return "", fmt.Errorf("expand query: %w", err)
	}
	// Whatever the model returns is treated as terms, never as instructions:
	// it is joined into a search string and never re-enters a prompt.
	expansion := strings.Join(strings.Fields(out.Content), " ")
	if runes := []rune(expansion); len(runes) > e.maxRune {
		expansion = string(runes[:e.maxRune])
	}
	return expansion, nil
}

// EinoQueryEmbedder adapts the embedding component the knowledge base already
// uses, so memory and knowledge vectors are produced by the same model. Mixing
// two embedding models would make the cosine floor calibrated in B1 meaningless.
type EinoQueryEmbedder struct {
	embedder  embedding.Embedder
	dimension int
}

func NewEinoQueryEmbedder(embedder embedding.Embedder, dimension int) (*EinoQueryEmbedder, error) {
	if embedder == nil || dimension <= 0 {
		return nil, fmt.Errorf("query embedder requires an embedder and a positive dimension")
	}
	return &EinoQueryEmbedder{embedder: embedder, dimension: dimension}, nil
}

func (e *EinoQueryEmbedder) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	vectors, err := e.embedder.EmbedStrings(ctx, []string{text})
	if err != nil {
		return nil, fmt.Errorf("embed memory query: %w", err)
	}
	if len(vectors) != 1 {
		return nil, fmt.Errorf("embed memory query: provider returned %d vectors", len(vectors))
	}
	return ToFloat32Vector(vectors[0], e.dimension)
}

// ToFloat32Vector narrows the provider's float64 output to the float32 the
// index stores. A dimension mismatch is rejected rather than padded: a short
// vector would still search, just against a silently different space.
func ToFloat32Vector(values []float64, dimension int) ([]float32, error) {
	if len(values) != dimension {
		return nil, fmt.Errorf("vector has %d dimensions, index requires %d", len(values), dimension)
	}
	vector := make([]float32, dimension)
	for index, value := range values {
		vector[index] = float32(value)
	}
	return vector, nil
}
