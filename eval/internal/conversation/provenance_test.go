package conversation

import (
	"context"
	"path/filepath"
	"testing"
)

func TestProvenanceExcludesEphemeralInfrastructure(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	s, err := offlineSettings(root, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	before, err := collectProvenance(context.Background(), root, s, validSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	s.Config.MysqlPassword = "different-synthetic-secret"
	s.Config.MysqlPort++
	s.Config.StorageDir = t.TempDir()
	after, err := collectProvenance(context.Background(), root, s, validSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	if before.SnapshotSHA256 != after.SnapshotSHA256 || before.ConfigSHA256 != after.ConfigSHA256 || len(before.CheckoutSHA256) != 64 || len(before.Revision) != 40 {
		t.Fatal("unstable provenance")
	}
	s.Config.SummaryTriggerMessages++
	changed, err := collectProvenance(context.Background(), root, s, validSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	if changed.ConfigSHA256 == after.ConfigSHA256 {
		t.Fatal("functional change omitted from fingerprint")
	}
}
