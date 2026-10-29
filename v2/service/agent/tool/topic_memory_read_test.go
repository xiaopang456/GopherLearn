package agenttool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"GopherAI/model"
	serviceagent "GopherAI/service/agent"

	"github.com/cloudwego/eino/components/tool"
)

// recordingTopicProvider is the exact-key reader, recording what it was asked
// for so a test can prove the keys came from server state and nowhere else.
type recordingTopicProvider struct {
	mu       sync.Mutex
	calls    atomic.Int32
	lastKeys []string
	lastRun  serviceagent.RunContext
	records  []serviceagent.TopicMemoryRecord
	err      error
	delay    time.Duration
}

func (p *recordingTopicProvider) TopicMemoriesByKeys(ctx context.Context, run serviceagent.RunContext,
	keys []string) ([]serviceagent.TopicMemoryRecord, error) {
	p.calls.Add(1)
	p.mu.Lock()
	p.lastKeys = append([]string(nil), keys...)
	p.lastRun = run
	p.mu.Unlock()
	if p.delay > 0 {
		select {
		case <-time.After(p.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if p.err != nil {
		return nil, p.err
	}
	return p.records, nil
}

func (p *recordingTopicProvider) keys() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.lastKeys...)
}

func topicRow(id, topicKey, subject, content string, version uint64, owner int64) serviceagent.TopicMemoryRecord {
	return serviceagent.TopicMemoryRecord{
		ID: id, Version: version, Owner: owner, Status: string(model.LongTermMemoryActive),
		Scope: string(model.MemoryScopeTopic), TopicKey: topicKey, Subject: subject, Content: content,
		Line: fmt.Sprintf("[topic; tags=%s] %s: %s", topicKey, subject, content),
	}
}

func topicReadRun(keys ...string) serviceagent.RunContext {
	return serviceagent.RunContext{
		RunID: "run-1", UserID: 1, UserName: "learner", SessionID: "current", Question: "q",
		AuthoritativeTopicKeys: keys, MemoryEvidence: serviceagent.NewMemoryEvidenceRegistry(),
	}
}

func buildTopicRead(t *testing.T, provider serviceagent.AuthoritativeTopicMemoryProvider,
	run serviceagent.RunContext) tool.InvokableTool {
	t.Helper()
	registry, err := NewTopicMemoryReadRegistry(provider, DefaultTopicMemoryReadConfig())
	if err != nil {
		t.Fatal(err)
	}
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(tools))
	}
	invokable, ok := tools[0].(tool.InvokableTool)
	if !ok {
		t.Fatal("topic_memory_read is not invokable")
	}
	return invokable
}

func readTopic(t *testing.T, invokable tool.InvokableTool, arguments string) TopicMemoryReadOutput {
	t.Helper()
	raw, err := invokable.InvokableRun(context.Background(), arguments)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	var output TopicMemoryReadOutput
	if err := json.Unmarshal([]byte(raw), &output); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return output
}

