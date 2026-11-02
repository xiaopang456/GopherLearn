package hybrid

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"GopherAI/entry"
	"github.com/xiaopang456/GopherLearn/conversation"
)

// readmeExampleTOML extracts the copy-and-paste TOML block that README §5.1
// offers as "the only values you have to change".
func readmeExampleTOML(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	const begin = "```toml\n[isolatedEntry]"
	start := strings.Index(text, begin)
	if start < 0 {
		t.Fatal("README §5.1 example block not found")
	}
	rest := text[start+len("```toml\n"):]
	end := strings.Index(rest, "\n```")
	if end < 0 {
		t.Fatal("README §5.1 example block is not closed")
	}
	block := rest[:end]
	for _, want := range []string{"[isolatedEntry]", "[ragModelConfig]", "[ordinaryChat]", "[mysqlConfig]", "[redisConfig]", "[jwtConfig]", "[knowledgeConfig]", "[mainConfig]"} {
		if !strings.Contains(block, want) {
			t.Fatalf("README §5.1 example is missing section %s", want)
		}
	}
	return block
}

// TestReadmeExampleConfigurationStartsWithStubbedProviders runs the exact
// configuration the README tells a first-time user to copy, through the real
// startup path, with stubbed providers: no model is called and no MySQL or
// Redis server is contacted.
func TestReadmeExampleConfigurationStartsWithStubbedProviders(t *testing.T) {
	text := readmeExampleTOML(t)
	// The only edit to the README text: keep the upload directory out of the
	// working tree. The loader only requires the value to be non-empty.
	text = strings.Replace(text,
		`storageDir = "data/gopherlearn-integrated/knowledge"`,
		`storageDir = "`+filepath.ToSlash(t.TempDir())+`"`, 1)
	path := filepath.Join(t.TempDir(), "readme.example.toml")
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}

	runtime, _, _, _, _ := entryRuntimeFixture(t, &ordinaryModel{})
	// The fixture asserts the tutorial configuration's ordinary model; this
	// test asserts the README's own values instead.
	runtime.client = func(c conversation.ClientConfig) (conversation.Model, error) {
		if c.Model != "deepseek-flash" || c.Timeout != 30*time.Second || c.MaxOutputTokens != 1024 {
			t.Errorf("ordinary client did not resolve from the README example: %+v", c)
		}
		return &ordinaryModel{}, nil
	}
	listening := make(chan string, 1)
	runtime.listen = func(network, address string) (net.Listener, error) {
		listener, err := net.Listen(network, "127.0.0.1:0")
		if err == nil {
			listening <- "http://" + listener.Addr().String()
		}
		return listener, err
	}
	runtime.server = entry.NewHTTPServer

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- runEntry(ctx, path, runtime) }()
	select {
	case <-listening:
	case err := <-finished:
		t.Fatalf("README example configuration was refused at startup: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("entry did not listen with the README example configuration")
	}
	cancel()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("entry did not shut down cleanly: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("unbounded entry shutdown")
	}
}
