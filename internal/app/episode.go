package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/xiaopang456/GopherLearn/internal/agent"
	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/repository/redisindex"
	"github.com/xiaopang456/GopherLearn/internal/retrieval"
	"github.com/xiaopang456/GopherLearn/internal/service"
)

// The episodic memory assembly. It is a setup object rather than a plain
// struct because the tools need the chat model, and the chat model is built
// inside Assemble: the binary hands the setup to Assemble's options, Assemble
// calls Install once with the model it built, and the binary collects the
// running system afterwards. One installation per process is the invariant -
// a second one would build a second ledger, and the per-turn evidence boundary
// would quietly split in two.

// EpisodeTools is the tool set the episodic system installs into the agent
// registry. Both tools are installed together or not at all: the search tool
// shows message ids, and the read tool is the promise that those ids mean
// something.
type EpisodeTools struct {
	Searcher      agent.EpisodeSearcher
	HistoryReader agent.EpisodeHistoryReader
}

// EpisodeStack is what the binary keeps after installation.
type EpisodeStack struct {
	System      *service.EpisodeSystem
	Publisher   service.EpisodeCompletionPublisher
	StartupLine string
	// WarmStart rebuilds the derived view from the authority. It is nil for a
	// view that survives restarts on its own (Redis).
	WarmStart func(ctx context.Context) (int, error)
	Close     func() error
}

// EpisodeSetup installs the episodic system into an Assemble call.
type EpisodeSetup struct {
	repo     repository.Repository
	mode     RetrievalMode
	semantic struct {
		Embedding EmbeddingSettings
		Redis     RedisSettings
	}
	config service.EpisodeConfig
	system *service.EpisodeSystem
	line   string
	close  func() error
	warm   func(ctx context.Context) (int, error)
}

// NewEpisodeSetup resolves the episodic configuration. enabled is false unless
// the environment asked for the feature; a disabled setup is never handed to
// Assemble, so the tools and the trigger both stay absent.
func NewEpisodeSetup(repo repository.Repository, retrievalConfig RetrievalConfig, lookup func(string) string) (*EpisodeSetup, bool, error) {
	enabled, config, err := service.EpisodeConfigFromEnvironment(lookup)
	if err != nil {
		return nil, false, err
	}
	if !enabled {
		return nil, false, nil
	}
	setup := &EpisodeSetup{repo: repo, mode: retrievalConfig.Mode, config: config}
	setup.semantic.Embedding = retrievalConfig.Semantic.Embedding
	setup.semantic.Redis = retrievalConfig.Semantic.Redis
	return setup, true, nil
}

// Install builds the system around the chat model Assemble selected. It is
// called at most once per process; a second call would be a wiring bug and is
// refused rather than tolerated.
func (s *EpisodeSetup) Install(chat model.ToolChatModel) (EpisodeTools, error) {
	if s.system != nil {
		return EpisodeTools{}, fmt.Errorf("the episodic system is already installed in this process")
	}
	embedder, view, closeView, err := s.buildView()
	if err != nil {
		return EpisodeTools{}, err
	}
	system, err := service.NewEpisodeSystem(s.repo, chat, embedder, view, service.EpisodeSystemConfig{
		Worker: service.EpisodeWorkerConfig{
			WorkerCount:       s.config.WorkerCount,
			QueueSize:         s.config.QueueSize,
			ScanInterval:      s.config.ScanInterval,
			ReconcileInterval: s.config.ReconcileInterval,
			TaskTimeout:       s.config.TaskTimeout,
			Lease:             s.config.Lease,
			RecoveryInterval:  s.config.RecoveryInterval,
			RetryDelay:        s.config.RetryDelay,
			MaxAttempts:       s.config.MaxAttempts,
		},
		QuietWindow:     s.config.QuietWindow,
		MaxMessages:     s.config.MaxMessages,
		MaxInputTokens:  s.config.MaxInputTokens,
		MaxOutputTokens: s.config.MaxOutputTokens,
		RecallMode:      s.config.RecallMode,
		AdmissionMode:   s.config.AdmissionMode,
	})
	if err != nil {
		_ = closeView()
		return EpisodeTools{}, err
	}
	s.system = system
	s.close = closeView
	s.line = "episodes=on worker=" + strconvItoa(s.config.WorkerCount) +
		" quiet_window=" + s.config.QuietWindow.String() +
		" max_attempts=" + strconvItoa(s.config.MaxAttempts) +
		" view=" + s.viewName()
	return EpisodeTools{
		Searcher:      episodeSearcher{system: system},
		HistoryReader: episodeHistoryReader{system: system},
	}, nil
}

