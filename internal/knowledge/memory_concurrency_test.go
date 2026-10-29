package knowledge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"sync"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// TestConcurrentUploadsCannotOverAdmit drives the store from eight goroutines and
// requires the invariant the ceilings exist for: never admit more than the limit,
// and never leave a row behind a refusal. The measurement and the write have to be
// one decision - a pre-check sitting in the caller would let two simultaneous
// uploads each read "under the limit" from a snapshot the other had not written
// into yet.
//
// This is deliberately not part of knowledgetest.Suite. The GORM store is tested
// on SQLite, whose single-writer locking answers a concurrent transaction with
// SQLITE_BUSY rather than queueing it, so the same body there measures the test
// engine instead of the rule. What the GORM store does hold is covered there by
// refusing, rolling back and freeing a slot - and the row lock that decides this
// on MySQL (lockUserRow) still needs a gated run before it can be called verified.
func TestConcurrentUploadsCannotOverAdmit(t *testing.T) {
	ctx := context.Background()
	const ceiling = 3
	store := NewMemoryStore().WithLimits(Limits{MaxDocuments: ceiling, MaxTotalBytes: MaxTotalBytesPerUser})
	collection, err := store.CreateCollection(ctx, &domain.Collection{UserID: "user-1", Title: "手册"})
	if err != nil {
		t.Fatal(err)
	}

	var group sync.WaitGroup
	results := make([]error, 8)
	for index := range results {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			content := "并发上传的内容 " + strconv.Itoa(index)
			document := &domain.Document{
				CollectionID: collection.ID,
				Title:        "并发-" + strconv.Itoa(index) + ".md",
				Format:       domain.DocumentFormatText,
				ContentHash:  hash(content),
				FileSize:     int64(len(content)),
			}
			_, err := store.CreateDocument(ctx, "user-1", document, []domain.DocumentChunk{{Text: "内容"}})
			results[index] = err
		}(index)
	}
	group.Wait()

	admitted, refused := 0, 0
	for index, err := range results {
		switch {
		case err == nil:
			admitted++
		case errors.Is(err, ErrDocumentCountLimit):
			refused++
		default:
			t.Errorf("concurrent upload %d = %v, want either admission or the count ceiling", index, err)
		}
	}
	if admitted != ceiling || refused != len(results)-ceiling {
		t.Fatalf("admitted %d and refused %d, want %d and %d", admitted, refused, ceiling, len(results)-ceiling)
	}
	usage, err := store.UsageByUser(ctx, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if usage.DocumentCount != int64(ceiling) || usage.Unmeasured != 0 || usage.TotalBytes <= 0 {
		t.Fatalf("usage = %#v, want exactly %d live rows, all of them measured", usage, ceiling)
	}
	listed, err := store.ListDocuments(ctx, "user-1", collection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != ceiling {
		t.Fatalf("listing = %d documents, want the ceiling with no orphan rows behind the refusals", len(listed))
	}
}

func hash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}
