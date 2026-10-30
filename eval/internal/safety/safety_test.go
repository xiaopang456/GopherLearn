package safety

import (
	"context"
	"errors"
	"strings"
	"testing"

	"GopherAI/common/rag"
)

// memKV is an in-memory KV. Other keys stand for the application's own data.
type memKV struct {
	data       map[string]string
	deleted    []int64
	deleteFail error
}

func newKV(keys ...string) *memKV {
	kv := &memKV{data: map[string]string{}}
	for _, k := range keys {
		kv.data[k] = "x"
	}
	return kv
}

func (m *memKV) DBSize(context.Context) (int64, error) { return int64(len(m.data)), nil }
func (m *memKV) Get(_ context.Context, key string) (string, bool, error) {
	v, ok := m.data[key]
	return v, ok, nil
}
func (m *memKV) Set(_ context.Context, key, value string) error { m.data[key] = value; return nil }
func (m *memKV) DeleteEvalData(_ context.Context, userID int64) error {
	if m.deleteFail != nil {
		return m.deleteFail
	}
	m.deleted = append(m.deleted, userID)
	delete(m.data, FingerprintKey)
	return nil
}

func fp() Fingerprint {
	return NewFingerprint("corpus-hash", "embed-model", 1024, rag.DefaultChunkerConfig(), 900001)
}

func TestFingerprintTracksEverythingTheIndexDependsOn(t *testing.T) {
	base := fp()
	if base.Hash() != fp().Hash() || len(base.Hash()) != 64 {
		t.Fatal("hash must be stable")
	}
	mutations := map[string]func(*Fingerprint){
		"corpus":     func(f *Fingerprint) { f.CorpusHash = "other" },
		"model":      func(f *Fingerprint) { f.EmbeddingModel = "other" },
		"dimension":  func(f *Fingerprint) { f.Dimension = 768 },
		"max tokens": func(f *Fingerprint) { f.Chunker.MaxTokens++ },
		"overlap":    func(f *Fingerprint) { f.Chunker.OverlapTokens++ },
		"max chunks": func(f *Fingerprint) { f.Chunker.MaxChunks++ },
		"content":    func(f *Fingerprint) { f.ContentVersion = "v-next" },
		"user":       func(f *Fingerprint) { f.EvalUserID++ },
	}
	for name, mutate := range mutations {
		changed := base
		mutate(&changed)
		if changed.Hash() == base.Hash() {
			t.Errorf("changing the %s did not change the fingerprint", name)
		}
	}
	if base.ContentVersion != rag.EmbeddingContentVersion {
		t.Error("the fingerprint must carry the product's EmbeddingContentVersion")
	}
}

func TestCheckAllowsAnEmptyDatabase(t *testing.T) {
	d, err := Check(context.Background(), newKV(), fp(), false)
	if err != nil || d != Build {
		t.Fatalf("decision %v, err %v", d, err)
	}
}

func TestCheckNeverWritesIntoADatabaseItDoesNotOwn(t *testing.T) {
	for _, rebuild := range []bool{false, true} {
		kv := newKV("user:1:profile", "session:abc")
		d, err := Check(context.Background(), kv, fp(), rebuild)
		if !errors.Is(err, ErrForeignDatabase) {
			t.Fatalf("rebuild=%v: err = %v, want ErrForeignDatabase", rebuild, err)
		}
		if d == Reuse {
			t.Fatalf("rebuild=%v: a foreign database must never be reused", rebuild)
		}
		if len(kv.deleted) != 0 || len(kv.data) != 2 {
			t.Fatalf("rebuild=%v: the guard touched a foreign database: %v", rebuild, kv.data)
		}
		if !strings.Contains(err.Error(), "dedicated") {
			t.Errorf("the error must say what to do: %v", err)
		}
	}
}

func TestCheckReusesAMatchingIndexAndRefusesAStaleOne(t *testing.T) {
	kv := newKV("chunk:1")
	kv.data[FingerprintKey] = fp().Hash()
	d, err := Check(context.Background(), kv, fp(), false)
	if err != nil || d != Reuse {
		t.Fatalf("decision %v, err %v", d, err)
	}

	other := fp()
	other.CorpusHash = "changed"
	d, err = Check(context.Background(), kv, other, false)
	if !errors.Is(err, ErrStaleIndex) || d == Reuse || !strings.Contains(err.Error(), "-rebuild") {
		t.Fatalf("decision %v, err %v", d, err)
	}
	if len(kv.deleted) != 0 {
		t.Fatal("a stale index must not be deleted without -rebuild")
	}
}

func TestRebuildReplacesOnlyAnEvaluationIndex(t *testing.T) {
	kv := newKV("chunk:1")
	kv.data[FingerprintKey] = "building:" + fp().Hash()
	other := fp()
	d, err := Check(context.Background(), kv, other, true)
	if err != nil || d != Build {
		t.Fatalf("decision %v, err %v", d, err)
	}
	if len(kv.deleted) != 1 || kv.deleted[0] != other.EvalUserID {
		t.Fatalf("deleted = %v", kv.deleted)
	}

	// -rebuild also replaces an index that matches: it is how a damaged one is repaired.
	kv2 := newKV("chunk:1")
	kv2.data[FingerprintKey] = fp().Hash()
	if d, err := Check(context.Background(), kv2, fp(), true); err != nil || d != Build || len(kv2.deleted) != 1 {
		t.Fatalf("decision %v, err %v, deleted %v", d, err, kv2.deleted)
	}

	kv3 := newKV("chunk:1")
	kv3.data[FingerprintKey] = "stale"
	kv3.deleteFail = errors.New("redis went away")
	if _, err := Check(context.Background(), kv3, fp(), true); err == nil || !strings.Contains(err.Error(), "remove the old evaluation index") {
		t.Fatalf("err = %v", err)
	}
}

func TestAnUnfinishedBuildIsRecognisedAsOursButNotReusable(t *testing.T) {
	kv := newKV()
	if err := Begin(context.Background(), kv, fp()); err != nil {
		t.Fatal(err)
	}
	// The build "crashed": chunks exist, the fingerprint is still the marker.
	kv.data["chunk:1"] = "x"
	d, err := Check(context.Background(), kv, fp(), false)
	if !errors.Is(err, ErrStaleIndex) || d == Reuse || !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("decision %v, err %v; a half-built index must not be reused", d, err)
	}
	if errors.Is(err, ErrForeignDatabase) {
		t.Fatal("a half-built evaluation index is ours, not foreign: -rebuild must be able to repair it")
	}
	if d, err := Check(context.Background(), kv, fp(), true); err != nil || d != Build {
		t.Fatalf("-rebuild must repair it: %v %v", d, err)
	}

	if err := Commit(context.Background(), kv, fp()); err != nil {
		t.Fatal(err)
	}
	if d, err := Check(context.Background(), kv, fp(), false); err != nil || d != Reuse {
		t.Fatalf("after Commit the index is reusable: %v %v", d, err)
	}
}

func TestDecisionStrings(t *testing.T) {
	if Build.String() != "build" || Reuse.String() != "reuse" {
		t.Fatal("decision strings")
	}
}