// buildView decides the embedder and the derived view from the retrieval mode,
// mirroring the knowledge stack's two-mode rule: deterministic builds the
// process-local view over the deterministic embedder; semantic_redis builds a
// real RediSearch view over the same embedding endpoint the knowledge stack
// uses. A missing or unusable piece stops startup rather than degrading.
func (s *EpisodeSetup) buildView() (retrieval.Embedder, service.EpisodeVectorIndex, func() error, error) {
	if s.mode == RetrievalModeDeterministic {
		embedder := retrieval.NewDeterministicEmbedder(0)
		view := service.NewMemoryEpisodeVectorIndex()
		return embedder, view, func() error { return nil }, nil
	}
	embedder, err := retrieval.NewHTTPEmbedder(retrieval.EmbeddingsOptions{
		BaseURL: s.semantic.Embedding.BaseURL, APIKey: s.semantic.Embedding.APIKey,
		Model: s.semantic.Embedding.Model, Dimensions: s.semantic.Embedding.Dimensions,
		Timeout: s.semantic.Embedding.Timeout, BatchSize: s.semantic.Embedding.BatchSize,
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("episode embedding configuration is invalid: %v", err)
	}
	index, err := redisindex.NewEpisodeIndex(redisindex.Options{
		Address: s.semantic.Redis.Address, Password: s.semantic.Redis.Password,
		Database: s.semantic.Redis.Database,
		// The episode view always uses its own index name, so the two
		// retrieval views cannot drift into each other's fields. The knowledge
		// index's name override does not leak here, and the episode view has no
		// override of its own to keep the namespace unambiguous.
		Dimensions: embedder.Dimensions(),
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("episode redis configuration is invalid: %v", err)
	}
	view := redisEpisodeView{index: index}
	return embedder, view, index.Close, nil
}

func (s *EpisodeSetup) viewName() string {
	if s.mode == RetrievalModeDeterministic {
		return "in-process"
	}
	return "redis:" + redisindex.DefaultEpisodeIndexName
}

// Stack returns the running system for the binary's startup and shutdown. It
// is only available after a successful Install.
func (s *EpisodeSetup) Stack() (*EpisodeStack, error) {
	if s.system == nil {
		return nil, fmt.Errorf("the episodic system has not been installed")
	}
	stack := &EpisodeStack{
		System: s.system,
		// The publisher the conversation service sees is scoped to the source
		// project's trigger surface: only learning-agent sessions project. The
		// reconcile scan filters the same way, so a skipped publish for a chat
		// session is not a missed projection - that session is out of scope.
		Publisher:   agentScopedPublisher{repo: s.repo, inner: s.system.Publisher()},
		StartupLine: s.line,
		Close:       s.close,
	}
	// Only the process-local view needs a warm start; a Redis-backed view
	// keeps its content across restarts and is repaired by reindexing instead.
	if s.mode == RetrievalModeDeterministic {
		stack.WarmStart = func(ctx context.Context) (int, error) {
			return s.system.WarmStart(ctx)
		}
	}
	return stack, nil
}

// agentScopedPublisher gates the episodic trigger by session mode. The source
// project publishes projection work only from its agent service - an ordinary
// chat turn never enqueues anything - and its reconcile scan filters the same
// set. This wrapper restores that boundary at the one seam this project owns:
// the publisher value the conversation service is wired with. A session whose
// mode cannot be read is not published (fail-closed): the reconcile scan
// repairs any genuinely missed agent turn, while forwarding on an unknown
// session could enqueue work for a session that is out of scope.
type agentScopedPublisher struct {
	repo  repository.Repository
	inner service.EpisodeCompletionPublisher
}

// PublishConversationCompleted implements service.EpisodeCompletionPublisher.
func (a agentScopedPublisher) PublishConversationCompleted(ctx context.Context, userID, sessionID, latestMessageID string) error {
	session, err := a.repo.GetSession(ctx, userID, sessionID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			// The session is gone or foreign; there is nothing to project.
			return nil
		}
		return fmt.Errorf("episode trigger could not read the session: %w", err)
	}
	if session.Mode != domain.SessionModeLearningAgent {
		return nil
	}
	return a.inner.PublishConversationCompleted(ctx, userID, sessionID, latestMessageID)
}

// --- Agent adapters ----------------------------------------------------------
//
// The adapters are the whole mapping between what the system stores and what a
// tool may show a model: card identity, owner, version and watermark stop here,
// and the card's words plus the message ids its entries cite cross over. The
// ids are the one deliberate exception, because the read tool needs them - and
// they only mean something inside the server-side ledger the search registered
// them into.

type episodeSearcher struct {
	system *service.EpisodeSystem
}

// RecallEpisodes implements agent.EpisodeSearcher.
func (a episodeSearcher) RecallEpisodes(ctx context.Context, userID, sessionID, turnID, query string) (agent.EpisodeReading, error) {
	cards, err := a.system.RecallForTurn(ctx, userID, sessionID, turnID, query)
	if err != nil {
		return agent.EpisodeReading{}, err
	}
	reading := agent.EpisodeReading{
		ScopeNote: "检索范围：该用户其他会话的经历记录（当前会话除外）。",
	}
	for _, card := range cards {
		reading.Cards = append(reading.Cards, agent.EpisodeEntry{
			EpisodeID:      card.ID,
			EpisodeVersion: card.Version,
			Title:          card.Title,
			Synopsis:       card.Synopsis,
			Topics:         append([]string(nil), card.Topics...),
			Assertions:     episodeCardAssertions(card),
			Citation:       episodeCitation(card),
			OccurredAt:     card.OccurredAt.Format("2006-01-02"),
		})
	}
	reading.AuthorizeDisplayed = func(displayed []agent.EpisodeAuthorization) error {
		converted := make([]service.EpisodeDisplayedAuthorization, 0, len(displayed))
		for _, item := range displayed {
			converted = append(converted, service.EpisodeDisplayedAuthorization{EpisodeID: item.EpisodeID, EpisodeVersion: item.EpisodeVersion, MessageIDs: item.MessageIDs})
		}
		return a.system.AuthorizeDisplayed(ctx, userID, sessionID, turnID, converted)
	}
	return reading, nil
}

// episodeCardAssertions maps the stored card's assertion slots into the
// structured lines the tool renders itself. The ids stay per-assertion data,
// not pre-formatted text: the tool owns where each id lands in the observation,
// which is what makes displayed-id bookkeeping exact.
func episodeCardAssertions(card service.EpisodeCard) []agent.EpisodeAssertionLine {
	var lines []agent.EpisodeAssertionLine
	slot := func(label string, entries []service.EpisodeCardEntry) {
		for _, entry := range entries {
			lines = append(lines, agent.EpisodeAssertionLine{
				Label:      label,
				Text:       entry.Text,
				MessageIDs: append([]string(nil), entry.MessageIDs...),
			})
		}
	}
	slot("决定", card.Decisions)
	slot("遗留", card.OpenLoops)
	slot("纠正", card.Corrections)
	slot("偏好", card.Preferences)
	slot("认识", card.Experiences)
	return lines
}

// episodeCitation builds the citation one recalled card earns. The source id is
// stable for the card so a client can deduplicate repeated references, and the
// snippet is the card's own synopsis - the words the model was actually shown.
func episodeCitation(card service.EpisodeCard) domain.Source {
	return domain.Source{
		SourceID: "src-episode-" + card.ID + "-v" + strconvItoa64(card.Version),
		Title:    card.Title,
		Section:  "情景经历",
		Snippet:  domain.Snippet(card.Synopsis, 280),
	}
}

func strconvItoa64(v uint64) string { return fmt.Sprintf("%d", v) }

type episodeHistoryReader struct {
	system *service.EpisodeSystem
}

// ReadEpisodeHistory implements agent.EpisodeHistoryReader.
func (a episodeHistoryReader) ReadEpisodeHistory(ctx context.Context, userID, sessionID, turnID string, messageIDs []string) (agent.HistoryReading, error) {
	messages, refusals, err := a.system.ReadHistoryForTurn(ctx, userID, sessionID, turnID, messageIDs, 0)
	if err != nil {
		return agent.HistoryReading{}, err
	}
	reading := agent.HistoryReading{}
	for _, message := range messages {
		reading.Messages = append(reading.Messages, agent.HistoryMessage{
			MessageID: message.MessageID,
			Role:      message.Role,
			SentAt:    message.SentAt.Format("2006-01-02T15:04:05Z"),
			Content:   message.Content,
			Truncated: message.Truncated,
			Citation: domain.Source{
				SourceID: "src-history-" + message.MessageID,
				Title:    "会话原文",
				Section:  "历史消息",
				Snippet:  domain.Snippet(message.Content, 280),
			},
		})
	}
	for _, refusal := range refusals {
		reading.Refusals = append(reading.Refusals, agent.HistoryRefusal{
			MessageID: refusal.MessageID, Reason: refusal.Reason,
		})
	}
	return reading, nil
}

// strconvItoa is a tiny indirection so the startup line builder stays readable.
func strconvItoa(value int) string {
	return fmt.Sprintf("%d", value)
}

// redisEpisodeView adapts the Redis index to the service's view interface.
type redisEpisodeView struct {
	index *redisindex.EpisodeIndex
}

// NewRedisEpisodeView exposes the Redis view adapter so the reindex tool can
// rebuild the same view the server writes, through the same type mapping.
func NewRedisEpisodeView(index *redisindex.EpisodeIndex) service.EpisodeVectorIndex {
	return redisEpisodeView{index: index}
}

func (v redisEpisodeView) EnsureIndex(ctx context.Context) error {
	return v.index.EnsureIndex(ctx)
}

func (v redisEpisodeView) Store(ctx context.Context, document service.EpisodeIndexDocument) error {
	return v.index.Store(ctx, redisindex.EpisodeDocument{
		EpisodeID:     document.EpisodeID,
		UserID:        document.UserID,
		SessionID:     document.SessionID,
		Status:        document.Status,
		LastMessageAt: document.LastMessageAt,
		Title:         document.Title,
		TopicKeys:     document.TopicKeys,
		SearchText:    document.SearchText,
		Identity:      document.Identity,
		Vector:        document.Vector,
	})
}

func (v redisEpisodeView) Delete(ctx context.Context, userID string, episodeIDs []string) error {
	return v.index.Delete(ctx, userID, episodeIDs)
}

func (v redisEpisodeView) SearchByVector(ctx context.Context, userID, currentSession, identity string, vector []float32, topK int) ([]service.EpisodeSearchHit, error) {
	hits, err := v.index.SearchByVector(ctx, userID, currentSession, identity, vector, topK)
	if err != nil {
		return nil, err
	}
	return convertEpisodeHits(hits), nil
}

func (v redisEpisodeView) SearchByText(ctx context.Context, userID, currentSession, identity string, terms []string, topK int) ([]service.EpisodeSearchHit, error) {
	hits, err := v.index.SearchByText(ctx, userID, currentSession, identity, terms, topK)
	if err != nil {
		return nil, err
	}
	return convertEpisodeHits(hits), nil
}

func (v redisEpisodeView) SearchExactTopics(ctx context.Context, userID, currentSession, identity string, topics []string, topK int) ([]service.EpisodeSearchHit, error) {
	hits, err := v.index.SearchExactTopics(ctx, userID, currentSession, identity, topics, topK)
	if err != nil {
		return nil, err
	}
	return convertEpisodeHits(hits), nil
}

func (v redisEpisodeView) CountUser(ctx context.Context, userID string) (int, error) {
	return v.index.CountUser(ctx, userID)
}

func (v redisEpisodeView) ResetUser(ctx context.Context, userID string) error {
	return v.index.ResetUser(ctx, userID)
}

func convertEpisodeHits(hits []redisindex.EpisodeSearchResult) []service.EpisodeSearchHit {
	out := make([]service.EpisodeSearchHit, 0, len(hits))
	for _, hit := range hits {
		out = append(out, service.EpisodeSearchHit{
			EpisodeID: hit.EpisodeID, LastMessageAt: hit.LastMessageAt,
			Score: hit.Score, Distance: hit.Distance, Rank: hit.Rank,
		})
	}
	return out
}
