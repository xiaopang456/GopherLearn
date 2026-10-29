package agenttool

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	serviceagent "GopherAI/service/agent"

	"github.com/cloudwego/eino/components/tool"
)

type recordingTopicSnapshotProvider struct {
	calls    atomic.Int32
	snapshot serviceagent.AuthoritativeTopicMemorySnapshot
	err      error
	keys     []string
}

func (p *recordingTopicSnapshotProvider) TopicMemorySnapshotByKeys(_ context.Context,
	_ serviceagent.RunContext, keys []string) (serviceagent.AuthoritativeTopicMemorySnapshot, error) {
	p.calls.Add(1)
	p.keys = append([]string(nil), keys...)
	return p.snapshot, p.err
}

func buildTopicReadV4(t *testing.T, provider serviceagent.AuthoritativeTopicMemorySnapshotProvider,
	run serviceagent.RunContext, config TopicMemoryReadConfig) tool.InvokableTool {
	t.Helper()
	registry, err := NewTopicMemoryReadV4Registry(provider, config)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	return tools[0].(tool.InvokableTool)
}

func readTopicV4(t *testing.T, invokable tool.InvokableTool) TopicMemoryReadV4Output {
	t.Helper()
	raw, err := invokable.InvokableRun(context.Background(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	var output TopicMemoryReadV4Output
	if err := json.Unmarshal([]byte(raw), &output); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return output
}

func TestTopicMemoryReadV4DeclaresScopeAndCompleteWindow(t *testing.T) {
	provider := &recordingTopicSnapshotProvider{snapshot: serviceagent.AuthoritativeTopicMemorySnapshot{
		Records: []serviceagent.TopicMemoryRecord{
			topicRow("m-1", "tcp-framing", "类比", "像没有刻度的水管", 1, 1),
			topicRow("m-2", "tcp-framing", "困惑", "没分清半包和粘包", 2, 1),
		}, EligibleCount: 2,
	}}
	run := topicReadRun("tcp-framing")
	output := readTopicV4(t, buildTopicReadV4(t, provider, run, DefaultTopicMemoryReadConfig()))
	if output.Scope != topicReadV4ScopeCurrent || len(output.CurrentTopicKeys) != 1 ||
		output.CurrentTopicKeys[0] != "tcp-framing" {
		t.Fatalf("scope = %#v", output)
	}
	if output.ReturnedCount != 2 || output.EligibleCount != 2 || output.AlreadyInContextCount != 0 ||
		output.OmittedCount != 0 || !output.Complete {
		t.Fatalf("counts = %#v", output)
	}
	if output.NextAction != topicReadV4NextEvaluate || len(run.MemoryEvidence.Entries()) != 2 {
		t.Fatalf("result = %#v", output)
	}
	reads := run.MemoryEvidence.TopicReads()
	if len(reads) != 1 || reads[0].Scope != output.Scope || reads[0].NextAction != output.NextAction {
		t.Fatalf("server trace did not retain the observation contract: %#v", reads)
	}
}

func TestTopicMemoryReadV4ReportsRowsBeyondTheBoundedWindow(t *testing.T) {
	records := make([]serviceagent.TopicMemoryRecord, 0, 8)
	for i := 0; i < 8; i++ {
		records = append(records, topicRow(string(rune('a'+i)), "tcp-framing", "记录", "内容", uint64(i+1), 1))
	}
	provider := &recordingTopicSnapshotProvider{snapshot: serviceagent.AuthoritativeTopicMemorySnapshot{
		Records: records, EligibleCount: 8,
	}}
	output := readTopicV4(t, buildTopicReadV4(t, provider, topicReadRun("tcp-framing"),
		DefaultTopicMemoryReadConfig()))
	if output.ReturnedCount != 5 || output.OmittedCount != 3 || output.Complete {
		t.Fatalf("overflow was hidden: %#v", output)
	}
	if output.ReturnedCount+output.AlreadyInContextCount+output.OmittedCount != output.EligibleCount {
		t.Fatalf("count equation failed: %#v", output)
	}
}

func TestTopicMemoryReadV4CountsAlreadyIssuedRowsBeyondTheDisplayWindow(t *testing.T) {
	records := make([]serviceagent.TopicMemoryRecord, 0, 8)
	for i := 0; i < 8; i++ {
		records = append(records, topicRow(string(rune('a'+i)), "tcp-framing", "记录", "内容", uint64(i+1), 1))
	}
	run := topicReadRun("tcp-framing")
	run.MemoryEvidence.Register(serviceagent.MemoryEvidenceEntry{
		Kind: serviceagent.MemoryEvidenceTopic, InternalID: "h", Version: 8, Owner: 1,
	})
	provider := &recordingTopicSnapshotProvider{snapshot: serviceagent.AuthoritativeTopicMemorySnapshot{
		Records: records, EligibleCount: 8,
	}}
	output := readTopicV4(t, buildTopicReadV4(t, provider, run, DefaultTopicMemoryReadConfig()))
	if output.ReturnedCount != 5 || output.AlreadyInContextCount != 1 || output.OmittedCount != 2 {
		t.Fatalf("an issued row outside the display window was not counted: %#v", output)
	}
}

func TestTopicMemoryReadV4CountsDedupAndTokenOmissions(t *testing.T) {
	long := strings.Repeat("很长的解释", 100)
	provider := &recordingTopicSnapshotProvider{snapshot: serviceagent.AuthoritativeTopicMemorySnapshot{
		Records: []serviceagent.TopicMemoryRecord{
			topicRow("given", "tcp-framing", "已有", "已给出", 1, 1),
			topicRow("long", "tcp-framing", "太长", long, 2, 1),
			topicRow("short", "tcp-framing", "可用", "短记录", 3, 1),
		}, EligibleCount: 3,
	}}
	run := topicReadRun("tcp-framing")
	run.MemoryEvidence.Register(serviceagent.MemoryEvidenceEntry{
		Kind: serviceagent.MemoryEvidenceTopic, InternalID: "given", Version: 1, Owner: 1,
	})
	config := DefaultTopicMemoryReadConfig()
	config.MaxObservationTokens = 64
	output := readTopicV4(t, buildTopicReadV4(t, provider, run, config))
	if output.ReturnedCount != 1 || output.AlreadyInContextCount != 1 || output.OmittedCount != 1 || output.Complete {
		t.Fatalf("dedup or token omission was miscounted: %#v", output)
	}
}

func TestTopicMemoryReadV4StopsBeforeProviderWithoutAKey(t *testing.T) {
	provider := &recordingTopicSnapshotProvider{}
	output := readTopicV4(t, buildTopicReadV4(t, provider, topicReadRun(), DefaultTopicMemoryReadConfig()))
	if provider.calls.Load() != 0 || output.Status != TopicReadStatusNoAuthoritativeTopic ||
		output.Complete || output.NextAction != topicReadV4NextSearch {
		t.Fatalf("no-key output = %#v, provider calls = %d", output, provider.calls.Load())
	}
}

func TestTopicMemoryReadV4DoesNotChangeTheFrozenV3Schema(t *testing.T) {
	const frozenV3 = "7ea7d425bb43afca9e137e6f1cb364dd99519040295ac1d027162000979c1e07"
	v3, err := TopicMemoryReadSchemaDigest()
	if err != nil {
		t.Fatal(err)
	}
	v4, err := TopicMemoryReadV4SchemaDigest()
	if err != nil {
		t.Fatal(err)
	}
	if v3 != frozenV3 {
		t.Fatalf("v3 schema drifted: %s", v3)
	}
	if v4 == v3 {
		t.Fatal("v4 did not receive an isolated schema")
	}
}

func TestTopicMemoryReadV4InputStillOffersNoAuthorityHandles(t *testing.T) {
	registry, err := NewTopicMemoryReadV4Registry(&recordingTopicSnapshotProvider{}, DefaultTopicMemoryReadConfig())
	if err != nil {
		t.Fatal(err)
	}
	tools, err := registry.Tools(context.Background(), topicReadRun("tcp-framing"))
	if err != nil {
		t.Fatal(err)
	}
	info, err := tools[0].Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	params, err := info.ParamsOneOf.ToOpenAPIV3()
	if err != nil {
		t.Fatal(err)
	}
	blob, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"user_id", "session", "topic_key", "topicKey", "memory_id", "kinds", "top_k", "query"} {
		if strings.Contains(string(blob), forbidden) {
			t.Fatalf("v4 input schema offers %q: %s", forbidden, blob)
		}
	}
}

func TestTopicMemoryReadV4ConfigAndIdentityFailClosed(t *testing.T) {
	if _, err := NewTopicMemoryReadV4Registry(nil, DefaultTopicMemoryReadConfig()); err == nil {
		t.Fatal("v4 accepted a nil snapshot provider")
	}
	provider := &recordingTopicSnapshotProvider{}
	registry, err := NewTopicMemoryReadV4Registry(provider, TopicMemoryReadConfig{
		MaxCalls: 1, MaxResults: 5, MaxObservationTokens: 1500, Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Tools(context.Background(), serviceagent.RunContext{
		SessionID: "s", MemoryEvidence: serviceagent.NewMemoryEvidenceRegistry(),
	}); err == nil {
		t.Fatal("v4 built without a user identity")
	}
}