// The happy path: the server holds a key, the tool reads by that key, and every
// returned card carries a reference that resolves to the real record.
func TestTopicMemoryReadIssuesReferencesForTheServerHeldTopic(t *testing.T) {
	provider := &recordingTopicProvider{records: []serviceagent.TopicMemoryRecord{
		topicRow("m-1", "rpc-粘包", "粘包", "把 TCP 当成没有刻度的水管", 1730000000, 1),
		topicRow("m-2", "rpc-粘包", "分隔符", "以前用换行分隔，遇到二进制就炸了", 1730000001, 1),
	}}
	run := topicReadRun("rpc-粘包")
	output := readTopic(t, buildTopicRead(t, provider, run), `{"reason":"asked how they understand it"}`)

	if output.Status != topicReadStatusOK || output.Reason != topicReadReasonNone {
		t.Fatalf("output = %#v", output)
	}
	if len(output.Results) != 2 {
		t.Fatalf("results = %#v", output.Results)
	}
	if got := provider.keys(); len(got) != 1 || got[0] != "rpc-粘包" {
		t.Fatalf("the provider was asked for %v", got)
	}
	// The identity travels with the run and is re-checked by the provider; the
	// model cannot influence either.
	if provider.lastRun.UserID != 1 || provider.lastRun.SessionID != "current" {
		t.Fatalf("the provider was called under %#v", provider.lastRun)
	}
	for _, card := range output.Results {
		entry, err := run.MemoryEvidence.Lookup(card.Ref)
		if err != nil {
			t.Fatalf("reference %s does not resolve: %v", card.Ref, err)
		}
		if entry.Kind != serviceagent.MemoryEvidenceTopic || entry.Owner != 1 ||
			entry.Status != string(model.LongTermMemoryActive) {
			t.Fatalf("entry %s lost the server identity: %#v", card.Ref, entry)
		}
		if !strings.HasPrefix(entry.Source, "topic_memory_read:") ||
			!strings.Contains(entry.Source, "rpc-粘包") {
			t.Fatalf("entry %s does not record how it reached the model: %q", card.Ref, entry.Source)
		}
	}
	// The observation never prints the internal key. A model that could repeat
	// it back is a model a later prompt could imitate.
	blob, _ := json.Marshal(output)
	if strings.Contains(string(blob), "rpc-粘包") {
		t.Fatalf("the observation leaked the topic key: %s", blob)
	}
	reads := run.MemoryEvidence.TopicReads()
	if len(reads) != 1 || reads[0].Status != topicReadStatusOK || len(reads[0].Refs) != 2 {
		t.Fatalf("trace = %#v", reads)
	}
	// It is a topic read, not a search, and every cost report in this series
	// depends on those staying separable.
	if len(run.MemoryEvidence.Calls()) != 0 {
		t.Fatalf("a topic read was recorded as a memory search: %#v", run.MemoryEvidence.Calls())
	}
}

// No authoritative key is a complete answer, not a failed search. The provider
// must not be reached at all: a provider that could be called with no keys is
// one wiring mistake away from returning whatever it likes.
func TestTopicMemoryReadRefusesWithoutAnAuthoritativeKey(t *testing.T) {
	provider := &recordingTopicProvider{records: []serviceagent.TopicMemoryRecord{
		topicRow("m-1", "rpc-粘包", "粘包", "不该被读到", 1730000000, 1),
	}}
	for name, run := range map[string]serviceagent.RunContext{
		"no keys at all": topicReadRun(),
		"blank key":      topicReadRun("   "),
	} {
		output := readTopic(t, buildTopicRead(t, provider, run), `{}`)
		if output.Status != TopicReadStatusNoAuthoritativeTopic ||
			output.Reason != topicReadReasonNoActiveTopic {
			t.Fatalf("%s: output = %#v", name, output)
		}
		if len(output.Results) != 0 {
			t.Fatalf("%s: a topic was returned anyway: %#v", name, output.Results)
		}
		if provider.calls.Load() != 0 {
			t.Fatalf("%s: the provider was reached %d times", name, provider.calls.Load())
		}
		if len(run.MemoryEvidence.Entries()) != 0 {
			t.Fatalf("%s: a reference was issued: %#v", name, run.MemoryEvidence.Entries())
		}
		// It is still recorded: a tool call the model chose to make is a
		// routing decision whether or not anything came back.
		reads := run.MemoryEvidence.TopicReads()
		if len(reads) != 1 || reads[0].Status != TopicReadStatusNoAuthoritativeTopic ||
			len(reads[0].Keys) != 0 {
			t.Fatalf("%s: trace = %#v", name, reads)
		}
	}
}

// A provider failure loses this tool and nothing else. There is no lexical
// fallback, no most-recent episode and no widening to the general search: the
// whole claim of the tool is "the server knows this is the topic".
func TestTopicMemoryReadFailsClosedWithoutFallback(t *testing.T) {
	provider := &recordingTopicProvider{err: errors.New("mysql is unhappy")}
	run := topicReadRun("rpc-粘包")
	output := readTopic(t, buildTopicRead(t, provider, run), `{}`)
	if output.Status != topicReadStatusUnavailable || output.Reason != topicReadReasonProviderFailed {
		t.Fatalf("output = %#v", output)
	}
	if !output.Degraded || len(output.Results) != 0 {
		t.Fatalf("a failed read returned something: %#v", output)
	}
	if len(run.MemoryEvidence.Entries()) != 0 {
		t.Fatalf("a failed read issued references: %#v", run.MemoryEvidence.Entries())
	}
	reads := run.MemoryEvidence.TopicReads()
	if len(reads) != 1 || !reads[0].Degraded {
		t.Fatalf("trace = %#v", reads)
	}
}

