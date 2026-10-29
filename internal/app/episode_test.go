package app

import (
	"strings"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/agent"
	"github.com/xiaopang456/GopherLearn/internal/knowledge"
	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/openai"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/service"
)

// episodeLookup builds an env lookup from a map.
func episodeLookup(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestEpisodeConfigDefaultsToOff(t *testing.T) {
	_, enabled, err := NewEpisodeSetup(repository.NewInMemoryRepository(),
		RetrievalConfig{Mode: RetrievalModeDeterministic}, episodeLookup(nil))
	if err != nil {
		t.Fatalf("unset environment: %v", err)
	}
	if enabled {
		t.Fatal("the feature turned itself on without being asked")
	}
	if _, enabled, _ := NewEpisodeSetup(repository.NewInMemoryRepository(),
		RetrievalConfig{Mode: RetrievalModeDeterministic},
		episodeLookup(map[string]string{"GOPHERLEARN_EPISODES": "OFF"})); enabled {
		t.Fatal("the off spelling was not honoured")
	}
	if _, _, err := NewEpisodeSetup(repository.NewInMemoryRepository(),
		RetrievalConfig{Mode: RetrievalModeDeterministic},
		episodeLookup(map[string]string{"GOPHERLEARN_EPISODES": "perhaps"})); err == nil {
		t.Fatal("a typo silently defaulted to off")
	}
}

func TestEpisodeConfigValidatesItsBudgets(t *testing.T) {
	// A lease shorter than the task timeout invites a second worker onto a
	// projection the first one is still running.
	_, _, err := NewEpisodeSetup(repository.NewInMemoryRepository(),
		RetrievalConfig{Mode: RetrievalModeDeterministic},
		episodeLookup(map[string]string{
			"GOPHERLEARN_EPISODES":             "on",
			"GOPHERLEARN_EPISODE_TASK_TIMEOUT": "60s",
			"GOPHERLEARN_EPISODE_LEASE":        "30s",
		}))
	if err == nil {
		t.Fatal("a lease shorter than the task timeout was accepted")
	}
	_, _, err = NewEpisodeSetup(repository.NewInMemoryRepository(),
		RetrievalConfig{Mode: RetrievalModeDeterministic},
		episodeLookup(map[string]string{
			"GOPHERLEARN_EPISODES":             "on",
			"GOPHERLEARN_EPISODE_MAX_ATTEMPTS": "12",
		}))
	if err == nil {
		t.Fatal("an out-of-range attempt ceiling was accepted")
	}
	// A valid on-configuration resolves to the production defaults.
	_, validConfig, err := service.EpisodeConfigFromEnvironment(episodeLookup(map[string]string{"GOPHERLEARN_EPISODES": "on"}))
	if err != nil {
		t.Fatalf("a valid configuration was refused: %v", err)
	}
	if validConfig.MaxAttempts != service.DefaultEpisodeConfig().MaxAttempts ||
		validConfig.QuietWindow != service.DefaultEpisodeConfig().QuietWindow {
		t.Fatalf("defaults drifted: %+v", validConfig)
	}
}

// fakeEpisodeChat is the tool-capable model the wiring test installs.
type fakeEpisodeChat struct {
	toolChat
}

func TestEpisodeInstallBuildsToolsOnce(t *testing.T) {
	setup, enabled, err := NewEpisodeSetup(repository.NewInMemoryRepository(),
		RetrievalConfig{Mode: RetrievalModeDeterministic},
		episodeLookup(map[string]string{"GOPHERLEARN_EPISODES": "on"}))
	if err != nil || !enabled {
		t.Fatalf("setup: enabled=%v err=%v", enabled, err)
	}
	tools, err := setup.Install(&fakeEpisodeChat{})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if tools.Searcher == nil || tools.HistoryReader == nil {
		t.Fatal("the install produced an incomplete tool pair")
	}
	if _, err := setup.Install(&fakeEpisodeChat{}); err == nil {
		t.Fatal("a second installation was tolerated")
	}
	stack, err := setup.Stack()
	if err != nil {
		t.Fatalf("stack: %v", err)
	}
	if stack.Publisher == nil || stack.System == nil {
		t.Fatal("the stack is incomplete")
	}
	if !strings.HasPrefix(stack.StartupLine, "episodes=on") || !strings.Contains(stack.StartupLine, "view=in-process") {
		t.Fatalf("startup line = %q", stack.StartupLine)
	}
}

// TestAssembleRegistersEpisodeTools pins the registry shape: both episodic
// tools are installed together, and a deployment without the setup registers
// none of them. The startup line is the assembly's own statement of what it
// registered, so the assertion reads that rather than reaching inside.
func TestAssembleRegistersEpisodeTools(t *testing.T) {
	config, err := ConfigFromEnvironment(lookup(openAIEnvironment()))
	if err != nil {
		t.Fatal(err)
	}
	setup, enabled, err := NewEpisodeSetup(repository.NewInMemoryRepository(),
		RetrievalConfig{Mode: RetrievalModeDeterministic},
		episodeLookup(map[string]string{"GOPHERLEARN_EPISODES": "on"}))
	if err != nil || !enabled {
		t.Fatalf("setup: enabled=%v err=%v", enabled, err)
	}
	_, summary, err := New(knowledge.NewMemoryStore(), config, Options{
		NewModel: func(openai.Config) (model.ChatModel, error) { return &toolChat{}, nil },
		Episodes: setup.Install,
	})
	if err != nil {
		t.Fatalf("assemble with episodes: %v", err)
	}
	for _, want := range []string{agent.EpisodeSearchName, agent.ReadHistoryMessageName} {
		if !strings.Contains(summary, want) {
			t.Fatalf("%s was not registered: %q", want, summary)
		}
	}

	_, summary, err = New(knowledge.NewMemoryStore(), config, Options{
		NewModel: func(openai.Config) (model.ChatModel, error) { return &toolChat{}, nil },
	})
	if err != nil {
		t.Fatalf("assemble without episodes: %v", err)
	}
	if strings.Contains(summary, agent.EpisodeSearchName) || strings.Contains(summary, agent.ReadHistoryMessageName) {
		t.Fatalf("an episodic tool was registered without a setup: %q", summary)
	}
}

// TestEpisodeCitationIdentityComesFromTheRecord pins the citation's stability
// contract: the source id names the stored record and its version, so two
// cards that happen to share a title and an occurrence date never collapse
// into one citation, while repeated hits on the same card version do.
func TestEpisodeCitationIdentityComesFromTheRecord(t *testing.T) {
	card := func(id string, version uint64) service.EpisodeCard {
		return service.EpisodeCard{
			ID: id, Version: version,
			Title:      "编解码器选型",
			Synopsis:   "确定了选型。",
			OccurredAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		}
	}
	first := episodeCitation(card("ep-aaa", 1))
	sameTitleSameDate := episodeCitation(card("ep-bbb", 1))
	if first.SourceID == sameTitleSameDate.SourceID {
		t.Fatalf("two distinct cards with one title and date share a source id %q", first.SourceID)
	}
	if first.SourceID != episodeCitation(card("ep-aaa", 1)).SourceID {
		t.Fatalf("the same card and version changed its source id: %q", first.SourceID)
	}
	advanced := episodeCitation(card("ep-aaa", 2))
	if advanced.SourceID == first.SourceID {
		t.Fatalf("a re-projected card kept the old source id %q", first.SourceID)
	}
	if first.Title != "编解码器选型" || first.Section != "情景经历" {
		t.Fatalf("citation payload drifted: %+v", first)
	}
}
