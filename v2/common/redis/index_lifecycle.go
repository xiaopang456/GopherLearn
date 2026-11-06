package redis

import (
	"context"
	"fmt"
	"sort"
	"strings"

	redisCli "github.com/redis/go-redis/v9"
)

const LegacyIndexCleanupConfirmation = "DROP_LEGACY_INDEX_METADATA"

type ManagedSearchIndexFamily struct {
	Name               string   `json:"name"`
	Current            string   `json:"current"`
	KeyPrefix          string   `json:"key_prefix"`
	NamePrefix         string   `json:"name_prefix"`
	Legacy             []string `json:"legacy"`
	RequiredAttributes []string `json:"required_attributes"`
}

type SearchIndexFamilyAudit struct {
	Name            string   `json:"name"`
	Current         string   `json:"current"`
	CurrentDocs     int      `json:"current_docs"`
	HashKeys        int      `json:"hash_keys"`
	LegacyPresent   []string `json:"legacy_present"`
	CurrentProblems []string `json:"current_problems"`
	LegacyProblems  []string `json:"legacy_problems"`
}

type SearchIndexAuditReport struct {
	Families       []SearchIndexFamilyAudit `json:"families"`
	UnknownManaged []string                 `json:"unknown_managed"`
}

func (r SearchIndexAuditReport) SafeToDrop() bool {
	if len(r.UnknownManaged) > 0 {
		return false
	}
	for _, family := range r.Families {
		if len(family.CurrentProblems) > 0 || len(family.LegacyProblems) > 0 {
			return false
		}
	}
	return true
}

func (r SearchIndexAuditReport) LegacyIndexes() []string {
	var indexes []string
	for _, family := range r.Families {
		indexes = append(indexes, family.LegacyPresent...)
	}
	sort.Strings(indexes)
	return indexes
}

func ManagedSearchIndexFamilies() []ManagedSearchIndexFamily {
	return []ManagedSearchIndexFamily{
		{
			Name: "knowledge", Current: KnowledgeChunkIndexName, KeyPrefix: KnowledgeChunkKeyPrefix,
			NamePrefix: "gopherai_knowledge_chunks_", Legacy: []string{"gopherai_knowledge_chunks_v2_idx"},
			RequiredAttributes: []string{KnowledgeFieldUserID, KnowledgeFieldChunkID, KnowledgeFieldSearchBigrams, KnowledgeFieldVector},
		},
		{
			Name: "episode", Current: EpisodeIndexName, KeyPrefix: EpisodeKeyPrefix,
			NamePrefix: "gopherai_episode_", Legacy: []string{"gopherai_episode_v1_idx"},
			RequiredAttributes: []string{EpisodeFieldUserID, EpisodeFieldEpisodeID, EpisodeFieldSearchBigrams, EpisodeFieldVector},
		},
		{
			Name: "memory", Current: MemoryIndexName, KeyPrefix: MemoryKeyPrefix,
			NamePrefix: "gopherai_memory_", Legacy: []string{"gopherai_memory_v1_idx"},
			RequiredAttributes: []string{MemoryFieldUserID, MemoryFieldMemoryID, MemoryFieldContent, MemoryFieldVector},
		},
		{
			// The summary segments are indexed like the other two: the same
			// instrument, under its own name. The audit knows it so that a
			// deployment whose index was never created is reported rather
			// than passed over.
			Name: "summary_segment", Current: SummarySegmentIndexName, KeyPrefix: SummarySegmentKeyPrefix,
			NamePrefix: "gopherai_summary_segment_",
			RequiredAttributes: []string{SummarySegmentFieldUserID, SummarySegmentFieldSegmentID,
				SummarySegmentFieldSearchBigrams, SummarySegmentFieldVector},
		},
	}
}

// ManagedSearchIndexFamilyByName returns the registered family with this name
// ("knowledge", "episode" or "memory"), so a caller that has to audit one
// family does not have to pick it out of the list by position.
func ManagedSearchIndexFamilyByName(name string) (ManagedSearchIndexFamily, bool) {
	for _, family := range ManagedSearchIndexFamilies() {
		if family.Name == name {
			return family, true
		}
	}
	return ManagedSearchIndexFamily{}, false
}

type SearchIndexInventory struct {
	LegacyPresent  []string
	UnknownManaged []string
}

// InspectManagedSearchIndexInventory is intentionally lightweight enough for
// startup diagnostics. It never scans keys and never mutates Redis.
func InspectManagedSearchIndexInventory(ctx context.Context, client *redisCli.Client) (SearchIndexInventory, error) {
	if client == nil {
		return SearchIndexInventory{}, fmt.Errorf("search index inventory: redis client is nil")
	}
	listed, err := listSearchIndexes(ctx, client)
	if err != nil {
		return SearchIndexInventory{}, err
	}
	legacy, unknown := classifyManagedIndexes(listed, ManagedSearchIndexFamilies())
	return SearchIndexInventory{LegacyPresent: legacy, UnknownManaged: unknown}, nil
}

