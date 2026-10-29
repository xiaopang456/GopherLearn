package gormmysql

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/knowledge"
)

// legacyDocumentRecord is knowledge_documents as it stood before ingestion
// recorded any byte metadata. It exists only so this test can build the old
// schema, plant a row in it, and then run the real migration over the top.
type legacyDocumentRecord struct {
	ID           string    `gorm:"column:id;type:varchar(96);primaryKey"`
	CollectionID string    `gorm:"column:collection_id;type:varchar(96);not null"`
	Title        string    `gorm:"column:title;type:varchar(200);not null"`
	Format       string    `gorm:"column:format;type:varchar(16);not null"`
	Version      int       `gorm:"column:version;not null;default:1"`
	ChunkCount   int       `gorm:"column:chunk_count;not null;default:0"`
	CreatedAt    time.Time `gorm:"column:created_at"`
	UpdatedAt    time.Time `gorm:"column:updated_at"`
}

func (legacyDocumentRecord) TableName() string { return "knowledge_documents" }

// TestKnowledgeMetadataMigrationKeepsLegacyRowsHonest is the old-data case: an
// existing deployment has documents with no hash and no size, and the upgrade
// must neither lose them nor invent measurements for them.
//
// What "invent" would mean concretely: a missing size read back as zero bytes
// hands that document free space inside the storage quota, and a missing hash
// compared with == matches every other missing hash, so a second upload of any
// unmeasured text would be refused as a duplicate of the first. Both are asserted
// against here rather than argued.
func TestKnowledgeMetadataMigrationKeepsLegacyRowsHonest(t *testing.T) {
	ctx := context.Background()
	db := openPlainTestDB(t, filepath.Join(t.TempDir(), "legacy.db"))
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	// The pre-upgrade schema: collections and chunks as they are today, documents
	// as they were.
	if err := db.AutoMigrate(&CollectionRecord{}, &DocumentChunkRecord{}, &legacyDocumentRecord{}); err != nil {
		t.Fatal(err)
	}
	store := NewKnowledgeRepository(db)
	collection, err := store.CreateCollection(ctx, &domain.Collection{UserID: "user-1", Title: "手册"})
	if err != nil {
		t.Fatal(err)
	}
	stamped := time.Now().UTC()
	if err := db.Create(&legacyDocumentRecord{
		ID: "document-legacy", CollectionID: collection.ID, Title: "旧文档.md",
		Format: string(domain.DocumentFormatMarkdown), Version: 1, ChunkCount: 1,
		CreatedAt: stamped, UpdatedAt: stamped,
	}).Error; err != nil {
		t.Fatal(err)
	}

	// The real additive migration runs over a table that already holds a row.
	if err := MigrateKnowledge(ctx, db); err != nil {
		t.Fatalf("additive migration over legacy rows: %v", err)
	}

	legacy, err := store.GetDocument(ctx, "user-1", "document-legacy")
	if err != nil {
		t.Fatalf("the legacy document must survive the migration: %v", err)
	}
	if legacy.ContentHash != "" || legacy.FileSize != 0 {
		t.Fatalf("legacy document read back as %#v, want no invented hash or size", legacy)
	}

	usage, err := store.UsageByUser(ctx, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if usage.DocumentCount != 1 || usage.TotalBytes != 0 || usage.Unmeasured != 1 {
		t.Fatalf("usage over a legacy row = %#v, want it counted and named as unmeasured", usage)
	}

	// A title is measured even on an old row, so it still collides: the uniqueness
	// rule must not silently stop covering pre-existing documents.
	if _, err := store.CreateDocument(ctx, "user-1", &domain.Document{
		CollectionID: collection.ID, Title: "旧文档.md", Format: domain.DocumentFormatMarkdown,
		ContentHash: probeHash(), FileSize: 10,
	}, []domain.DocumentChunk{{Text: "同名的新文档"}}); err != knowledge.ErrDocumentNameTaken {
		t.Errorf("same title as a legacy row = %v, want the title still taken", err)
	}

	// An unmeasured row has no hash, so it cannot be a content duplicate of
	// anything: the new document has to be filed, not refused against a blank.
	if _, err := store.CreateDocument(ctx, "user-1", &domain.Document{
		CollectionID: collection.ID, Title: "新文档.md", Format: domain.DocumentFormatMarkdown,
		ContentHash: probeHash(), FileSize: 10,
	}, []domain.DocumentChunk{{Text: "内容"}}); err != nil {
		t.Errorf("upload beside an unmeasured row = %v, want it allowed", err)
	}

	// The gap this design accepts, pinned so it stays visible rather than becoming
	// folklore: the storage ceiling is measured in bytes, and a row whose bytes
	// were never recorded contributes nothing to that sum. So a legacy document of
	// unknown size sits inside a 100-byte ceiling while the measured total says the
	// user is at 90 bytes - the ceiling is honest about what it knows and silent
	// about what it does not. Closing the gap means re-uploading, not guessing.
	tight := NewKnowledgeRepository(db).WithLimits(knowledge.Limits{MaxDocuments: 100, MaxTotalBytes: 100})
	if _, err := tight.CreateDocument(ctx, "user-1", &domain.Document{
		CollectionID: collection.ID, Title: "新度量.md", Format: domain.DocumentFormatText, FileSize: 90,
	}, []domain.DocumentChunk{{Text: "内容"}}); err != nil {
		t.Fatalf("90 bytes against a 100-byte ceiling beside an unmeasured row = %v", err)
	}
	after, err := tight.UsageByUser(ctx, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	// Three live rows: the legacy one, the 10-byte probe from the duplicate checks
	// and this 90-byte one - but only 100 measured bytes, because the legacy row
	// contributed nothing it could be asked about.
	if after.DocumentCount != 3 || after.TotalBytes != 100 || after.Unmeasured != 1 {
		t.Fatalf("usage = %#v, want three rows, 100 measured bytes and one unmeasured", after)
	}
	if _, err := tight.CreateDocument(ctx, "user-1", &domain.Document{
		CollectionID: collection.ID, Title: "挤不下.md", Format: domain.DocumentFormatText, FileSize: 20,
	}, []domain.DocumentChunk{{Text: "内容"}}); err != knowledge.ErrStorageLimit {
		t.Errorf("another 20 bytes = %v, want the ceiling to refuse against the measured total", err)
	}
}

// probeHash returns a well-formed 64-character digest so a document is refused
// only for the reason a test is asserting, never for the shape of its metadata.
func probeHash() string {
	sum := sha256.Sum256([]byte("probe"))
	return hex.EncodeToString(sum[:])
}

// TestLegacyDocumentsStaySearchableThroughTheRealMigration is the old-data case that
// reading the row back cannot answer: retrieval filters on columns, so a compatibility
// rule that only lives in the record-to-domain mapping leaves every pre-existing
// document in the database and missing from every answer. It walks the actual sequence -
// old schema, old row, production MigrateKnowledge, then ChunksFor - and then updates
// the document through the lifecycle, which is where the second half of the rule bites.
func TestLegacyDocumentsStaySearchableThroughTheRealMigration(t *testing.T) {
	ctx := context.Background()
	db := openPlainTestDB(t, filepath.Join(t.TempDir(), "legacy-search.db"))
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&CollectionRecord{}, &DocumentChunkRecord{}, &legacyDocumentRecord{}); err != nil {
		t.Fatal(err)
	}
	store := NewKnowledgeRepository(db)
	collection, err := store.CreateCollection(ctx, &domain.Collection{UserID: "user-1", Title: "手册"})
	if err != nil {
		t.Fatal(err)
	}
	stamped := time.Now().UTC()
	if err := db.Create(&legacyDocumentRecord{
		ID: "document-legacy", CollectionID: collection.ID, Title: "旧文档.md",
		Format: string(domain.DocumentFormatMarkdown), Version: 3, ChunkCount: 1,
		CreatedAt: stamped, UpdatedAt: stamped,
	}).Error; err != nil {
		t.Fatal(err)
	}
	// A legacy document can sit at any version number; 3 keeps a test from passing
	// because version and active_version happen to be the same small integer.
	if err := db.Create(&DocumentChunkRecord{
		ID: "document-legacy-v3-c1", DocumentID: "document-legacy", CollectionID: collection.ID,
		Version: 3, Seq: 1, Text: "旧文档的原文内容", StartLine: 1, EndLine: 1, CreatedAt: stamped,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := MigrateKnowledge(ctx, db); err != nil {
		t.Fatalf("production migration over a legacy row: %v", err)
	}

	chunks, err := store.ChunksFor(ctx, "user-1", []string{collection.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 || chunks[0].Text != "旧文档的原文内容" || chunks[0].Version != 3 {
		t.Fatalf("retrieval after the migration = %#v, want the legacy document still serving its own version", chunks)
	}

	// Updating a legacy document has to work, and afterwards the rows it replaced have
	// to be gone: CompleteIndexing learns the superseded version from what the row was
	// serving, not from an active_version column that was 0 all along.
	replaced, err := store.ReplaceDocumentContent(ctx, "user-1", "document-legacy", knowledge.DocumentReplacement{
		Format:      domain.DocumentFormatMarkdown,
		ContentHash: probeHash(),
		FileSize:    12,
		Chunks:      []domain.DocumentChunk{{Text: "改写后的原文内容"}},
	})
	if err != nil {
		t.Fatalf("update of a legacy document: %v", err)
	}
	if replaced.Version != 4 || replaced.ActiveVersion != 4 || replaced.IndexStatus != domain.DocumentStatusReady {
		t.Fatalf("updated legacy document = %#v, want version 4 published", replaced)
	}
	after, err := store.ChunksFor(ctx, "user-1", []string{collection.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].Text != "改写后的原文内容" || after[0].Version != 4 {
		t.Fatalf("retrieval after the update = %#v, want only the new version serving", after)
	}
	var orphans int64
	if err := db.Raw("SELECT COUNT(*) FROM knowledge_chunks WHERE document_id = ? AND version = 3", "document-legacy").
		Scan(&orphans).Error; err != nil {
		t.Fatal(err)
	}
	if orphans != 0 {
		t.Fatalf("%d superseded legacy rows survived the update; they are unreachable and nothing will ever remove them", orphans)
	}
}

// TestPendingDocumentsAreNotSearchableDespiteTheLegacyRule is the other half: the
// compatibility reading applies to untracked rows only. A staged document of a real
// lifecycle write has no active version yet, and letting the same "serve what Version
// says" rule cover it would make an upload answer queries before its index exists.
func TestPendingDocumentsAreNotSearchableDespiteTheLegacyRule(t *testing.T) {
	ctx := context.Background()
	store, db := newKnowledgeStore(t)
	collection, err := store.CreateCollection(ctx, &domain.Collection{UserID: "user-1", Title: "手册"})
	if err != nil {
		t.Fatal(err)
	}
	staged, err := store.(knowledge.Lifecycle).StageDocument(ctx, "user-1", &domain.Document{
		CollectionID: collection.ID, Title: "暂存.md", Format: domain.DocumentFormatText,
		ContentHash: probeHash(), FileSize: 9,
	}, []domain.DocumentChunk{{Text: "暂存的正文"}})
	if err != nil {
		t.Fatal(err)
	}
	if staged.IndexStatus != domain.DocumentStatusPending || staged.ServingVersion() != 0 {
		t.Fatalf("staged document = %#v, want pending with no serving version", staged)
	}
	chunks, err := store.ChunksFor(ctx, "user-1", []string{collection.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 0 {
		t.Fatalf("a staged version is already searchable: %#v", chunks)
	}
	// The rows are on disk - that is what makes the publish cheap and the failure
	// recoverable - they are just not reachable through retrieval.
	var stored int64
	if err := db.Raw("SELECT COUNT(*) FROM knowledge_chunks WHERE document_id = ?", staged.ID).Scan(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored != 1 {
		t.Fatalf("%d staged chunk rows stored, want the text staged and merely unpublished", stored)
	}
}
