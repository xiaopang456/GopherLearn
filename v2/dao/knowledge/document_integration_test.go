package knowledge

import (
	"GopherAI/internal/testdb"
	"GopherAI/model"
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestRepositoryMySQLIntegration(t *testing.T) {
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("DOC_B06_NAMESPACE") != "" {
		db = privateB06LegacySchema(t, dsn)
	}
	if err := db.AutoMigrate(&model.KnowledgeDocument{}); err != nil {
		t.Fatal(err)
	}

	userA := time.Now().UnixNano() & 0x3fffffffffffffff
	userB := userA + 1
	if err := db.Create(&model.User{ID: userA, Username: fmt.Sprintf("ragtest-%d", userA)}).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Unscoped().Where("user_id IN ?", []int64{userA, userB}).Delete(&model.KnowledgeDocument{})
		db.Unscoped().Delete(&model.User{}, userA)
	})
	repository := NewRepository(db)
	ctx := context.Background()

	first := testDocument("doc-a", userA, "gin.md", "a")
	if err := repository.Create(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := repository.Create(ctx, testDocument("doc-b", userA, "redis.md", "b")); err != nil {
		t.Fatal(err)
	}
	if err := repository.CreateWithinLimits(ctx, testDocument("doc-limit", userA, "mysql.md", "z"), 2, 100); !errors.Is(err, ErrDocumentLimit) {
		t.Fatalf("atomic document limit error = %v", err)
	}
	if err := repository.Create(ctx, testDocument("doc-c", userB, "gin.md", "c")); err != nil {
		t.Fatalf("different users may use the same name: %v", err)
	}
	if err := repository.Create(ctx, testDocument("doc-d", userA, "gin.md", "d")); !errors.Is(err, ErrActiveNameConflict) {
		t.Fatalf("same-user conflict error = %v", err)
	}
	if err := repository.Create(ctx, testDocument("doc-e", userA, "gorm.md", "a")); !errors.Is(err, ErrActiveHashConflict) {
		t.Fatalf("same-content conflict error = %v", err)
	}

	got, err := repository.GetByIDAndUserID(ctx, first.ID, userA)
	if err != nil || got.OriginalName != "gin.md" {
		t.Fatalf("get own document = (%+v, %v)", got, err)
	}
	if _, err := repository.GetByIDAndUserID(ctx, first.ID, userB); !errors.Is(err, ErrDocumentNotFound) {
		t.Fatalf("cross-user lookup error = %v", err)
	}
	usage, err := repository.UsageByUserID(ctx, userA)
	if err != nil || usage.DocumentCount != 2 || usage.TotalBytes != 2 {
		t.Fatalf("usage = (%+v, %v)", usage, err)
	}
	if err := repository.UpdateIndex(ctx, first.ID, userA, IndexUpdate{
		Status: model.KnowledgeStatusReady, ChunkCount: 3, ActiveVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}
	got, err = repository.GetByIDAndUserID(ctx, first.ID, userA)
	if err != nil || got.IndexStatus != model.KnowledgeStatusReady || got.ChunkCount != 3 {
		t.Fatalf("updated document = (%+v, %v)", got, err)
	}

	// Two workers may scan the same pending row, but the database compare-and-
	// swap permits exactly one of them to claim it.
	var wait sync.WaitGroup
	claims := make(chan bool, 2)
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			claimed, claimErr := repository.ClaimForIndexing(ctx, "doc-b", userA, 1)
			if claimErr != nil {
				t.Errorf("claim pending document: %v", claimErr)
			}
			claims <- claimed
		}()
	}
	wait.Wait()
	close(claims)
	claimCount := 0
	for claimed := range claims {
		if claimed {
			claimCount++
		}
	}
	if claimCount != 1 {
		t.Fatalf("successful concurrent claims = %d, want 1", claimCount)
	}
	if err := repository.CompleteIndexing(ctx, "doc-b", userA, 1, model.KnowledgeStatusFailed, 0, "embedding: operation failed"); err != nil {
		t.Fatal(err)
	}
	if err := repository.RetryFailed(ctx, "doc-b", userA); err != nil {
		t.Fatal(err)
	}
	claimed, err := repository.ClaimForIndexing(ctx, "doc-b", userA, 1)
	if err != nil || !claimed {
		t.Fatalf("claim retried document = (%v, %v)", claimed, err)
	}
	if err := repository.CompleteIndexing(ctx, "doc-b", userA, 1, model.KnowledgeStatusReady, 4, ""); err != nil {
		t.Fatal(err)
	}

	interrupted := testDocument("doc-interrupted", userA, "mq.md", "interrupted")
	interrupted.IndexStatus = model.KnowledgeStatusIndexing
	if err := repository.Create(ctx, interrupted); err != nil {
		t.Fatal(err)
	}
	recovered, err := repository.RecoverInterruptedIndexing(ctx)
	if err != nil || recovered != 1 {
		t.Fatalf("recovered indexing rows = (%d, %v)", recovered, err)
	}
	got, err = repository.GetByIDAndUserID(ctx, interrupted.ID, userA)
	if err != nil || got.IndexStatus != model.KnowledgeStatusPending {
		t.Fatalf("recovered document = (%+v, %v)", got, err)
	}

	updated, err := repository.StageUpdate(ctx, first.ID, userA, 1, DocumentUpdate{
		OriginalName: "gin.md", StoragePath: fmt.Sprintf("%d/%s/source-v2.md", userA, first.ID),
		FileType: "md", ContentHash: fmt.Sprintf("%064s", "updated"), FileSize: 2,
	}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Version != 2 || updated.ActiveVersion != 1 || updated.IndexStatus != model.KnowledgeStatusPending || updated.CleanupVersion != 1 {
		t.Fatalf("staged update = %+v", updated)
	}
	claimed, err = repository.ClaimForIndexing(ctx, first.ID, userA, 2)
	if err != nil || !claimed {
		t.Fatalf("claim update = (%v, %v)", claimed, err)
	}
	if err := repository.CompleteIndexing(ctx, first.ID, userA, 2, model.KnowledgeStatusReady, 5, ""); err != nil {
		t.Fatal(err)
	}
	got, err = repository.GetByIDAndUserID(ctx, first.ID, userA)
	if err != nil || got.IndexStatus != model.KnowledgeStatusCleanupPending || got.ActiveVersion != 2 || got.CleanupVersion != 1 {
		t.Fatalf("switched active version = (%+v, %v)", got, err)
	}
	due, err := repository.ListCleanupDue(ctx, time.Now().Add(time.Second), 10)
	if err != nil {
		t.Fatal(err)
	}
	var cleanup model.KnowledgeDocument
	for _, candidate := range due {
		if candidate.ID == first.ID {
			cleanup = candidate
		}
	}
	if cleanup.ID == "" {
		t.Fatal("updated document was not listed for cleanup")
	}
	now := time.Now().Add(time.Second)
	cleanupClaimed, err := repository.ClaimCleanup(ctx, cleanup.ID, cleanup.UserID, cleanup.Version, now, now.Add(time.Minute))
	if err != nil || !cleanupClaimed {
		t.Fatalf("claim cleanup = (%v, %v)", cleanupClaimed, err)
	}
	if err := repository.CompleteCleanup(ctx, cleanup); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reindex(ctx, first.ID, userA); err != nil {
		t.Fatal(err)
	}
	claimed, err = repository.ClaimForIndexing(ctx, first.ID, userA, 2)
	if err != nil || !claimed {
		t.Fatalf("claim reindex = (%v, %v)", claimed, err)
	}
	if err := repository.CompleteIndexing(ctx, first.ID, userA, 2, model.KnowledgeStatusReady, 5, ""); err != nil {
		t.Fatal(err)
	}
	failedUpdate, err := repository.StageUpdate(ctx, first.ID, userA, 2, DocumentUpdate{
		OriginalName: "gin.md", StoragePath: fmt.Sprintf("%d/%s/source-v3.md", userA, first.ID),
		FileType: "md", ContentHash: fmt.Sprintf("%064s", "failed-v3"), FileSize: 3,
	}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if failedUpdate.Version != 3 || failedUpdate.ActiveVersion != 2 {
		t.Fatalf("failed-update staging = %+v", failedUpdate)
	}
	claimed, err = repository.ClaimForIndexing(ctx, first.ID, userA, 3)
	if err != nil || !claimed {
		t.Fatalf("claim failed update = (%v, %v)", claimed, err)
	}
	if err := repository.CompleteIndexing(ctx, first.ID, userA, 3, model.KnowledgeStatusFailed, 0, "embedding: operation failed"); err != nil {
		t.Fatal(err)
	}
	got, err = repository.GetByIDAndUserID(ctx, first.ID, userA)
	if err != nil || got.IndexStatus != model.KnowledgeStatusFailed || got.ActiveVersion != 2 || got.ChunkCount != 5 {
		t.Fatalf("failed update must retain old active version = (%+v, %v)", got, err)
	}
	activeVersions, err := repository.ListActiveVersionsByUserID(ctx, userA)
	if err != nil {
		t.Fatal(err)
	}
	foundOldActive := false
	for _, active := range activeVersions {
		if active.ID == first.ID && active.ActiveVersion == 2 {
			foundOldActive = true
		}
	}
	if !foundOldActive {
		t.Fatalf("failed update lost retrieval allow-list entry: %+v", activeVersions)
	}
	if err := repository.RetryFailed(ctx, first.ID, userA); err != nil {
		t.Fatal(err)
	}
	claimed, err = repository.ClaimForIndexing(ctx, first.ID, userA, 3)
	if err != nil || !claimed {
		t.Fatalf("claim retried update = (%v, %v)", claimed, err)
	}
	if err := repository.CompleteIndexing(ctx, first.ID, userA, 3, model.KnowledgeStatusReady, 6, ""); err != nil {
		t.Fatal(err)
	}
	got, err = repository.GetByIDAndUserID(ctx, first.ID, userA)
	if err != nil || got.IndexStatus != model.KnowledgeStatusCleanupPending || got.ActiveVersion != 3 || got.CleanupVersion != 2 {
		t.Fatalf("retried update switch = (%+v, %v)", got, err)
	}
	if err := repository.CompleteCleanup(ctx, *got); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.MarkDeleting(ctx, first.ID, userB); !errors.Is(err, ErrDocumentNotFound) {
		t.Fatalf("cross-user delete error = %v", err)
	}
	deleting, err := repository.MarkDeleting(ctx, first.ID, userA)
	if err != nil || deleting.ID != first.ID {
		t.Fatalf("mark deleting = (%+v, %v)", deleting, err)
	}
	if _, err := repository.GetByIDAndUserID(ctx, first.ID, userA); !errors.Is(err, ErrDocumentNotFound) {
		t.Fatalf("deleted document must immediately disappear, error = %v", err)
	}
	activeVersions, err = repository.ListActiveVersionsByUserID(ctx, userA)
	if err != nil {
		t.Fatal(err)
	}
	for _, active := range activeVersions {
		if active.ID == first.ID {
			t.Fatalf("logical deletion remained retrievable: %+v", activeVersions)
		}
	}
	var deletingRow model.KnowledgeDocument
	if err := db.Where("id = ?", first.ID).First(&deletingRow).Error; err != nil {
		t.Fatal(err)
	}
	if err := repository.CompleteCleanup(ctx, deletingRow); err != nil {
		t.Fatal(err)
	}
	if err := db.Where("id = ?", first.ID).First(&deletingRow).Error; err != nil || deletingRow.IndexStatus != model.KnowledgeStatusDeleted {
		t.Fatalf("deleted row = (%+v, %v)", deletingRow, err)
	}
}

func testDocument(id string, userID int64, name, hashSeed string) *model.KnowledgeDocument {
	return &model.KnowledgeDocument{
		ID: id, UserID: userID, OriginalName: name,
		StoragePath: fmt.Sprintf("%d/%s/source-v1.md", userID, id),
		FileType:    "md", ContentHash: fmt.Sprintf("%064s", hashSeed), FileSize: 1,
	}
}