// The provider's own timeout is inside the run deadline, and a slow database
// costs this tool rather than the turn.
func TestTopicMemoryReadTimesOutWithoutFailingTheTurn(t *testing.T) {
	provider := &recordingTopicProvider{delay: 3 * time.Second,
		records: []serviceagent.TopicMemoryRecord{topicRow("m-1", "rpc-粘包", "粘包", "不该被读到", 1730000000, 1)}}
	registry, err := NewTopicMemoryReadRegistry(provider, TopicMemoryReadConfig{
		MaxCalls: 1, MaxResults: 5, MaxObservationTokens: 1500,
		Timeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	run := topicReadRun("rpc-粘包")
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	output := readTopic(t, tools[0].(tool.InvokableTool), `{}`)
	if output.Status != topicReadStatusUnavailable {
		t.Fatalf("a timed-out read did not report unavailable: %#v", output)
	}
}

// A cancelled run stops the tool rather than letting it answer from a context
// that is already over.
func TestTopicMemoryReadRefusesACancelledContext(t *testing.T) {
	provider := &recordingTopicProvider{}
	invokable := buildTopicRead(t, provider, topicReadRun("rpc-粘包"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := invokable.InvokableRun(ctx, `{}`); err == nil {
		t.Fatal("a cancelled run still read topic memory")
	}
	if provider.calls.Load() != 0 {
		t.Fatalf("the provider ran %d times under a cancelled context", provider.calls.Load())
	}
}

// One call per run. A second could only re-read the same rows from the same
// server-held key, so it is refused here as well as by the runner's per-tool
// limit - two fuses, because the runner's is about budget and this one is about
// the tool having nothing else to say.
func TestTopicMemoryReadAllowsOneCallPerRun(t *testing.T) {
	provider := &recordingTopicProvider{records: []serviceagent.TopicMemoryRecord{
		topicRow("m-1", "rpc-粘包", "粘包", "把 TCP 当成没有刻度的水管", 1730000000, 1),
	}}
	run := topicReadRun("rpc-粘包")
	invokable := buildTopicRead(t, provider, run)
	if first := readTopic(t, invokable, `{}`); first.Status != topicReadStatusOK {
		t.Fatalf("first = %#v", first)
	}
	second := readTopic(t, invokable, `{}`)
	if second.Status != topicReadStatusCallLimit || second.Reason != topicReadReasonCallsExhausted {
		t.Fatalf("second = %#v", second)
	}
	if provider.calls.Load() != 1 {
		t.Fatalf("the provider ran %d times", provider.calls.Load())
	}
	if len(run.MemoryEvidence.Entries()) != 1 {
		t.Fatalf("the refused call still issued references: %#v", run.MemoryEvidence.Entries())
	}
}

// A record the run has already issued is not handed over a second time under a
// second label, whichever component issued it first. An answer citing both
// would be citing one record twice while looking like it had two.
func TestTopicMemoryReadSkipsRecordsThisRunAlreadyIssued(t *testing.T) {
	provider := &recordingTopicProvider{records: []serviceagent.TopicMemoryRecord{
		topicRow("m-1", "rpc-粘包", "粘包", "把 TCP 当成没有刻度的水管", 1730000000, 1),
		topicRow("m-2", "rpc-粘包", "分隔符", "换行分隔遇到二进制就炸", 1730000001, 1),
	}}
	run := topicReadRun("rpc-粘包")
	// memory_search got there first with m-1.
	first := run.MemoryEvidence.Register(serviceagent.MemoryEvidenceEntry{
		Kind: serviceagent.MemoryEvidenceTopic, InternalID: "m-1", Version: 1730000000, Owner: 1,
	})
	output := readTopic(t, buildTopicRead(t, provider, run), `{}`)
	if len(output.Results) != 1 || output.Results[0].Ref == first {
		t.Fatalf("results = %#v", output.Results)
	}
	if !strings.Contains(output.Results[0].Content, "换行分隔") {
		t.Fatalf("the wrong record survived: %#v", output.Results[0])
	}
	reads := run.MemoryEvidence.TopicReads()
	if len(reads) != 1 || reads[0].SuppressedHits != 1 {
		t.Fatalf("trace = %#v", reads)
	}
}

// Every hit already issued is "nothing new", not "nothing recorded". Reporting
// the second would tell the model a negative it has no basis for, which is the
// shape of BUG-AGENT-152.
func TestTopicMemoryReadSaysNothingNewRatherThanNothingRecorded(t *testing.T) {
	provider := &recordingTopicProvider{records: []serviceagent.TopicMemoryRecord{
		topicRow("m-1", "rpc-粘包", "粘包", "把 TCP 当成没有刻度的水管", 1730000000, 1),
	}}
	run := topicReadRun("rpc-粘包")
	run.MemoryEvidence.Register(serviceagent.MemoryEvidenceEntry{
		Kind: serviceagent.MemoryEvidenceTopic, InternalID: "m-1", Version: 1730000000, Owner: 1,
	})
	output := readTopic(t, buildTopicRead(t, provider, run), `{}`)
	if output.Status != topicReadStatusNoEvidence || output.Reason != topicReadReasonAlreadyProvided {
		t.Fatalf("output = %#v", output)
	}
	if strings.Contains(output.Summary, "没有已记录") {
		t.Fatalf("the summary asserts a negative: %q", output.Summary)
	}
}

// The dedup is exact. A different version is a record the model has not read, a
// different kind is a different table, and a different id is a different note.
func TestTopicMemoryReadDedupIsExactOnKindIDAndVersion(t *testing.T) {
	provider := &recordingTopicProvider{records: []serviceagent.TopicMemoryRecord{
		topicRow("m-1", "rpc-粘包", "编辑过的", "改过之后的理解", 1730000009, 1),
		topicRow("m-9", "rpc-粘包", "另一条", "另一个记录", 1730000000, 1),
	}}
	run := topicReadRun("rpc-粘包")
	// An older version of m-1, and an episode that happens to share m-9's id.
	run.MemoryEvidence.Register(serviceagent.MemoryEvidenceEntry{
		Kind: serviceagent.MemoryEvidenceTopic, InternalID: "m-1", Version: 1730000000, Owner: 1,
	})
	run.MemoryEvidence.Register(serviceagent.MemoryEvidenceEntry{
		Kind: serviceagent.MemoryEvidenceEpisodic, InternalID: "m-9", Version: 1730000000, Owner: 1,
	})
	output := readTopic(t, buildTopicRead(t, provider, run), `{}`)
	if len(output.Results) != 2 {
		t.Fatalf("the dedup removed a record it was not asked to: %#v", output.Results)
	}
}

// A record that does not fit the observation budget is dropped whole and never
// registered. A reference the model was not shown would still resolve, which is
// a legal citation of a record it could not have read.
func TestTopicMemoryReadRegistersOnlyWhatFits(t *testing.T) {
	long := strings.Repeat("很长的一段解释。", 400)
	provider := &recordingTopicProvider{records: []serviceagent.TopicMemoryRecord{
		topicRow("m-short", "rpc-粘包", "短的", "短的理解", 1730000000, 1),
		topicRow("m-long", "rpc-粘包", "长的", long, 1730000001, 1),
	}}
	run := topicReadRun("rpc-粘包")
	registry, err := NewTopicMemoryReadRegistry(provider, TopicMemoryReadConfig{
		MaxCalls: 1, MaxResults: 5, MaxObservationTokens: 64,
		Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	output := readTopic(t, tools[0].(tool.InvokableTool), `{}`)
	issued := run.MemoryEvidence.Entries()
	if len(output.Results) != len(issued) {
		t.Fatalf("%d cards but %d references", len(output.Results), len(issued))
	}
	for _, entry := range issued {
		if entry.InternalID == "m-long" {
			t.Fatalf("a record that did not fit was still given a reference: %#v", entry)
		}
	}
	// Skipped, not stopped: the short note behind the long one still arrives,
	// and the observation declares that something was left out.
	if len(output.Results) != 1 || !strings.Contains(output.Results[0].Content, "短的理解") {
		t.Fatalf("results = %#v", output.Results)
	}
	if !output.Truncated {
		t.Fatal("the omission was not declared")
	}
}

// MaxResults bounds the block the way it bounded v2's, so the two modes show
// the model the same amount of the same topic.
func TestTopicMemoryReadHonoursTheResultCap(t *testing.T) {
	records := make([]serviceagent.TopicMemoryRecord, 0, 8)
	for i := 0; i < 8; i++ {
		records = append(records, topicRow(fmt.Sprintf("m-%d", i), "rpc-粘包",
			fmt.Sprintf("第%d条", i), "一段理解", uint64(1730000000+i), 1))
	}
	run := topicReadRun("rpc-粘包")
	output := readTopic(t, buildTopicRead(t, &recordingTopicProvider{records: records}, run), `{}`)
	if len(output.Results) != DefaultTopicMemoryReadConfig().MaxResults {
		t.Fatalf("results = %d", len(output.Results))
	}
	if len(run.MemoryEvidence.Entries()) != DefaultTopicMemoryReadConfig().MaxResults {
		t.Fatalf("references = %d", len(run.MemoryEvidence.Entries()))
	}
}

// The tool refuses to exist without a trusted identity and without the run's
// evidence registry. A memory tool that would run without an owner is a
// cross-account read waiting for a wiring mistake.
func TestTopicMemoryReadRefusesUntrustedWiring(t *testing.T) {
	provider := &recordingTopicProvider{}
	registry, err := NewTopicMemoryReadRegistry(provider, DefaultTopicMemoryReadConfig())
	if err != nil {
		t.Fatal(err)
	}
	for name, run := range map[string]serviceagent.RunContext{
		"no user":     {SessionID: "current", MemoryEvidence: serviceagent.NewMemoryEvidenceRegistry()},
		"no session":  {UserID: 1, MemoryEvidence: serviceagent.NewMemoryEvidenceRegistry()},
		"no registry": {UserID: 1, SessionID: "current"},
	} {
		if _, err := registry.Tools(context.Background(), run); err == nil {
			t.Fatalf("%s: the tool was built anyway", name)
		}
	}
	if _, err := NewTopicMemoryReadRegistry(nil, DefaultTopicMemoryReadConfig()); err == nil {
		t.Fatal("a registry was built without a provider")
	}
}

// The model has no argument that reaches a query, a filter or a permission
// check. This is asserted on the frozen schema rather than on the struct,
// because the schema is what the model actually reads.
func TestTopicMemoryReadSchemaOffersNoHandles(t *testing.T) {
	registry, err := NewTopicMemoryReadRegistry(&recordingTopicProvider{}, DefaultTopicMemoryReadConfig())
	if err != nil {
		t.Fatal(err)
	}
	tools, err := registry.Tools(context.Background(), topicReadRun("rpc-粘包"))
	if err != nil {
		t.Fatal(err)
	}
	info, err := tools[0].Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The parameters alone, without the prose description: what matters is
	// what the model may send, not which words the instructions use.
	params, err := info.ParamsOneOf.ToOpenAPIV3()
	if err != nil {
		t.Fatal(err)
	}
	blob, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"user_id", "userId", "session", "topic_key", "topicKey",
		"memory_id", "kinds", "top_k", "query"} {
		if strings.Contains(string(blob), forbidden) {
			t.Fatalf("the schema offers %q: %s", forbidden, blob)
		}
	}
	if !strings.Contains(string(blob), "reason") {
		t.Fatalf("the schema lost its only field: %s", blob)
	}
	// And the digest is stable, because a freeze binds the contract rather than
	// describing it.
	digest, err := TopicMemoryReadSchemaDigest()
	if err != nil {
		t.Fatal(err)
	}
	again, err := TopicMemoryReadSchemaDigest()
	if err != nil {
		t.Fatal(err)
	}
	if digest != again || len(digest) != 64 {
		t.Fatalf("schema digest = %q / %q", digest, again)
	}
	searchDigest, err := MemorySearchSchemaDigest()
	if err != nil {
		t.Fatal(err)
	}
	if digest == searchDigest {
		t.Fatal("the two tools hash to one contract")
	}
}

// Two runs in flight must not see each other's topic, references or dedup
// state. The registry is shared; everything per-run is not.
func TestConcurrentTopicReadsDoNotShareRunState(t *testing.T) {
	provider := serviceagent.AuthoritativeTopicMemoryProviderFunc(
		func(_ context.Context, run serviceagent.RunContext, keys []string) ([]serviceagent.TopicMemoryRecord, error) {
			return []serviceagent.TopicMemoryRecord{
				topicRow(fmt.Sprintf("m-%d", run.UserID), keys[0],
					fmt.Sprintf("owner-%d", run.UserID), fmt.Sprintf("只属于 %d 的理解", run.UserID),
					1730000000, run.UserID),
			}, nil
		})
	registry, err := NewTopicMemoryReadRegistry(provider, DefaultTopicMemoryReadConfig())
	if err != nil {
		t.Fatal(err)
	}
	const runs = 8
	var wg sync.WaitGroup
	failures := make(chan string, runs)
	for i := 1; i <= runs; i++ {
		wg.Add(1)
		go func(owner int64) {
			defer wg.Done()
			run := serviceagent.RunContext{
				RunID: fmt.Sprintf("run-%d", owner), UserID: owner,
				SessionID:              fmt.Sprintf("session-%d", owner),
				AuthoritativeTopicKeys: []string{fmt.Sprintf("topic-%d", owner)},
				MemoryEvidence:         serviceagent.NewMemoryEvidenceRegistry(),
			}
			tools, toolErr := registry.Tools(context.Background(), run)
			if toolErr != nil {
				failures <- toolErr.Error()
				return
			}
			raw, runErr := tools[0].(tool.InvokableTool).InvokableRun(context.Background(), `{}`)
			if runErr != nil {
				failures <- runErr.Error()
				return
			}
			var output TopicMemoryReadOutput
			if err := json.Unmarshal([]byte(raw), &output); err != nil {
				failures <- err.Error()
				return
			}
			if len(output.Results) != 1 ||
				!strings.Contains(output.Results[0].Content, fmt.Sprintf("只属于 %d", owner)) {
				failures <- fmt.Sprintf("owner %d read %#v", owner, output.Results)
				return
			}
			entry, err := run.MemoryEvidence.Lookup(output.Results[0].Ref)
			if err != nil || entry.Owner != owner {
				failures <- fmt.Sprintf("owner %d resolved %s to %#v (%v)", owner, output.Results[0].Ref, entry, err)
			}
		}(int64(i))
	}
	wg.Wait()
	close(failures)
	for failure := range failures {
		t.Fatal(failure)
	}
}

// The config is validated rather than defaulted, so a wiring mistake stops the
// build instead of quietly widening what the tool may return.
func TestTopicMemoryReadConfigIsValidated(t *testing.T) {
	valid := DefaultTopicMemoryReadConfig()
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*TopicMemoryReadConfig){
		"two calls":    func(c *TopicMemoryReadConfig) { c.MaxCalls = 2 },
		"zero calls":   func(c *TopicMemoryReadConfig) { c.MaxCalls = 0 },
		"six results":  func(c *TopicMemoryReadConfig) { c.MaxResults = 6 },
		"no results":   func(c *TopicMemoryReadConfig) { c.MaxResults = 0 },
		"tiny window":  func(c *TopicMemoryReadConfig) { c.MaxObservationTokens = 8 },
		"huge window":  func(c *TopicMemoryReadConfig) { c.MaxObservationTokens = 9000 },
		"no timeout":   func(c *TopicMemoryReadConfig) { c.Timeout = 0 },
		"long timeout": func(c *TopicMemoryReadConfig) { c.Timeout = time.Minute },
	} {
		broken := DefaultTopicMemoryReadConfig()
		mutate(&broken)
		if err := broken.Validate(); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}
