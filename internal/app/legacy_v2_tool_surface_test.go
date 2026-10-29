package app

import (
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/agent"
	"github.com/xiaopang456/GopherLearn/internal/knowledge"
	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/openai"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

func TestLegacyV2ProductionToolSurfaceSeparatesHistoryCapability(t *testing.T) {
	config := ConfigFromTestEnvironment()
	config.MemoryReadMode = MemoryReadLegacyV2
	store := repository.NewInMemoryRepository()
	setup, enabled, err := NewEpisodeSetup(store, RetrievalConfig{Mode: RetrievalModeDeterministic}, episodeLookup(map[string]string{"GOPHERLEARN_EPISODES": "on"}))
	if err != nil || !enabled {
		t.Fatalf("episode setup: enabled=%v err=%v", enabled, err)
	}
	_, startup, err := New(knowledge.NewMemoryStore(), config, Options{
		NewModel: func(openai.Config) (model.ChatModel, error) { return &toolChat{}, nil },
		Episodes: setup.Install,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(startup, "knowledge_search") || !strings.Contains(startup, agent.ReadHistoryMessageName) {
		t.Fatalf("legacy_v2 startup tools = %q", startup)
	}
	for _, forbidden := range []string{agent.EpisodeSearchName, "memory_search", "topic_memory_read"} {
		if strings.Contains(startup, forbidden) {
			t.Fatalf("legacy_v2 registered forbidden tool %q: %s", forbidden, startup)
		}
	}
}

func ConfigFromTestEnvironment() Config {
	config, err := ConfigFromEnvironment(lookup(openAIEnvironment()))
	if err != nil {
		panic(err)
	}
	return config
}