func AuditManagedSearchIndexes(ctx context.Context, client *redisCli.Client) (SearchIndexAuditReport, error) {
	if client == nil {
		return SearchIndexAuditReport{}, fmt.Errorf("search index audit: redis client is nil")
	}
	families := ManagedSearchIndexFamilies()
	listed, err := listSearchIndexes(ctx, client)
	if err != nil {
		return SearchIndexAuditReport{}, err
	}
	_, unknown := classifyManagedIndexes(listed, families)
	present := searchIndexNameSet(listed)
	report := SearchIndexAuditReport{Families: make([]SearchIndexFamilyAudit, 0, len(families)), UnknownManaged: unknown}
	for _, family := range families {
		audit, err := auditSearchIndexFamily(ctx, client, family, present)
		if err != nil {
			return SearchIndexAuditReport{}, err
		}
		report.Families = append(report.Families, audit)
	}
	return report, nil
}

// AuditManagedSearchIndex reads one registered family with the same judgement
// AuditManagedSearchIndexes applies to it. It exists so a caller that cares
// about one family - the startup knowledge index audit, for example - does not
// have to keep its own idea of what a sound index is, and so the two readings
// cannot drift apart.
//
// It is read-only: it creates no index, drops nothing and changes no key.
func AuditManagedSearchIndex(ctx context.Context, client *redisCli.Client, family ManagedSearchIndexFamily) (SearchIndexFamilyAudit, error) {
	if client == nil {
		return SearchIndexFamilyAudit{}, fmt.Errorf("search index audit: redis client is nil")
	}
	listed, err := listSearchIndexes(ctx, client)
	if err != nil {
		return SearchIndexFamilyAudit{}, err
	}
	return auditSearchIndexFamily(ctx, client, family, searchIndexNameSet(listed))
}

func searchIndexNameSet(listed []string) map[string]struct{} {
	present := make(map[string]struct{}, len(listed))
	for _, index := range listed {
		present[index] = struct{}{}
	}
	return present
}

// auditSearchIndexFamily is the whole per-family reading: whether the current
// index exists at all, whether it is a well-formed view over the family's key
// prefix, whether the document count it reports is the number of HASH keys
// under that prefix, and which legacy indexes of the family are still present.
func auditSearchIndexFamily(ctx context.Context, client *redisCli.Client, family ManagedSearchIndexFamily, present map[string]struct{}) (SearchIndexFamilyAudit, error) {
	audit := SearchIndexFamilyAudit{Name: family.Name, Current: family.Current}
	before, err := countHashKeys(ctx, client, family.KeyPrefix)
	if err != nil {
		return audit, fmt.Errorf("count %s hashes: %w", family.Name, err)
	}
	if _, listed := present[family.Current]; !listed {
		// FT._LIST decides existence. A failed FT.INFO also covers a server
		// that is merely busy, and an operator needs to know which of the two
		// they are looking at: a missing index means retrieval finds nothing
		// even while every block of every document is still in Redis.
		audit.CurrentProblems = append(audit.CurrentProblems, fmt.Sprintf("current index %q does not exist", family.Current))
	} else {
		info, infoErr := client.FTInfo(ctx, family.Current).Result()
		if infoErr != nil {
			audit.CurrentProblems = append(audit.CurrentProblems, fmt.Sprintf("current index unavailable: %v", infoErr))
		} else {
			audit.CurrentDocs = info.NumDocs
			audit.CurrentProblems = append(audit.CurrentProblems, validateCurrentIndex(family, info, before)...)
		}
	}
	after, err := countHashKeys(ctx, client, family.KeyPrefix)
	if err != nil {
		return audit, fmt.Errorf("recount %s hashes: %w", family.Name, err)
	}
	audit.HashKeys = after
	if before != after {
		audit.CurrentProblems = append(audit.CurrentProblems, fmt.Sprintf("hash count changed during audit: %d -> %d", before, after))
	}
	for _, legacy := range family.Legacy {
		if _, ok := present[legacy]; !ok {
			continue
		}
		audit.LegacyPresent = append(audit.LegacyPresent, legacy)
		legacyInfo, err := client.FTInfo(ctx, legacy).Result()
		if err != nil {
			audit.LegacyProblems = append(audit.LegacyProblems, fmt.Sprintf("%s unavailable: %v", legacy, err))
			continue
		}
		if !sameSinglePrefix(legacyInfo.IndexDefinition.Prefixes, family.KeyPrefix) {
			audit.LegacyProblems = append(audit.LegacyProblems, fmt.Sprintf("%s has unexpected prefixes %v", legacy, legacyInfo.IndexDefinition.Prefixes))
		}
	}
	sort.Strings(audit.LegacyPresent)
	return audit, nil
}

