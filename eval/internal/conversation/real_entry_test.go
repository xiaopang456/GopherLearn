package conversation

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The counting proxy is the only place a provider credential exists. The
// evaluation forwards to it and changes nothing else: the body, the path and
// the absence of an output limit have to survive the relay.
func TestProxyForwarderKeepsTheRequestUnchanged(t *testing.T) {
	type received struct {
		path string
		body string
	}
	var seen []received
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen = append(seen, received{path: r.URL.Path, body: string(body)})
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer proxy.Close()
	forward := httptest.NewServer(proxyForwarder(proxy.URL))
	defer forward.Close()
	cases := []struct {
		path string
		want string
	}{
		{"/v1/chat/completions", "/lane/chat/v1/chat/completions"},
		{"/v1/embeddings", "/lane/embed/embeddings"},
		{"/v1/rerank", "/lane/rerank/api/v1/services/rerank/text-rerank/text-rerank"},
	}
	for _, c := range cases {
		body := `{"model":"synthetic","messages":[{"role":"system","content":"known"}],"input":["a"]}`
		response, err := http.Post(forward.URL+c.path, "application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
	}
	if len(seen) != len(cases) {
		t.Fatalf("forwarded %d requests", len(seen))
	}
	for i, c := range cases {
		if seen[i].path != c.want {
			t.Fatalf("lane path %s, want %s", seen[i].path, c.want)
		}
		if seen[i].body != `{"model":"synthetic","messages":[{"role":"system","content":"known"}],"input":["a"]}` {
			t.Fatalf("the request body was rewritten: %s", seen[i].body)
		}
	}
}

// A request the evaluation does not recognise never reaches the proxy.
func TestProxyForwarderRefusesUnknownPaths(t *testing.T) {
	forward := httptest.NewServer(proxyForwarder("http://127.0.0.1:1"))
	defer forward.Close()
	response, err := http.Post(forward.URL+"/v1/completions", "application/json", bytes.NewBufferString("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown path answered %d", response.StatusCode)
	}
}

// The real wire records the role and the provider usage of every request it
// relays, and refuses a request whose role it cannot classify.
func TestRealWireRecordsRoleAndUsage(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}}`)
	}))
	defer proxy.Close()
	ledger := &ModelLedger{}
	wire := realWire(ledger, RolePrompts{"known": "answer"}, proxy.URL)
	defer wire.Close()
	response, err := http.Post(wire.URL+"/v1/chat/completions", "application/json", bytes.NewBufferString(`{"model":"synthetic","messages":[{"role":"system","content":"known"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.ReadAll(response.Body); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	for i := 0; i < 100 && !ledger.Complete(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	calls := ledger.Calls()
	if len(calls) != 1 || calls[0].Role != "answer" || calls[0].Usage == nil || calls[0].Usage.PromptTokens != 12 {
		t.Fatalf("call record %+v", calls)
	}
	if !ledger.Complete() {
		t.Fatal("the ledger did not close")
	}
	refused := httptest.NewRecorder()
	wire.Config.Handler.ServeHTTP(refused, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(`{"messages":[{"role":"system","content":"unrecognized"}]}`)))
	if refused.Code != http.StatusBadRequest {
		t.Fatalf("unclassified request answered %d", refused.Code)
	}
}

// A paid run may only reach a loopback proxy and a loopback index.
func TestRealEnvironmentRefusesRemoteEndpoints(t *testing.T) {
	base := RealEnvironment{ProxyBaseURL: "http://127.0.0.1:18140", RedisAddress: "127.0.0.1:6379", StorageDir: "storage", ChatModel: "chat", EmbeddingModel: "embed", RerankModel: "rerank", UserID: 7, UserName: "learner", SessionID: "session", SummarySegmentMode: "off"}
	if err := base.Validate(); err != nil {
		t.Fatalf("a complete environment was refused: %v", err)
	}
	unknownMode := base
	unknownMode.SummarySegmentMode = "partial"
	if err := unknownMode.Validate(); err == nil {
		t.Fatal("a mode the product does not know was accepted")
	}
	remote := base
	remote.ProxyBaseURL = "http://192.0.2.10:18140"
	if err := remote.Validate(); err == nil {
		t.Fatal("a remote proxy was accepted")
	}
	remote = base
	remote.RedisAddress = "192.0.2.10:6379"
	if err := remote.Validate(); err == nil {
		t.Fatal("a remote index was accepted")
	}
	incomplete := base
	incomplete.UserID = 0
	if err := incomplete.Validate(); err == nil {
		t.Fatal("an incomplete identity was accepted")
	}
}

// The seed carries the fields the product keeps out of its own JSON form.
func TestKnowledgeSeedRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "seed.json")
	seed := KnowledgeSeed{UserID: 7, UserName: "learner", Documents: []KnowledgeDocument{{
		ID: "doc-1", UserID: 7, OriginalName: "note.md", ActiveName: "note.md",
		ActiveHash: "hash", StoragePath: "7/doc-1/source-v1.md", FileType: "markdown",
		ContentHash: "hash", FileSize: 12, Version: 1, ActiveVersion: 1, ChunkCount: 3,
		IndexStatus: "ready",
	}}}
	if err := WriteKnowledgeSeed(path, seed); err != nil {
		t.Fatal(err)
	}
	if err := WriteKnowledgeSeed(path, seed); err == nil {
		t.Fatal("an existing seed was overwritten")
	}
	loaded, err := LoadKnowledgeSeed(path)
	if err != nil {
		t.Fatal(err)
	}
	rows := loaded.documents()
	if len(rows) != 1 || rows[0].ActiveName == nil || *rows[0].ActiveName != "note.md" || rows[0].ActiveHash == nil || *rows[0].ActiveHash != "hash" {
		t.Fatalf("seed row %+v", rows)
	}
	if rows[0].IndexStatus != "ready" || rows[0].ActiveVersion != 1 {
		t.Fatalf("seed row %+v", rows[0])
	}
	broken := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(broken, []byte(`{"user_id":7,"user_name":"learner","documents":[{"id":"doc-1"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadKnowledgeSeed(broken); err == nil {
		t.Fatal("an incomplete document was accepted")
	}
}

// The seed file carries no credential and the copy keeps the layout.
func TestKnowledgeSeedHasNoSecret(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "seed.json")
	seed := KnowledgeSeed{UserID: 7, UserName: "learner"}
	if err := WriteKnowledgeSeed(path, seed); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err = json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"password", "token", "key", "secret"} {
		if _, ok := decoded[forbidden]; ok {
			t.Fatalf("the seed exposes %s", forbidden)
		}
	}
}
