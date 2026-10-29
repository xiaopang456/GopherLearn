package gormmysql

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gorm.io/gorm"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/knowledge"
	"github.com/xiaopang456/GopherLearn/internal/knowledge/knowledgetest"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

// newKnowledgeStore opens a throwaway database, migrates both schemas and hands
// back the knowledge store plus its handle.
func newKnowledgeStore(t *testing.T) (knowledge.Store, *gorm.DB) {
	return newKnowledgeStoreWithLimits(t, knowledge.DefaultLimits())
}

// newKnowledgeStoreWithLimits is the same with the per-user ceilings pinned, so
// the shared contract can reach a quota boundary with two documents.
func newKnowledgeStoreWithLimits(t *testing.T, limits knowledge.Limits) (knowledge.Store, *gorm.DB) {
	t.Helper()
	db := openPlainTestDB(t, filepath.Join(t.TempDir(), "knowledge.db"))
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := MigrateKnowledge(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return NewKnowledgeRepository(db).WithLimits(limits), db
}

// TestKnowledgeRepositoryContract runs the same behavioural suite the in-memory
// store passes, so the two implementations cannot drift on ownership or version.
func TestKnowledgeRepositoryContract(t *testing.T) {
	newStore := func(t *testing.T, limits knowledge.Limits) knowledge.Store {
		store, _ := newKnowledgeStoreWithLimits(t, limits)
		return store
	}
	knowledgetest.Suite(t, newStore)
	knowledgetest.LifecycleSuite(t, newStore)
}

func TestKnowledgeSchemaShapes(t *testing.T) {
	_, db := newKnowledgeStore(t)
	for _, table := range []string{"knowledge_collections", "knowledge_documents", "knowledge_chunks"} {
		if !db.Migrator().HasTable(table) {
			t.Errorf("table %q missing", table)
		}
	}
	var definitions []string
	if err := db.Raw("SELECT sql FROM sqlite_master WHERE type = 'index' AND sql IS NOT NULL").Scan(&definitions).Error; err != nil {
		t.Fatal(err)
	}
	sort.Strings(definitions)
	joined := strings.Join(definitions, " | ")
	want := []string{
		"CREATE INDEX `idx_collections_user` ON `knowledge_collections`(`user_id`)",
		"CREATE INDEX `idx_documents_collection` ON `knowledge_documents`(`collection_id`)",
		"CREATE INDEX `idx_chunks_collection` ON `knowledge_chunks`(`collection_id`)",
		"CREATE INDEX `idx_chunks_document_seq` ON `knowledge_chunks`(`document_id`,`seq`)",
	}
	for _, statement := range want {
		if !strings.Contains(joined, statement) {
			t.Errorf("missing index DDL %q\napplied: %s", statement, joined)
		}
	}
}

func TestKnowledgeMigrateIsAdditiveAndIdempotent(t *testing.T) {
	store, db := newKnowledgeStore(t)
	ctx := context.Background()
	collection, err := store.CreateCollection(ctx, &domain.Collection{UserID: "user-1", Title: "手册"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateDocument(ctx, "user-1", &domain.Document{CollectionID: collection.ID, Title: "a.md", Format: domain.DocumentFormatMarkdown}, []domain.DocumentChunk{{Text: "内容"}}); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 2; round++ {
		if err := MigrateKnowledge(ctx, db); err != nil {
			t.Fatalf("repeat migrate round %d: %v", round, err)
		}
	}
	var collections, chunks int64
	if err := db.Raw("SELECT COUNT(*) FROM knowledge_collections").Scan(&collections).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Raw("SELECT COUNT(*) FROM knowledge_chunks").Scan(&chunks).Error; err != nil {
		t.Fatal(err)
	}
	if collections != 1 || chunks != 1 {
		t.Fatalf("repeating the migration changed rows: collections=%d chunks=%d", collections, chunks)
	}
	if err := MigrateKnowledge(nil, db); !errors.Is(err, repository.ErrInvalidArgument) {
		t.Fatalf("nil context error = %v", err)
	}
	if err := MigrateKnowledge(ctx, nil); !errors.Is(err, repository.ErrInvalidArgument) {
		t.Fatalf("nil db error = %v", err)
	}
}

// TestCreateDocumentRollsBackWhenChunksFail proves a document is never visible
// without its chunks: a half-written document would answer from an empty scope.
func TestCreateDocumentRollsBackWhenChunksFail(t *testing.T) {
	store, db := newKnowledgeStore(t)
	ctx := context.Background()
	collection, err := store.CreateCollection(ctx, &domain.Collection{UserID: "user-1", Title: "手册"})
	if err != nil {
		t.Fatal(err)
	}
	failCreateOn(t, db, "knowledge_chunks")

	if _, err := store.CreateDocument(ctx, "user-1", &domain.Document{
		CollectionID: collection.ID, Title: "a.md", Format: domain.DocumentFormatMarkdown,
	}, []domain.DocumentChunk{{Text: "第一段"}, {Text: "第二段"}}); err == nil {
		t.Fatal("CreateDocument succeeded despite the injected chunk failure")
	}
	var documents, chunks int64
	if err := db.Raw("SELECT COUNT(*) FROM knowledge_documents").Scan(&documents).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Raw("SELECT COUNT(*) FROM knowledge_chunks").Scan(&chunks).Error; err != nil {
		t.Fatal(err)
	}
	if documents != 0 || chunks != 0 {
		t.Fatalf("rolled-back upload left documents=%d chunks=%d", documents, chunks)
	}
}

// TestReplaceDocumentRollsBackTogether proves the retire-and-replace step is
// atomic: a failure after the old chunks were dropped must not leave the
// document with no content at all.
func TestReplaceDocumentRollsBackTogether(t *testing.T) {
	store, db := newKnowledgeStore(t)
	ctx := context.Background()
	collection, err := store.CreateCollection(ctx, &domain.Collection{UserID: "user-1", Title: "手册"})
	if err != nil {
		t.Fatal(err)
	}
	document, err := store.CreateDocument(ctx, "user-1", &domain.Document{
		CollectionID: collection.ID, Title: "a.md", Format: domain.DocumentFormatMarkdown,
	}, []domain.DocumentChunk{{Text: "旧内容"}})
	if err != nil {
		t.Fatal(err)
	}
	failCreateOn(t, db, "knowledge_chunks")

	if _, err := store.ReplaceDocumentContent(ctx, "user-1", document.ID, knowledge.DocumentReplacement{
		Format: domain.DocumentFormatMarkdown,
		Chunks: []domain.DocumentChunk{{Text: "新内容"}},
	}); err == nil {
		t.Fatal("ReplaceDocumentContent succeeded despite the injected failure")
	}
	chunks, err := store.ChunksFor(ctx, "user-1", []string{collection.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 || chunks[0].Text != "旧内容" {
		t.Fatalf("chunks after a rolled-back rewrite = %#v", chunks)
	}
	kept, err := store.GetDocument(ctx, "user-1", document.ID)
	if err != nil {
		t.Fatal(err)
	}
	if kept.Version != document.Version {
		t.Fatalf("version advanced to %d although the rewrite was rolled back", kept.Version)
	}
}

// TestVersionedChunksAreNotRecalled checks the store half of the claim that a
// rewrite stops answering from the old text.
func TestVersionedChunksAreNotRecalled(t *testing.T) {
	store, _ := newKnowledgeStore(t)
	ctx := context.Background()
	collection, err := store.CreateCollection(ctx, &domain.Collection{UserID: "user-1", Title: "手册"})
	if err != nil {
		t.Fatal(err)
	}
	document, err := store.CreateDocument(ctx, "user-1", &domain.Document{
		CollectionID: collection.ID, Title: "a.md", Format: domain.DocumentFormatMarkdown,
	}, []domain.DocumentChunk{{Text: "everysec 是默认值"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReplaceDocumentContent(ctx, "user-1", document.ID, knowledge.DocumentReplacement{
		Format: domain.DocumentFormatMarkdown,
		Chunks: []domain.DocumentChunk{{Text: "改为 no 策略"}},
	}); err != nil {
		t.Fatal(err)
	}
	var retired int64
	if err := store.(*KnowledgeRepository).db.Raw("SELECT COUNT(*) FROM knowledge_chunks WHERE text LIKE ?", "%everysec%").Scan(&retired).Error; err != nil {
		t.Fatal(err)
	}
	if retired != 0 {
		t.Fatalf("%d retired chunk rows are still in the table", retired)
	}
	chunks, err := store.ChunksFor(ctx, "user-1", []string{collection.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 || chunks[0].Text != "改为 no 策略" {
		t.Fatalf("live chunks = %#v", chunks)
	}
}

func TestKnowledgeRepositoryRejectsInvalidArguments(t *testing.T) {
	store, _ := newKnowledgeStore(t)
	ctx := context.Background()
	if _, err := store.CreateCollection(ctx, nil); !errors.Is(err, repository.ErrInvalidArgument) {
		t.Fatalf("nil collection error = %v", err)
	}
	if _, err := store.CreateDocument(ctx, "", &domain.Document{CollectionID: "c", Title: "t", Format: domain.DocumentFormatText}, []domain.DocumentChunk{{Text: "a"}}); !errors.Is(err, repository.ErrInvalidArgument) {
		t.Fatalf("no principal error = %v", err)
	}
	if _, err := store.ChunksFor(ctx, "user-1", nil); err != nil {
		t.Fatalf("empty scope error = %v, want an empty result", err)
	}
}