// DropAuditedLegacySearchIndexes re-audits immediately before mutation and
// drops index metadata only. FT.DROPINDEX DD is deliberately never used, so
// the shared HASH documents remain available for rollback/recreation.
func DropAuditedLegacySearchIndexes(ctx context.Context, client *redisCli.Client, confirmation string) (SearchIndexAuditReport, error) {
	if confirmation != LegacyIndexCleanupConfirmation {
		return SearchIndexAuditReport{}, fmt.Errorf("legacy index cleanup confirmation is invalid")
	}
	report, err := AuditManagedSearchIndexes(ctx, client)
	if err != nil {
		return report, err
	}
	if !report.SafeToDrop() {
		return report, fmt.Errorf("managed search index audit is not safe to apply")
	}
	for _, index := range report.LegacyIndexes() {
		if err := client.FTDropIndex(ctx, index).Err(); err != nil {
			return report, fmt.Errorf("drop legacy index %s: %w", index, err)
		}
	}
	after, err := AuditManagedSearchIndexes(ctx, client)
	if err != nil {
		return after, err
	}
	if !after.SafeToDrop() || len(after.LegacyIndexes()) > 0 {
		return after, fmt.Errorf("legacy index cleanup postcondition failed")
	}
	return after, nil
}

func validateCurrentIndex(family ManagedSearchIndexFamily, info redisCli.FTInfoResult, hashKeys int) []string {
	var problems []string
	if info.IndexName != family.Current {
		problems = append(problems, fmt.Sprintf("reported name %q", info.IndexName))
	}
	if !strings.EqualFold(info.IndexDefinition.KeyType, "HASH") {
		problems = append(problems, fmt.Sprintf("key type is %q", info.IndexDefinition.KeyType))
	}
	if !sameSinglePrefix(info.IndexDefinition.Prefixes, family.KeyPrefix) {
		problems = append(problems, fmt.Sprintf("prefixes are %v", info.IndexDefinition.Prefixes))
	}
	attributes := make(map[string]struct{}, len(info.Attributes))
	for _, attribute := range info.Attributes {
		attributes[attribute.Identifier] = struct{}{}
	}
	for _, required := range family.RequiredAttributes {
		if _, ok := attributes[required]; !ok {
			problems = append(problems, fmt.Sprintf("required attribute %q is missing", required))
		}
	}
	if info.Indexing != 0 || info.PercentIndexed < 0.999999 {
		problems = append(problems, fmt.Sprintf("indexing=%d percent_indexed=%g", info.Indexing, info.PercentIndexed))
	}
	if info.HashIndexingFailures != 0 || info.IndexErrors.IndexingFailures != 0 {
		problems = append(problems, fmt.Sprintf("hash_failures=%d index_failures=%d", info.HashIndexingFailures, info.IndexErrors.IndexingFailures))
	}
	if info.NumDocs != hashKeys {
		problems = append(problems, fmt.Sprintf("num_docs=%d hash_keys=%d", info.NumDocs, hashKeys))
	}
	return problems
}

func sameSinglePrefix(prefixes []string, expected string) bool {
	return len(prefixes) == 1 && prefixes[0] == expected
}

func listSearchIndexes(ctx context.Context, client *redisCli.Client) ([]string, error) {
	values, err := client.Do(ctx, "FT._LIST").StringSlice()
	if err != nil {
		return nil, fmt.Errorf("list search indexes: %w", err)
	}
	sort.Strings(values)
	return values, nil
}

func classifyManagedIndexes(listed []string, families []ManagedSearchIndexFamily) ([]string, []string) {
	knownCurrent := make(map[string]struct{})
	knownLegacy := make(map[string]struct{})
	var namePrefixes []string
	for _, family := range families {
		knownCurrent[family.Current] = struct{}{}
		namePrefixes = append(namePrefixes, family.NamePrefix)
		for _, legacy := range family.Legacy {
			knownLegacy[legacy] = struct{}{}
		}
	}
	var legacy, unknown []string
	for _, index := range listed {
		if _, ok := knownLegacy[index]; ok {
			legacy = append(legacy, index)
			continue
		}
		if _, ok := knownCurrent[index]; ok {
			continue
		}
		for _, prefix := range namePrefixes {
			if strings.HasPrefix(index, prefix) {
				unknown = append(unknown, index)
				break
			}
		}
	}
	sort.Strings(legacy)
	sort.Strings(unknown)
	return legacy, unknown
}

func countHashKeys(ctx context.Context, client *redisCli.Client, prefix string) (int, error) {
	count := 0
	for cursor := uint64(0); ; {
		keys, next, err := client.ScanType(ctx, cursor, prefix+"*", 500, "hash").Result()
		if err != nil {
			return 0, err
		}
		count += len(keys)
		cursor = next
		if cursor == 0 {
			return count, nil
		}
	}
}
