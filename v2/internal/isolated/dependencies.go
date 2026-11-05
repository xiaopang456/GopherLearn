package isolated

import (
	"GopherAI/common/aihelper"
	"GopherAI/common/rag"
	"GopherAI/config"
	agentService "GopherAI/service/agent"
	episodeService "GopherAI/service/episode"
	knowledgeService "GopherAI/service/knowledge"
	userService "GopherAI/service/user"
	"context"
	embeddingArk "github.com/cloudwego/eino-ext/components/embedding/ark"
	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/embedding"
	modelcomponent "github.com/cloudwego/eino/components/model"
	redisclient "github.com/redis/go-redis/v9"
	"net/http"
	"time"
)

type ChunkStore interface {
	rag.ChunkVectorRepository
	rag.ActiveChunkSearcher
	knowledgeService.ChunkCleaner
}
type Dependencies struct {
	Registration   *userService.RegistrationExternal
	Clock          func() time.Time
	Infrastructure Infrastructure
	ChatModel      func(context.Context, *Settings) (modelcomponent.ToolCallingChatModel, error)
	Embedder       func(context.Context, *config.Config) (embedding.Embedder, error)
	Chunks         func(*redisclient.Client, int) (ChunkStore, error)
}

func ProductionDependencies(settings *Settings) Dependencies {
	return Dependencies{
		Infrastructure: ProductionInfrastructure(),
		ChatModel: func(ctx context.Context, s *Settings) (modelcomponent.ToolCallingChatModel, error) {
			// The model's own thinking switch is the one every call inherits
			// unless it carries its own. Calls that carry one replace this
			// table, and calls that carry none - the summary, the episode
			// projection, the support verifier, the relation classifier - keep
			// it, so it has to name the provider's dialect.
			dialect, err := config.NormalizeChatThinkingDialect(s.Config.RagModelConfig.RagChatThinkingDialect)
			if err != nil {
				return nil, err
			}
			return openai.NewChatModel(ctx, &openai.ChatModelConfig{BaseURL: s.Entry.ChatBaseURL, Model: s.Entry.ChatModel, APIKey: s.Entry.ChatKey, ExtraFields: aihelper.ThinkingExtraFields(dialect, false)})
		},
		Embedder: func(ctx context.Context, c *config.Config) (embedding.Embedder, error) {
			return embeddingArk.NewEmbedder(ctx, &embeddingArk.EmbeddingConfig{BaseURL: c.RagEmbeddingBaseURL, Model: c.RagEmbeddingModel, APIKey: c.RagEmbeddingAPIKey})
		},
		Chunks: func(client *redisclient.Client, dim int) (ChunkStore, error) {
			return rag.NewRedisChunkRepository(client, dim)
		},
	}
}

type Application struct {
	*Lifecycle
	Handler http.Handler
	Agent   *agentService.Service
	Runner  *agentService.Runner
	Context *agentService.DatabaseContextBuilder
	// The same original service used by native handlers and document_read.
	Documents *knowledgeService.DocumentService
	// The original derived index used by this assembly, for deletion cleanup.
	EpisodeIndex episodeService.EpisodeIndex
}
