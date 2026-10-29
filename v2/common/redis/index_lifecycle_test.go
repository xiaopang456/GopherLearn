package redis

import (
	"context"
	"strings"
	"testing"

	redisCli "github.com/redis/go-redis/v9"
)

func TestManagedSearchIndexInventoryClassifiesOnlyExplicitVersions(t *testing.T) {
	families := ManagedSearchIndexFamilies()
	listed := []string{
		KnowledgeChunkIndexName, "gopherai_knowledge_chunks_v2_idx",
		EpisodeIndexName, "gopherai_episode_v1_idx",
		MemoryIndexName, "gopherai_memory_v1_idx",
		"gopherai_memory_v99_idx", "unrelated_idx",
	}
	legacy, unknown := classifyManagedIndexes(listed, families)
	if strings.Join(legacy, ",") != "gopherai_episode_v1_idx,gopherai_knowledge_chunks_v2_idx,gopherai_memory_v1_idx" {
		t.Fatalf("legacy=%v", legacy)
	}
	if len(unknown) != 1 || unknown[0] != "gopherai_memory_v99_idx" {
		t.Fatalf("unknown=%v", unknown)
	}
}

func TestValidateCurrentIndexRequiresPrefixSchemaCoverageAndIdleState(t *testing.T) {
	family := ManagedSearchIndexFamilies()[0]
	attributes := make([]redisCli.FTAttribute, 0, len(family.RequiredAttributes))
	for _, name := range family.RequiredAttributes {
		attributes = append(attributes, redisCli.FTAttribute{Identifier: name})
	}
	healthy := redisCli.FTInfoResult{
		IndexName: family.Current, IndexDefinition: redisCli.IndexDefinition{KeyType: "HASH", Prefixes: []string{family.KeyPrefix}},
		Attributes: attributes, NumDocs: 17, PercentIndexed: 1,
	}
	if problems := validateCurrentIndex(family, healthy, 17); len(problems) != 0 {
		t.Fatalf("healthy problems=%v", problems)
	}
	unhealthy := healthy
	unhealthy.IndexDefinition.Prefixes = []string{"wrong:"}
	unhealthy.Attributes = unhealthy.Attributes[:len(unhealthy.Attributes)-1]
	unhealthy.NumDocs = 16
	unhealthy.Indexing = 1
	unhealthy.PercentIndexed = 0.5
	unhealthy.HashIndexingFailures = 2
	if problems := validateCurrentIndex(family, unhealthy, 17); len(problems) < 5 {
		t.Fatalf("unhealthy problems=%v", problems)
	}
}

func TestSearchIndexAuditSafetyAndCleanupConfirmation(t *testing.T) {
	healthy := SearchIndexAuditReport{Families: []SearchIndexFamilyAudit{{Name: "knowledge", LegacyPresent: []string{"legacy"}}}}
	if !healthy.SafeToDrop() || len(healthy.LegacyIndexes()) != 1 {
		t.Fatalf("healthy report=%+v", healthy)
	}
	healthy.UnknownManaged = []string{"future"}
	if healthy.SafeToDrop() {
		t.Fatal("unknown managed index must block cleanup")
	}
	if _, err := DropAuditedLegacySearchIndexes(context.Background(), nil, "wrong"); err == nil {
		t.Fatal("invalid confirmation must fail before Redis access")
	}
}

func TestManagedSearchIndexRegistryHasNoSelfOrDuplicateLegacy(t *testing.T) {
	seen := make(map[string]string)
	for _, family := range ManagedSearchIndexFamilies() {
		if family.Current == "" || family.KeyPrefix == "" || family.NamePrefix == "" || len(family.RequiredAttributes) == 0 {
			t.Fatalf("incomplete family=%+v", family)
		}
		if prior := seen[family.Current]; prior != "" {
			t.Fatalf("current index %s repeated by %s and %s", family.Current, prior, family.Name)
		}
		seen[family.Current] = family.Name
		for _, legacy := range family.Legacy {
			if legacy == family.Current {
				t.Fatalf("family %s lists current index as legacy", family.Name)
			}
			if prior := seen[legacy]; prior != "" {
				t.Fatalf("index %s repeated by %s and %s", legacy, prior, family.Name)
			}
			seen[legacy] = family.Name
		}
	}
}
