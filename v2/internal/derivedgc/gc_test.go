package derivedgc

import (
	"reflect"
	"testing"
	"time"

	redisCommon "GopherAI/common/redis"
)

func TestParseManagedKeys(t *testing.T) {
	t.Parallel()
	user, identity, problem := parseKnowledgeKey(redisCommon.KnowledgeChunkKeyPrefix, "gopherai:knowledge:chunk:42:doc:3:chunk")
	if problem != "" || user != 42 || identity.(knowledgeIdentity) != (knowledgeIdentity{42, "doc", 3}) {
		t.Fatalf("unexpected knowledge parse: %d %#v %q", user, identity, problem)
	}
	user, identity, problem = parseExactKey(redisCommon.EpisodeKeyPrefix, "gopherai:episode:vec:9:episode")
	if problem != "" || user != 9 || identity.(exactIdentity) != (exactIdentity{9, "episode"}) {
		t.Fatalf("unexpected exact parse: %d %#v %q", user, identity, problem)
	}
	for _, key := range []string{"gopherai:knowledge:chunk:0:doc:1:chunk", "gopherai:knowledge:chunk:1::1:chunk", "gopherai:knowledge:chunk:1:doc:0:chunk"} {
		if _, _, problem := parseKnowledgeKey(redisCommon.KnowledgeChunkKeyPrefix, key); problem == "" {
			t.Fatalf("invalid key accepted: %s", key)
		}
	}
}

func TestReconcileKnowledgeKeepsOnlyActiveTuplesAndBlocksCountMismatch(t *testing.T) {
	t.Parallel()
	active := knowledgeIdentity{1, "active", 2}
	scanned := scannedFamily{Name: "knowledge", Prefix: redisCommon.KnowledgeChunkKeyPrefix, IndexDocs: 3, Keys: []scannedKey{
		{Key: "keep", UserID: 1, identity: active, Hash: true},
		{Key: "orphan-a", UserID: 2, identity: knowledgeIdentity{2, "old", 1}, Hash: true},
		{Key: "orphan-b", UserID: 2, identity: knowledgeIdentity{2, "old", 1}, Hash: true},
	}}
	report, candidates := reconcileKnowledge(scanned, map[knowledgeIdentity]int{active: 1})
	if !report.Ready() || report.Expected != 1 || report.Kept != 1 || report.Orphans != 2 || !reflect.DeepEqual(candidates, []string{"orphan-a", "orphan-b"}) {
		t.Fatalf("unexpected report: %#v %#v", report, candidates)
	}
	report, _ = reconcileKnowledge(scanned, map[knowledgeIdentity]int{active: 2})
	if report.Ready() || len(report.Blockers) == 0 {
		t.Fatalf("knowledge count mismatch did not block: %#v", report)
	}
}

func TestReconcileExactBlocksMissingAndUnexpectedTypes(t *testing.T) {
	t.Parallel()
	active := exactIdentity{1, "active"}
	scanned := scannedFamily{Name: "episode", Prefix: redisCommon.EpisodeKeyPrefix, IndexDocs: 2, Keys: []scannedKey{
		{Key: "keep", UserID: 1, identity: active, Hash: true},
		{Key: "orphan", UserID: 2, identity: exactIdentity{2, "old"}, Hash: true},
	}}
	report, candidates := reconcileExact(scanned, map[exactIdentity]struct{}{active: {}})
	if !report.Ready() || !reflect.DeepEqual(candidates, []string{"orphan"}) {
		t.Fatalf("unexpected exact report: %#v %#v", report, candidates)
	}
	scanned.Keys = append(scanned.Keys, scannedKey{Key: "wrong-type"})
	report, _ = reconcileExact(scanned, map[exactIdentity]struct{}{active: {}, {3, "missing"}: {}})
	if report.Ready() || report.UnexpectedTypes != 1 || len(report.Blockers) < 2 {
		t.Fatalf("unsafe exact view was accepted: %#v", report)
	}
}

func TestApplyPreconditionsFailBeforeAudit(t *testing.T) {
	t.Parallel()
	cleaner := &Cleaner{}
	if _, err := cleaner.Apply(t.Context(), time.Time{}, false, ApplyConfirmation); err == nil {
		t.Fatal("writers-stopped assertion was optional")
	}
	if _, err := cleaner.Apply(t.Context(), time.Time{}, true, "wrong"); err == nil {
		t.Fatal("wrong confirmation was accepted")
	}
}

func TestAuditFingerprintDetectsCandidateDrift(t *testing.T) {
	t.Parallel()
	left := audit{report: Report{Families: []FamilyReport{{Family: "memory", Expected: 1, RedisHashes: 2, IndexDocs: 2, Kept: 1, Orphans: 1}}}, candidates: []string{"old-a"}}
	right := audit{report: left.report, candidates: []string{"old-b"}}
	if left.fingerprint() == right.fingerprint() {
		t.Fatal("candidate drift did not change the audit fingerprint")
	}
}
