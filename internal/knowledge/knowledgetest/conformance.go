// Package knowledgetest holds the behavioural contract every knowledge.Store
// implementation must satisfy. It lives in a normal package, not a _test file, so
// both the in-memory store and the GORM store can be held to the same rules
// instead of each growing its own interpretation of ownership and versioning.
package knowledgetest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/knowledge"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

// Factory builds a fresh, empty store that enforces the given per-user ceilings.
// Taking the limits is part of the contract: a store that could not be asked to
// run a smaller quota could not be shown to enforce one either, so the quota
// rules below are tested against both backends instead of one.
type Factory func(t *testing.T, limits knowledge.Limits) knowledge.Store

// Suite runs the shared knowledge.Store contract.
func Suite(t *testing.T, newStore Factory) {
	t.Helper()
	ctx := context.Background()
	// Every subtest that is not about quotas runs on the production ceilings, so
	// passing them proves nothing about the quota path - and failing to pass the
	// quota subtests cannot be blamed on a small limit either.
	unlimited := func(t *testing.T) knowledge.Store { return newStore(t, knowledge.DefaultLimits()) }

	t.Run("collection round trip and creation order", func(t *testing.T) {
		store := unlimited(t)
		owner, err := store.CreateCollection(ctx, &domain.Collection{UserID: "user-1", Title: "  运行手册  "})
		if err != nil {
			t.Fatal(err)
		}
		if owner.Title != "运行手册" {
			t.Fatalf("title = %q, want it trimmed", owner.Title)
		}
		second, err := store.CreateCollection(ctx, &domain.Collection{UserID: "user-1", Title: "第二本"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreateCollection(ctx, &domain.Collection{UserID: "user-2", Title: "别人的"}); err != nil {
			t.Fatal(err)
		}
		stored, err := store.GetCollection(ctx, "user-1", owner.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.ID != owner.ID || stored.UserID != "user-1" {
			t.Fatalf("round trip mismatch: %#v", stored)
		}
		listed, err := store.ListCollections(ctx, "user-1")
		if err != nil {
			t.Fatal(err)
		}
		if len(listed) != 2 || listed[0].ID != owner.ID || listed[1].ID != second.ID {
			ids := make([]string, 0, len(listed))
			for _, item := range listed {
				ids = append(ids, item.ID)
			}
			t.Fatalf("list = %v, want creation order", ids)
		}
		if other, err := store.ListCollections(ctx, "user-2"); err != nil || len(other) != 1 {
			t.Fatalf("other user's list = %d items, err %v", len(other), err)
		}
	})

	t.Run("invalid collections are refused", func(t *testing.T) {
		store := unlimited(t)
		if _, err := store.CreateCollection(ctx, nil); !errors.Is(err, repository.ErrInvalidArgument) {
			t.Errorf("nil collection error = %v", err)
		}
		for _, title := range []string{"", "   ", strings.Repeat("t", 201)} {
			if _, err := store.CreateCollection(ctx, &domain.Collection{UserID: "user-1", Title: title}); !errors.Is(err, repository.ErrInvalidArgument) {
				t.Errorf("title %q error = %v, want ErrInvalidArgument", title, err)
			}
		}
		if _, err := store.CreateCollection(ctx, &domain.Collection{UserID: " ", Title: "ok"}); !errors.Is(err, repository.ErrInvalidArgument) {
			t.Errorf("missing owner error = %v", err)
		}
		if _, err := store.GetCollection(ctx, "user-1", "  "); !errors.Is(err, repository.ErrInvalidArgument) {
			t.Errorf("blank id error = %v", err)
		}
		if _, err := store.ListCollections(ctx, ""); !errors.Is(err, repository.ErrInvalidArgument) {
			t.Errorf("blank user error = %v", err)
		}
	})

	t.Run("a foreign collection reads as missing", func(t *testing.T) {
		store := unlimited(t)
		mine, err := store.CreateCollection(ctx, &domain.Collection{UserID: "user-1", Title: "mine"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.GetCollection(ctx, "user-2", mine.ID); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("error = %v, want ErrNotFound", err)
		}
		if err := store.EnsureReadable(ctx, "user-2", []string{mine.ID}); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("EnsureReadable error = %v, want ErrNotFound", err)
		}
		if err := store.EnsureReadable(ctx, "user-1", []string{mine.ID, "nope"}); !errors.Is(err, repository.ErrNotFound) {
			t.Fatal("one unknown id in a scope must refuse the whole scope")
		}
		if err := store.EnsureReadable(ctx, "user-1", []string{mine.ID}); err != nil {
			t.Fatalf("owner error = %v", err)
		}
		if err := store.EnsureReadable(ctx, "user-1", nil); err != nil {
			t.Fatalf("an empty scope should be a no-op, got %v", err)
		}
	})

	t.Run("documents carry chunks in order and only for their owner", func(t *testing.T) {
		store := unlimited(t)
		collection := mustCollection(t, store, "user-1")
		created, err := store.CreateDocument(ctx, "user-1", &domain.Document{
			CollectionID: collection.ID, Title: "redis.md", Format: domain.DocumentFormatMarkdown,
		}, []domain.DocumentChunk{
			{Text: "第二段内容", Section: "Redis / AOF", StartLine: 5, EndLine: 7},
			{Text: "第一段内容", Section: "Redis", StartLine: 1, EndLine: 3},
		})
		if err != nil {
			t.Fatal(err)
		}
		if created.Version != 1 || created.ChunkCount != 2 {
			t.Fatalf("created document = %#v", created)
		}
		chunks, err := store.ChunksFor(ctx, "user-1", []string{collection.ID})
		if err != nil {
			t.Fatal(err)
		}
		if len(chunks) != 2 {
			t.Fatalf("chunks = %d", len(chunks))
		}
		for index, item := range chunks {
			if item.Order != index+1 {
				t.Fatalf("chunk %d came back with order %d; the store owns the sequence", index, item.Order)
			}
			if item.DocumentID != created.ID || item.CollectionID != collection.ID || item.Version != 1 {
				t.Fatalf("chunk provenance lost: %#v", item)
			}
			if item.ID == "" {
				t.Fatal("chunk has no id")
			}
		}
		if _, err := store.ChunksFor(ctx, "user-2", []string{collection.ID}); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("cross-user chunks error = %v", err)
		}
		if _, err := store.GetDocument(ctx, "user-2", created.ID); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("cross-user document error = %v", err)
		}
		if _, err := store.CreateDocument(ctx, "user-1", &domain.Document{CollectionID: "nope", Title: "x", Format: domain.DocumentFormatText}, []domain.DocumentChunk{{Text: "a"}}); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("unknown collection error = %v", err)
		}
	})

	t.Run("invalid documents and chunks are refused", func(t *testing.T) {
		store := unlimited(t)
		collection := mustCollection(t, store, "user-1")
		if _, err := store.CreateDocument(ctx, "user-1", nil, nil); !errors.Is(err, repository.ErrInvalidArgument) {
			t.Errorf("nil document error = %v", err)
		}
		if _, err := store.CreateDocument(ctx, "user-1", &domain.Document{CollectionID: collection.ID, Title: "x", Format: domain.DocumentFormat("pdf")}, []domain.DocumentChunk{{Text: "a"}}); !errors.Is(err, repository.ErrInvalidArgument) {
			t.Errorf("unknown format error = %v", err)
		}
		if _, err := store.CreateDocument(ctx, "user-1", &domain.Document{CollectionID: collection.ID, Title: "x", Format: domain.DocumentFormatText}, nil); !errors.Is(err, repository.ErrInvalidArgument) {
			t.Errorf("no chunks error = %v", err)
		}
		if _, err := store.CreateDocument(ctx, "user-1", &domain.Document{CollectionID: collection.ID, Title: "  ", Format: domain.DocumentFormatText}, []domain.DocumentChunk{{Text: "a"}}); !errors.Is(err, repository.ErrInvalidArgument) {
			t.Errorf("blank title error = %v", err)
		}
		if _, err := store.CreateDocument(ctx, "user-1", &domain.Document{CollectionID: collection.ID, Title: "x", Format: domain.DocumentFormatText}, []domain.DocumentChunk{{Text: "   "}}); !errors.Is(err, repository.ErrInvalidArgument) {
			t.Errorf("blank chunk text error = %v", err)
		}
		if _, err := store.CreateDocument(ctx, "", &domain.Document{CollectionID: collection.ID, Title: "x", Format: domain.DocumentFormatText}, []domain.DocumentChunk{{Text: "a"}}); !errors.Is(err, repository.ErrInvalidArgument) {
			t.Errorf("missing principal error = %v", err)
		}
	})

	t.Run("replacing content retires the previous version", func(t *testing.T) {
		store := unlimited(t)
		collection := mustCollection(t, store, "user-1")
		created, err := store.CreateDocument(ctx, "user-1", &domain.Document{
			CollectionID: collection.ID, Title: "redis.md", Format: domain.DocumentFormatMarkdown,
		}, []domain.DocumentChunk{{Text: "旧版本：everysec 是默认策略"}, {Text: "旧版本第二段"}})
		if err != nil {
			t.Fatal(err)
		}
		replaced, err := store.ReplaceDocumentContent(ctx, "user-1", created.ID, replacement(domain.DocumentFormatMarkdown, "新版本：只谈 RDB"))
		if err != nil {
			t.Fatal(err)
		}
		if replaced.Version != created.Version+1 {
			t.Fatalf("version = %d, want %d", replaced.Version, created.Version+1)
		}
		if replaced.ChunkCount != 1 {
			t.Fatalf("chunk count = %d, want 1", replaced.ChunkCount)
		}
		chunks, err := store.ChunksFor(ctx, "user-1", []string{collection.ID})
		if err != nil {
			t.Fatal(err)
		}
		if len(chunks) != 1 || chunks[0].Text != "新版本：只谈 RDB" {
			t.Fatalf("retrievable chunks after a rewrite = %#v", chunks)
		}
		for _, item := range chunks {
			if item.Version != replaced.Version {
				t.Fatalf("chunk %q still carries version %d", item.ID, item.Version)
			}
			if strings.Contains(item.Text, "旧版本") {
				t.Fatalf("retired content is still retrievable: %q", item.Text)
			}
		}
		// Chunk ids stay unique across versions even though only one version is live.
		if chunks[0].ID == created.ID {
			t.Fatal("chunk id collided with the document id")
		}
		if _, err := store.ReplaceDocumentContent(ctx, "user-2", created.ID, replacement(domain.DocumentFormatText, "x")); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("cross-user replace error = %v", err)
		}
		if _, err := store.ReplaceDocumentContent(ctx, "user-1", "document-404", replacement(domain.DocumentFormatText, "x")); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("unknown document error = %v", err)
		}
		if _, err := store.ReplaceDocumentContent(ctx, "user-1", created.ID, replacement(domain.DocumentFormat("exe"), "x")); !errors.Is(err, repository.ErrInvalidArgument) {
			t.Fatalf("bad format error = %v", err)
		}
	})

	t.Run("documents list per collection in creation order", func(t *testing.T) {
		store := unlimited(t)
		first := mustCollection(t, store, "user-1")
		second := mustCollection(t, store, "user-1")
		theirs := mustCollection(t, store, "user-2")
		earlier := mustDocument(t, store, "user-1", first.ID, "earlier.md", "第一段内容")
		later := mustDocument(t, store, "user-1", first.ID, "later.md", "第二段内容")
		mustDocument(t, store, "user-1", second.ID, "elsewhere.md", "别的手册的内容")
		mustDocument(t, store, "user-2", theirs.ID, "private.md", "别人的内容")

		listed, err := store.ListDocuments(ctx, "user-1", first.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(listed) != 2 || listed[0].ID != earlier.ID || listed[1].ID != later.ID {
			ids := make([]string, 0, len(listed))
			for _, item := range listed {
				ids = append(ids, item.ID)
			}
			t.Fatalf("listing = %v, want this collection's two documents in creation order", ids)
		}
		for index, item := range listed {
			want := earlier
			if index == 1 {
				want = later
			}
			if item.Title != want.Title || item.Format != want.Format || item.Version != 1 || item.ChunkCount != 1 {
				t.Fatalf("listed document %d = %#v, want the stored fields of %#v", index, item, want)
			}
			if item.CollectionID != first.ID {
				t.Fatalf("listed document %d belongs to %q, want the requested collection %q", index, item.CollectionID, first.ID)
			}
		}
		// A collection the caller does not own is not listable, and does not
		// report its own existence either.
		if _, err := store.ListDocuments(ctx, "user-2", first.ID); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("cross-user listing error = %v", err)
		}
		if _, err := store.ListDocuments(ctx, "user-1", "collection-404"); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("unknown collection listing error = %v", err)
		}
		if _, err := store.ListDocuments(ctx, "user-1", ""); !errors.Is(err, repository.ErrInvalidArgument) {
			t.Fatalf("empty collection id error = %v", err)
		}
		empty, err := store.ListDocuments(ctx, "user-1", mustCollection(t, store, "user-1").ID)
		if err != nil || len(empty) != 0 {
			t.Fatalf("listing an empty owned collection = (%v, %v), want no documents and no error", empty, err)
		}
	})

	t.Run("deleting a document retires every chunk it had", func(t *testing.T) {
		store := unlimited(t)
		collection := mustCollection(t, store, "user-1")
		keep := mustDocument(t, store, "user-1", collection.ID, "keep.md", "保留下来的内容")
		gone, err := store.CreateDocument(ctx, "user-1", &domain.Document{
			CollectionID: collection.ID, Title: "gone.md", Format: domain.DocumentFormatMarkdown,
		}, []domain.DocumentChunk{{Text: "第一版将被删除的内容"}, {Text: "第一版第二段"}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.ReplaceDocumentContent(ctx, "user-1", gone.ID, replacement(domain.DocumentFormatMarkdown, "第二版也将被删除")); err != nil {
			t.Fatal(err)
		}
		if err := store.DeleteDocument(ctx, "user-2", gone.ID); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("cross-user delete error = %v, want the document to be missing for its non-owner", err)
		}
		if err := store.DeleteDocument(ctx, "user-1", "document-404"); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("delete of an unknown document error = %v", err)
		}
		if err := store.DeleteDocument(ctx, "user-1", ""); !errors.Is(err, repository.ErrInvalidArgument) {
			t.Fatalf("empty document id error = %v", err)
		}
		if err := store.DeleteDocument(ctx, "user-1", gone.ID); err != nil {
			t.Fatal(err)
		}
		if err := store.DeleteDocument(ctx, "user-1", gone.ID); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("second delete error = %v, want a deleted document to be gone", err)
		}
		if _, err := store.GetDocument(ctx, "user-1", gone.ID); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("GetDocument after delete error = %v", err)
		}
		listed, err := store.ListDocuments(ctx, "user-1", collection.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(listed) != 1 || listed[0].ID != keep.ID {
			t.Fatalf("listing after delete = %#v, want only the surviving document", listed)
		}
		chunks, err := store.ChunksFor(ctx, "user-1", []string{collection.ID})
		if err != nil {
			t.Fatal(err)
		}
		if len(chunks) != 1 || chunks[0].Text != "保留下来的内容" {
			t.Fatalf("retrievable chunks after delete = %#v, want only the survivor's", chunks)
		}
		for _, chunk := range chunks {
			if chunk.DocumentID == gone.ID {
				t.Fatalf("a deleted document still contributes chunk %q", chunk.ID)
			}
		}
		if titles, err := store.DocumentTitles(ctx, "user-1", []string{gone.ID, keep.ID}); err != nil {
			t.Fatal(err)
		} else if _, exists := titles[gone.ID]; exists {
			t.Fatal("a deleted document still resolves a title")
		}
	})

	t.Run("titles resolve only within the owner", func(t *testing.T) {
		store := unlimited(t)
		mine := mustCollection(t, store, "user-1")
		theirs := mustCollection(t, store, "user-2")
		first, err := store.CreateDocument(ctx, "user-1", &domain.Document{CollectionID: mine.ID, Title: "我的文档", Format: domain.DocumentFormatText}, []domain.DocumentChunk{{Text: "a"}})
		if err != nil {
			t.Fatal(err)
		}
		second, err := store.CreateDocument(ctx, "user-2", &domain.Document{CollectionID: theirs.ID, Title: "别人的文档", Format: domain.DocumentFormatText}, []domain.DocumentChunk{{Text: "b"}})
		if err != nil {
			t.Fatal(err)
		}
		titles, err := store.DocumentTitles(ctx, "user-1", []string{first.ID, second.ID, "missing"})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(titles, map[string]string{first.ID: "我的文档"}) {
			t.Fatalf("titles = %#v, want only the caller's own document", titles)
		}
		if empty, err := store.DocumentTitles(ctx, "user-1", nil); err != nil || len(empty) != 0 {
			t.Fatalf("empty request = %#v, %v", empty, err)
		}
	})

	t.Run("recorded titles and content are unique per user", func(t *testing.T) {
		store := unlimited(t)
		mine := mustCollection(t, store, "user-1")
		alsoMine := mustCollection(t, store, "user-1")
		theirs := mustCollection(t, store, "user-2")
		note := []domain.DocumentChunk{{Text: "everysec 是默认策略"}}
		if _, err := store.CreateDocument(ctx, "user-1", measured("redis.md", domain.DocumentFormatMarkdown, mine.ID, "持久化笔记"), note); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreateDocument(ctx, "user-1", measured("redis.md", domain.DocumentFormatMarkdown, mine.ID, "别的正文"), note); !errors.Is(err, knowledge.ErrDocumentNameTaken) {
			t.Errorf("same title twice error = %v, want the title to be taken", err)
		}
		// The ceiling v2 sets is per user, so it crosses collections on purpose: a
		// second copy of the same notes in another handbook is still a second copy.
		if _, err := store.CreateDocument(ctx, "user-1", measured("redis.md", domain.DocumentFormatMarkdown, alsoMine.ID, "又一篇"), note); !errors.Is(err, knowledge.ErrDocumentNameTaken) {
			t.Errorf("same title in another of the caller's collections = %v", err)
		}
		if _, err := store.CreateDocument(ctx, "user-1", measured("kafka.md", domain.DocumentFormatMarkdown, mine.ID, "持久化笔记"), note); !errors.Is(err, knowledge.ErrDocumentContentDuplicate) {
			t.Errorf("same content under a new title error = %v, want the content to be stored already", err)
		}
		// Another user with the same title and the same bytes is a different
		// document: uniqueness is owned per user, never globally.
		if _, err := store.CreateDocument(ctx, "user-2", measured("redis.md", domain.DocumentFormatMarkdown, theirs.ID, "持久化笔记"), note); err != nil {
			t.Errorf("cross-user identical document = %v, want it allowed", err)
		}
		// Rows whose metadata was never recorded cannot be duplicates of anything,
		// including each other: "unknown" must not collapse into one match.
		for index := 0; index < 2; index++ {
			document := &domain.Document{CollectionID: mine.ID, Title: fmt.Sprintf("未测量-%d.md", index), Format: domain.DocumentFormatMarkdown}
			if _, err := store.CreateDocument(ctx, "user-1", document, note); err != nil {
				t.Errorf("unmeasured document %d = %v, want it allowed", index, err)
			}
		}
	})

	t.Run("half-written metadata is refused", func(t *testing.T) {
		store := unlimited(t)
		collection := mustCollection(t, store, "user-1")
		chunks := []domain.DocumentChunk{{Text: "内容"}}
		for name, document := range map[string]*domain.Document{
			"truncated hash": {CollectionID: collection.ID, Title: "a.md", Format: domain.DocumentFormatText, ContentHash: "abcd"},
			"non-hex hash":   {CollectionID: collection.ID, Title: "b.md", Format: domain.DocumentFormatText, ContentHash: strings.Repeat("g", 64)},
			"negative size":  {CollectionID: collection.ID, Title: "c.md", Format: domain.DocumentFormatText, FileSize: -1},
		} {
			if _, err := store.CreateDocument(ctx, "user-1", document, chunks); !errors.Is(err, repository.ErrInvalidArgument) {
				t.Errorf("%s error = %v, want the metadata refused rather than stored as if it measured something", name, err)
			}
		}
	})

	t.Run("the count ceiling refuses the next document", func(t *testing.T) {
		store := newStore(t, knowledge.Limits{MaxDocuments: 2, MaxTotalBytes: knowledge.MaxTotalBytesPerUser})
		collection := mustCollection(t, store, "user-1")
		other := mustCollection(t, store, "user-2")
		for index := 0; index < 2; index++ {
			if _, err := store.CreateDocument(ctx, "user-1", measured(fmt.Sprintf("a%d.md", index), domain.DocumentFormatText, collection.ID, fmt.Sprintf("第一篇内容%d", index)), []domain.DocumentChunk{{Text: "内容"}}); err != nil {
				t.Fatalf("document %d = %v", index, err)
			}
		}
		if _, err := store.CreateDocument(ctx, "user-1", measured("third.md", domain.DocumentFormatText, collection.ID, "第三篇内容"), []domain.DocumentChunk{{Text: "内容"}}); !errors.Is(err, knowledge.ErrDocumentCountLimit) {
			t.Errorf("third document error = %v, want the count ceiling to refuse it", err)
		}
		// The ceiling is per user, so a full user cannot block an empty one.
		if _, err := store.CreateDocument(ctx, "user-2", measured("mine.md", domain.DocumentFormatText, other.ID, "别人的内容"), []domain.DocumentChunk{{Text: "内容"}}); err != nil {
			t.Errorf("another user's first document = %v, want it allowed", err)
		}
		// Deleting spends nothing: the ceiling is measured against the live rows.
		listed, err := store.ListDocuments(ctx, "user-1", collection.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.DeleteDocument(ctx, "user-1", listed[0].ID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreateDocument(ctx, "user-1", measured("third.md", domain.DocumentFormatText, collection.ID, "第三篇内容"), []domain.DocumentChunk{{Text: "内容"}}); err != nil {
			t.Errorf("document after one was deleted = %v, want a freed slot to be usable", err)
		}
	})

	t.Run("the byte ceiling refuses the larger upload", func(t *testing.T) {
		store := newStore(t, knowledge.Limits{MaxDocuments: knowledge.MaxDocumentsPerUser, MaxTotalBytes: 40})
		collection := mustCollection(t, store, "user-1")
		if _, err := store.CreateDocument(ctx, "user-1", measured("a.md", domain.DocumentFormatText, collection.ID, "1234567890"), []domain.DocumentChunk{{Text: "内容"}}); err != nil {
			t.Fatal(err)
		}
		usage, err := store.UsageByUser(ctx, "user-1")
		if err != nil {
			t.Fatal(err)
		}
		if usage.TotalBytes != 10 || usage.DocumentCount != 1 || usage.Unmeasured != 0 {
			t.Fatalf("usage = %#v, want the one measured document counted at its own size", usage)
		}
		if _, err := store.CreateDocument(ctx, "user-1", measured("b.md", domain.DocumentFormatText, collection.ID, strings.Repeat("x", 31)), []domain.DocumentChunk{{Text: "内容"}}); !errors.Is(err, knowledge.ErrStorageLimit) {
			t.Errorf("41 bytes into a 40 byte ceiling = %v, want the storage limit", err)
		}
		// Exactly up to the ceiling is still inside it: v2 refuses past the total,
		// not at it.
		if _, err := store.CreateDocument(ctx, "user-1", measured("c.md", domain.DocumentFormatText, collection.ID, strings.Repeat("y", 30)), []domain.DocumentChunk{{Text: "内容"}}); err != nil {
			t.Errorf("exactly at the ceiling = %v, want it allowed", err)
		}
	})

	t.Run("a rewrite spends only the difference", func(t *testing.T) {
		store := newStore(t, knowledge.Limits{MaxDocuments: knowledge.MaxDocumentsPerUser, MaxTotalBytes: 40})
		collection := mustCollection(t, store, "user-1")
		created, err := store.CreateDocument(ctx, "user-1", measured("a.md", domain.DocumentFormatText, collection.ID, strings.Repeat("x", 40)), []domain.DocumentChunk{{Text: "内容"}})
		if err != nil {
			t.Fatal(err)
		}
		// The document is already at the whole ceiling; a rewrite that grows it by
		// one byte has to fail, because its own bytes are released only as the new
		// ones are charged.
		if _, err := store.ReplaceDocumentContent(ctx, "user-1", created.ID, measuredReplacement(strings.Repeat("x", 41), 41)); !errors.Is(err, knowledge.ErrStorageLimit) {
			t.Errorf("growing a full-size document = %v, want the storage limit", err)
		}
		replaced, err := store.ReplaceDocumentContent(ctx, "user-1", created.ID, measuredReplacement(strings.Repeat("x", 12), 12))
		if err != nil {
			t.Fatal(err)
		}
		if replaced.Version != created.Version+1 {
			t.Fatalf("version = %d, want the rewrite to advance from %d", replaced.Version, created.Version)
		}
		usage, err := store.UsageByUser(ctx, "user-1")
		if err != nil {
			t.Fatal(err)
		}
		if usage.TotalBytes != 12 {
			t.Fatalf("usage after the rewrite = %#v, want the old size released", usage)
		}
	})

	t.Run("identical content advances nothing", func(t *testing.T) {
		store := unlimited(t)
		collection := mustCollection(t, store, "user-1")
		created, err := store.CreateDocument(ctx, "user-1", measured("a.md", domain.DocumentFormatMarkdown, collection.ID, "第一版内容"), []domain.DocumentChunk{{Text: "第一版内容"}, {Text: "第二段"}})
		if err != nil {
			t.Fatal(err)
		}
		replaced, err := store.ReplaceDocumentContent(ctx, "user-1", created.ID, measuredReplacement("第一版内容", 15))
		if !errors.Is(err, knowledge.ErrDocumentUnchanged) {
			t.Fatalf("rewrite with the same bytes = %v, want it reported as unchanged", err)
		}
		if replaced == nil {
			t.Fatal("an unchanged rewrite must still hand back the live document")
		}
		if replaced.Version != created.Version || replaced.ChunkCount != created.ChunkCount {
			t.Fatalf("unchanged rewrite returned %#v, want the live document untouched", replaced)
		}
		live, err := store.GetDocument(ctx, "user-1", created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if live.Version != created.Version {
			t.Fatalf("stored version = %d, want no version bump for identical content", live.Version)
		}
		chunks, err := store.ChunksFor(ctx, "user-1", []string{collection.ID})
		if err != nil {
			t.Fatal(err)
		}
		if len(chunks) != 2 || chunks[0].Text != "第一版内容" {
			t.Fatalf("chunks after an unchanged rewrite = %#v, want the live chunk set intact", chunks)
		}
		// Different bytes still rewrite normally.
		if _, err := store.ReplaceDocumentContent(ctx, "user-1", created.ID, measuredReplacement("第二版内容", 15)); err != nil {
			t.Fatalf("a real rewrite = %v", err)
		}
		// A rewrite of a document whose own hash was never recorded cannot be judged
		// identical to anything, so it advances rather than pretending to know.
		legacy, err := store.CreateDocument(ctx, "user-1", &domain.Document{CollectionID: collection.ID, Title: "legacy.md", Format: domain.DocumentFormatText}, []domain.DocumentChunk{{Text: "旧文档"}})
		if err != nil {
			t.Fatal(err)
		}
		advanced, err := store.ReplaceDocumentContent(ctx, "user-1", legacy.ID, measuredReplacement("旧文档", 9))
		if err != nil {
			t.Fatalf("rewrite of an unmeasured document = %v, want it to advance", err)
		}
		if advanced.Version != legacy.Version+1 {
			t.Fatalf("version = %d, want %d", advanced.Version, legacy.Version+1)
		}
	})

	t.Run("usage is per user and says what it did not measure", func(t *testing.T) {
		store := unlimited(t)
		mine := mustCollection(t, store, "user-1")
		theirs := mustCollection(t, store, "user-2")
		if usage, err := store.UsageByUser(ctx, "user-1"); err != nil || usage != (knowledge.Usage{}) {
			t.Fatalf("empty usage = %#v, %v", usage, err)
		}
		if _, err := store.CreateDocument(ctx, "user-1", measured("a.md", domain.DocumentFormatText, mine.ID, "12345"), []domain.DocumentChunk{{Text: "内容"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreateDocument(ctx, "user-1", &domain.Document{CollectionID: mine.ID, Title: "unmeasured.md", Format: domain.DocumentFormatText}, []domain.DocumentChunk{{Text: "内容"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreateDocument(ctx, "user-2", measured("private.md", domain.DocumentFormatText, theirs.ID, "xxxxxxxxxxxxxxxxxxxx"), []domain.DocumentChunk{{Text: "内容"}}); err != nil {
			t.Fatal(err)
		}
		usage, err := store.UsageByUser(ctx, "user-1")
		if err != nil {
			t.Fatal(err)
		}
		if usage.DocumentCount != 2 || usage.TotalBytes != 5 || usage.Unmeasured != 1 {
			t.Fatalf("usage = %#v, want two documents, five measured bytes and one unmeasured row", usage)
		}
		if _, err := store.UsageByUser(ctx, ""); !errors.Is(err, repository.ErrInvalidArgument) {
			t.Errorf("usage without a principal = %v", err)
		}
	})

	t.Run("canceled contexts do no work", func(t *testing.T) {
		store := unlimited(t)
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		assertCanceled := func(name string, err error) {
			t.Helper()
			if !errors.Is(err, context.Canceled) {
				t.Errorf("%s error = %v, want context.Canceled", name, err)
			}
		}
		_, err := store.CreateCollection(canceled, &domain.Collection{UserID: "user-1", Title: "x"})
		assertCanceled("CreateCollection", err)
		_, err = store.ListCollections(canceled, "user-1")
		assertCanceled("ListCollections", err)
		_, err = store.GetCollection(canceled, "user-1", "c")
		assertCanceled("GetCollection", err)
		_, err = store.CreateDocument(canceled, "user-1", &domain.Document{CollectionID: "c", Title: "t", Format: domain.DocumentFormatText}, []domain.DocumentChunk{{Text: "a"}})
		assertCanceled("CreateDocument", err)
		_, err = store.GetDocument(canceled, "user-1", "d")
		assertCanceled("GetDocument", err)
		_, err = store.ReplaceDocumentContent(canceled, "user-1", "d", replacement(domain.DocumentFormatText, "a"))
		assertCanceled("ReplaceDocumentContent", err)
		_, err = store.UsageByUser(canceled, "user-1")
		assertCanceled("UsageByUser", err)
		_, err = store.ChunksFor(canceled, "user-1", []string{"c"})
		assertCanceled("ChunksFor", err)
		_, err = store.DocumentTitles(canceled, "user-1", []string{"d"})
		assertCanceled("DocumentTitles", err)
		assertCanceled("EnsureReadable", store.EnsureReadable(canceled, "user-1", []string{"c"}))
		if listed, err := store.ListCollections(ctx, "user-1"); err != nil || len(listed) != 0 {
			t.Fatalf("a canceled write still stored %d collections (%v)", len(listed), err)
		}
	})
}

func mustDocument(t *testing.T, store knowledge.Store, userID, collectionID, title, text string) *domain.Document {
	t.Helper()
	document, err := store.CreateDocument(context.Background(), userID, &domain.Document{
		CollectionID: collectionID, Title: title, Format: domain.DocumentFormatMarkdown,
	}, []domain.DocumentChunk{{Text: text}})
	if err != nil {
		t.Fatalf("create document %q: %v", title, err)
	}
	return document
}

// measured builds an upload request that carries the byte metadata ingestion
// records: the hash identifies the content and the size is what the storage
// quota spends. The hash is computed here rather than through the chunker's
// helper because a store must not depend on how a document was normalized - it
// only has to honour what it was told.
func measured(title string, format domain.DocumentFormat, collectionID, content string) *domain.Document {
	return &domain.Document{
		CollectionID: collectionID,
		Title:        title,
		Format:       format,
		ContentHash:  contentHash(content),
		FileSize:     int64(len(content)),
	}
}

// replacement is a rewrite request that carries no byte metadata: it stands for
// the shape a store handled before hashing existed, and for a caller that has
// nothing to declare. A store cannot call such a rewrite identical to anything.
func replacement(format domain.DocumentFormat, texts ...string) knowledge.DocumentReplacement {
	chunks := make([]domain.DocumentChunk, 0, len(texts))
	for _, text := range texts {
		chunks = append(chunks, domain.DocumentChunk{Text: text})
	}
	return knowledge.DocumentReplacement{Format: format, Chunks: chunks}
}

// measuredReplacement is the rewrite form of measured: same content hashes to
// the same value, different content does not.
func measuredReplacement(content string, size int64) knowledge.DocumentReplacement {
	if size == 0 {
		size = int64(len(content))
	}
	return knowledge.DocumentReplacement{
		Format:      domain.DocumentFormatText,
		ContentHash: contentHash(content),
		FileSize:    size,
		Chunks:      []domain.DocumentChunk{{Text: content}},
	}
}

func contentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func mustCollection(t *testing.T, store knowledge.Store, userID string) *domain.Collection {
	t.Helper()
	collection, err := store.CreateCollection(context.Background(), &domain.Collection{UserID: userID, Title: "手册 " + userID})
	if err != nil {
		t.Fatal(err)
	}
	return collection
}
