package knowledge

import (
	daoknowledge "GopherAI/dao/knowledge"
	"GopherAI/model"
	"context"
	"strings"
	"sync/atomic"
	"testing"
)

// TestO1DocumentServiceNotifiesOnlyAfterSuccess pins when the change notifier fires: after a
// successful write that leaves a document waiting (create, staged update with new content, retry,
// reindex) and never for deletes, unchanged updates or failures.
func TestO1DocumentServiceNotifiesOnlyAfterSuccess(t *testing.T) {
	storage, err := NewLocalFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	name := "gin.md"
	old := "# Gin\nold content\n"
	docs := func() []model.KnowledgeDocument {
		return []model.KnowledgeDocument{
			{ID: "doc-ready", UserID: 73, OriginalName: name, ActiveName: &name, ContentHash: ContentHash([]byte(old)), Version: 1, ActiveVersion: 1, IndexStatus: model.KnowledgeStatusReady},
			{ID: "doc-failed", UserID: 73, OriginalName: "f.md", Version: 1, IndexStatus: model.KnowledgeStatusFailed},
			{ID: "doc-busy", UserID: 73, OriginalName: "b.md", Version: 1, IndexStatus: model.KnowledgeStatusIndexing},
		}
	}
	setup := func(repository *fakeDocumentRepository) (*DocumentService, *atomic.Int64) {
		service := NewDocumentService(repository, storage, testKnowledgeLimits())
		var calls atomic.Int64
		service.UseChangeNotifier(func() { calls.Add(1) })
		return service, &calls
	}
	ctx := context.Background()

	t.Run("no hook behaves as before", func(t *testing.T) {
		service := NewDocumentService(&fakeDocumentRepository{}, storage, testKnowledgeLimits())
		if _, err := service.CreateDocument(ctx, 73, "plain.md", strings.NewReader("# plain\n")); err != nil {
			t.Fatalf("CreateDocument without a hook: %v", err)
		}
		service.UseChangeNotifier(func() { t.Fatal("a removed hook was called") })
		service.UseChangeNotifier(nil)
		if _, err := service.CreateDocument(ctx, 73, "plain2.md", strings.NewReader("# plain two\n")); err != nil {
			t.Fatalf("CreateDocument after removing the hook: %v", err)
		}
	})

	t.Run("create", func(t *testing.T) {
		service, calls := setup(&fakeDocumentRepository{})
		if _, err := service.CreateDocument(ctx, 73, "new.md", strings.NewReader("# new\n")); err != nil || calls.Load() != 1 {
			t.Fatalf("successful create: err=%v notifications=%d, want exactly 1", err, calls.Load())
		}
		// failures: invalid input, duplicate name, duplicate hash, limits, unreadable source
		for label, run := range map[string]func() error{
			"unsupported type": func() error {
				_, err := service.CreateDocument(ctx, 73, "x.pdf", strings.NewReader("x"))
				return err
			},
			"empty": func() error { _, err := service.CreateDocument(ctx, 73, "e.md", strings.NewReader("")); return err },
			"unreadable source": func() error {
				_, err := service.CreatePending(ctx, 73, "u.md", "md", errorReader{})
				return err
			},
		} {
			if err := run(); err == nil {
				t.Fatalf("%s: expected an error", label)
			}
		}
		existing := &model.KnowledgeDocument{ID: "other"}
		dup, dupCalls := setup(&fakeDocumentRepository{byName: existing})
		if _, err := dup.CreateDocument(ctx, 73, "dup.md", strings.NewReader("# dup\n")); err == nil {
			t.Fatal("duplicate name must fail")
		}
		hashed, hashCalls := setup(&fakeDocumentRepository{byHash: existing})
		if _, err := hashed.CreateDocument(ctx, 73, "dup2.md", strings.NewReader("# dup2\n")); err == nil {
			t.Fatal("duplicate hash must fail")
		}
		full, fullCalls := setup(&fakeDocumentRepository{usage: daoknowledge.Usage{DocumentCount: 3}})
		if _, err := full.CreateDocument(ctx, 73, "full.md", strings.NewReader("# full\n")); err == nil {
			t.Fatal("document limit must fail")
		}
		if calls.Load() != 1 || dupCalls.Load() != 0 || hashCalls.Load() != 0 || fullCalls.Load() != 0 {
			t.Fatalf("failed creates notified: %d (same service, want 1 in total), %d, %d, %d", calls.Load(), dupCalls.Load(), hashCalls.Load(), fullCalls.Load())
		}
	})

	t.Run("update", func(t *testing.T) {
		service, calls := setup(&fakeDocumentRepository{docs: docs()})
		if _, unchanged, err := service.UpdateDocument(ctx, 73, "doc-ready", name, strings.NewReader(old)); err != nil || !unchanged || calls.Load() != 0 {
			t.Fatalf("unchanged update: unchanged=%v err=%v notifications=%d, want 0", unchanged, err, calls.Load())
		}
		if _, _, err := service.UpdateDocument(ctx, 73, "doc-busy", "b.md", strings.NewReader("new")); err == nil || calls.Load() != 0 {
			t.Fatalf("update of a busy document: err=%v notifications=%d, want an error and 0", err, calls.Load())
		}
		if _, _, err := service.UpdateDocument(ctx, 73, "doc-ready", name, strings.NewReader("")); err == nil || calls.Load() != 0 {
			t.Fatalf("empty update: err=%v notifications=%d, want an error and 0", err, calls.Load())
		}
		if _, unchanged, err := service.UpdateDocument(ctx, 73, "doc-ready", name, strings.NewReader("new content")); err != nil || unchanged || calls.Load() != 1 {
			t.Fatalf("staged update: unchanged=%v err=%v notifications=%d, want exactly 1", unchanged, err, calls.Load())
		}
	})

	t.Run("retry", func(t *testing.T) {
		service, calls := setup(&fakeDocumentRepository{docs: docs()})
		if _, err := service.RetryDocument(ctx, 73, "doc-ready"); err == nil || calls.Load() != 0 {
			t.Fatalf("retry of a document that did not fail: err=%v notifications=%d, want an error and 0", err, calls.Load())
		}
		if _, err := service.RetryDocument(ctx, 73, "missing"); err == nil || calls.Load() != 0 {
			t.Fatalf("retry of a missing document: err=%v notifications=%d", err, calls.Load())
		}
		if _, err := service.RetryDocument(ctx, 73, "doc-failed"); err != nil || calls.Load() != 1 {
			t.Fatalf("successful retry: err=%v notifications=%d, want exactly 1", err, calls.Load())
		}
	})

	t.Run("reindex", func(t *testing.T) {
		service, calls := setup(&fakeDocumentRepository{docs: docs()})
		if _, err := service.ReindexDocument(ctx, 73, "missing"); err == nil || calls.Load() != 0 {
			t.Fatalf("reindex of a missing document: err=%v notifications=%d", err, calls.Load())
		}
		if _, err := service.ReindexDocument(ctx, 73, "doc-ready"); err != nil || calls.Load() != 1 {
			t.Fatalf("successful reindex: err=%v notifications=%d, want exactly 1", err, calls.Load())
		}
	})

	t.Run("delete", func(t *testing.T) {
		service, calls := setup(&fakeDocumentRepository{docs: docs()})
		if err := service.DeleteDocument(ctx, 73, "doc-ready"); err != nil || calls.Load() != 0 {
			t.Fatalf("delete: err=%v notifications=%d, want 0", err, calls.Load())
		}
	})

	t.Run("a panicking hook does not fail the committed write", func(t *testing.T) {
		service := NewDocumentService(&fakeDocumentRepository{}, storage, testKnowledgeLimits())
		service.UseChangeNotifier(func() { panic("hook bug") })
		if document, err := service.CreateDocument(ctx, 73, "boom.md", strings.NewReader("# boom\n")); err != nil || document == nil {
			t.Fatalf("create with a panicking hook: (%v, %v)", document, err)
		}
	})
}
