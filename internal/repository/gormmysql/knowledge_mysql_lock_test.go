package gormmysql

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/knowledge"
)

// Timeouts for this file. Every database call, every lock observation and every
// goroutine reclaim is bounded: a gated test that hangs costs the next person who
// runs it more than a gated test that fails does.
const (
	// mysqlOpTimeout bounds an ordinary store call or observation query.
	mysqlOpTimeout = 30 * time.Second
	// mysqlWaitingOpTimeout bounds a store call that is expected to block on the
	// held user lock. It stays under InnoDB's 50s lock_wait_timeout so a real wait
	// reports the wait, not a driver-side expiry.
	mysqlWaitingOpTimeout = 45 * time.Second
	// mysqlObserveTimeout bounds how long a case will wait for the writers to become
	// visibly blocked. Running out is a failure, never a skip.
	mysqlObserveTimeout = 15 * time.Second
	// mysqlReclaimTimeout bounds how long a failed case waits for its goroutines to
	// return after cancelling them and releasing the holder.
	mysqlReclaimTimeout = 20 * time.Second
)

// TestRealMySQLKnowledgeWriteLocking is the half of the per-user ceilings that no
// offline engine can answer. SQLite ignores FOR UPDATE and serialises writers for its
// own reasons, so passing there proves statements and transactions but says nothing
// about the mechanism the deployment depends on: on real MySQL the user row lock is
// what makes "measure, then write" one decision, and the default REPEATABLE READ
// establishes a transaction's read snapshot at its first non-locking read - which is
// why the lock has to come before the document is read.
//
// Contention here is observed, not assumed. Each case parks the writers behind a
// held row lock and waits until MySQL itself reports the expected number of
// transactions in LOCK WAIT for a locking read of the users row, scoped to this run's
// throwaway schema. That single observation carries three claims at once, and no
// goroutine barrier is offered as evidence for any of them: the writers really are
// blocked on that lock, they are blocked on separate connections (the ids are
// distinct), and they only proceed when the holder releases it.
//
// None of it has been executed. It needs GOPHERLEARN_MYSQL_INTEGRATION_DSN plus the
// PROCESS privilege to read information_schema and performance_schema, and this round
// had no authorisation to start an external service, so the whole file skips.
func TestRealMySQLKnowledgeWriteLocking(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv(EnvRealMySQLDSN))
	if dsn == "" {
		t.Skipf("%s is unset; these cases need real MySQL, not the embedded engine", EnvRealMySQLDSN)
	}
	db := openIntegrationSchema(t, dsn)
	schema := integrationSchemaName(t, db)

	t.Run("a principal with no users row is refused rather than left unlocked", func(t *testing.T) {
		truncateAll(t, db)
		ctx := bounded(t, mysqlOpTimeout)
		store := NewKnowledgeRepository(db)
		seedUserRow(t, db, "user-present")
		collection, err := store.CreateCollection(ctx, &domain.Collection{UserID: "user-present", Title: "手册"})
		if err != nil {
			t.Fatal(err)
		}
		// A request whose principal has no users row cannot be serialised against
		// anything. Refusing is the honest answer; continuing would report a ceiling
		// that nobody held.
		if _, err := store.CreateDocument(ctx, "user-missing", &domain.Document{
			CollectionID: collection.ID, Title: "无主.md", Format: domain.DocumentFormatText,
			ContentHash: probeDigest("无主"), FileSize: 8,
		}, []domain.DocumentChunk{{Text: "内容"}}); !errors.Is(err, knowledge.ErrOwnerUnresolved) {
			t.Errorf("create for a user with no row = %v, want %v", err, knowledge.ErrOwnerUnresolved)
		}
		known, err := store.CreateDocument(ctx, "user-present", &domain.Document{
			CollectionID: collection.ID, Title: "有主.md", Format: domain.DocumentFormatText,
			ContentHash: probeDigest("有主"), FileSize: 8,
		}, []domain.DocumentChunk{{Text: "内容"}})
		if err != nil {
			t.Fatal(err)
		}
		// Refused before the document is read, so it is not reported as somebody
		// else's missing document.
		if _, err := store.ReplaceDocumentContent(ctx, "user-missing", known.ID, knowledge.DocumentReplacement{
			Format: domain.DocumentFormatText, ContentHash: probeDigest("改写"), FileSize: 9,
			Chunks: []domain.DocumentChunk{{Text: "改写内容"}},
		}); !errors.Is(err, knowledge.ErrOwnerUnresolved) {
			t.Errorf("rewrite for a user with no row = %v, want %v", err, knowledge.ErrOwnerUnresolved)
		}
	})

	t.Run("the count ceiling refuses on the count, with bytes out of the way", func(t *testing.T) {
		truncateAll(t, db)
		const userID, writers = "user-count", 6
		seedUserRow(t, db, userID)
		// Byte ceiling at the production number so it cannot bind first: three
		// one-line documents are nowhere near 50 MiB. If this case refused with
		// ErrStorageLimit the two ceilings would be masking each other, so the
		// refusal reason is asserted, not just the count.
		store := NewKnowledgeRepository(db).WithLimits(knowledge.Limits{
			MaxDocuments: 3, MaxTotalBytes: knowledge.MaxTotalBytesPerUser,
		})
		collection := mustCollectionID(t, store, userID)

		blocked := holdUserRow(t, db, userID)
		defer blocked.release()
		burst := launch(t, writers, func(ctx context.Context, index int) error {
			_, err := store.CreateDocument(ctx, userID, &domain.Document{
				CollectionID: collection, Title: "计数-" + strconv.Itoa(index) + ".md",
				Format: domain.DocumentFormatText, ContentHash: probeDigest("计数" + strconv.Itoa(index)), FileSize: 12,
			}, []domain.DocumentChunk{{Text: "内容"}})
			return err
		})
		defer burst.abandon()

		blockCount := blocked.awaitBlocked(t, db, schema, writers)
		blocked.commit(t)
		results := burst.awaitAll(t)

		if admitted, refused := tallyAdmitted(results); admitted != 3 || refused != writers-3 {
			t.Errorf("admitted %d and refused %d, want 3 through and %d refused", admitted, refused, writers-3)
		}
		for index, err := range results {
			switch {
			case err == nil:
			case errors.Is(err, knowledge.ErrDocumentCountLimit):
			default:
				t.Errorf("writer %d refused with %v, want ErrDocumentCountLimit specifically", index, err)
			}
		}
		usage := mustUsage(t, store, userID)
		if usage.DocumentCount != 3 || usage.Unmeasured != 0 {
			t.Errorf("usage = %#v, want three live rows after %d blocked writers (%v waited)", usage, writers, blockCount)
		}
	})

	t.Run("the byte ceiling refuses on bytes, with the count out of the way", func(t *testing.T) {
		truncateAll(t, db)
		const userID, writers, each = "user-bytes", 3, 30
		seedUserRow(t, db, userID)
		// Count ceiling at the production number so three documents can never trip it.
		store := NewKnowledgeRepository(db).WithLimits(knowledge.Limits{
			MaxDocuments: knowledge.MaxDocumentsPerUser, MaxTotalBytes: 50,
		})
		collection := mustCollectionID(t, store, userID)

		blocked := holdUserRow(t, db, userID)
		defer blocked.release()
		burst := launch(t, writers, func(ctx context.Context, index int) error {
			content := strings.Repeat("x", each) + strconv.Itoa(index)
			_, err := store.CreateDocument(ctx, userID, &domain.Document{
				CollectionID: collection, Title: "字节-" + strconv.Itoa(index) + ".md",
				Format: domain.DocumentFormatText, ContentHash: probeDigest(content), FileSize: int64(len(content)),
			}, []domain.DocumentChunk{{Text: "内容"}})
			return err
		})
		defer burst.abandon()

		blockCount := blocked.awaitBlocked(t, db, schema, writers)
		blocked.commit(t)
		results := burst.awaitAll(t)

		// Serialised against a 50 byte ceiling, exactly one 30 byte document fits:
		// 0+30 is inside it, 30+30 is not.
		if admitted, refused := tallyAdmitted(results); admitted != 1 || refused != writers-1 {
			t.Errorf("admitted %d and refused %d, want one through and two refused (%v writers waited)", admitted, refused, blockCount)
		}
		for index, err := range results {
			switch {
			case err == nil:
			case errors.Is(err, knowledge.ErrStorageLimit):
			default:
				t.Errorf("writer %d refused with %v, want ErrStorageLimit specifically - the count ceiling must not be answering here", index, err)
			}
		}
		usage := mustUsage(t, store, userID)
		if usage.TotalBytes > 50 || usage.DocumentCount > 1 {
			t.Errorf("usage = %#v, want the byte ceiling respected", usage)
		}
	})

	t.Run("concurrent rewrites lose no version", func(t *testing.T) {
		truncateAll(t, db)
		const userID, writers = "user-rewrite", 6
		seedUserRow(t, db, userID)
		store := NewKnowledgeRepository(db)
		collection := mustCollectionID(t, store, userID)
		created, err := store.CreateDocument(bounded(t, mysqlOpTimeout), userID, &domain.Document{
			CollectionID: collection, Title: "a.md", Format: domain.DocumentFormatText,
			ContentHash: probeDigest("第一版"), FileSize: 9,
		}, []domain.DocumentChunk{{Text: "第一版"}})
		if err != nil {
			t.Fatal(err)
		}

		blocked := holdUserRow(t, db, userID)
		defer blocked.release()
		burst := launch(t, writers, func(ctx context.Context, index int) error {
			content := "改写" + strconv.Itoa(index)
			_, err := store.ReplaceDocumentContent(ctx, userID, created.ID, knowledge.DocumentReplacement{
				Format: domain.DocumentFormatText, ContentHash: probeDigest(content), FileSize: int64(len(content)),
				Chunks: []domain.DocumentChunk{{Text: content}},
			})
			return err
		})
		defer burst.abandon()

		waited := blocked.awaitBlocked(t, db, schema, writers)
		blocked.commit(t)
		for index, err := range burst.awaitAll(t) {
			if err != nil {
				t.Errorf("rewrite %d = %v, want every distinct rewrite to land after %v writers queued on the lock", index, err, waited)
			}
		}
		// Each writer read the version only once it held the lock, so no two could
		// write the same successor. A read taken before the lock advances the version
		// fewer times than there were writers - the lost update.
		final, err := store.GetDocument(bounded(t, mysqlOpTimeout), userID, created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if final.Version != created.Version+writers {
			t.Fatalf("version = %d, want %d: %d writers each advancing one step", final.Version, created.Version+writers, writers)
		}
		chunks, err := store.ChunksFor(bounded(t, mysqlOpTimeout), userID, []string{collection})
		if err != nil {
			t.Fatal(err)
		}
		if len(chunks) != 1 || chunks[0].Version != final.Version {
			t.Errorf("live chunks = %#v, want only the final version's", chunks)
		}
	})

	t.Run("a create and a rewrite of one content cannot both be admitted", func(t *testing.T) {
		truncateAll(t, db)
		const userID = "user-dedup"
		seedUserRow(t, db, userID)
		store := NewKnowledgeRepository(db)
		collection := mustCollectionID(t, store, userID)
		const shared = "同内容竞争"
		hash := probeDigest(shared)
		existing, err := store.CreateDocument(bounded(t, mysqlOpTimeout), userID, &domain.Document{
			CollectionID: collection, Title: "已有.md", Format: domain.DocumentFormatText,
			ContentHash: probeDigest("旧内容"), FileSize: 9,
		}, []domain.DocumentChunk{{Text: "旧内容"}})
		if err != nil {
			t.Fatal(err)
		}

		blocked := holdUserRow(t, db, userID)
		defer blocked.release()
		burst := launch(t, 2, func(ctx context.Context, index int) error {
			if index == 0 {
				_, err := store.CreateDocument(ctx, userID, &domain.Document{
					CollectionID: collection, Title: "新建.md", Format: domain.DocumentFormatText,
					ContentHash: hash, FileSize: int64(len(shared)),
				}, []domain.DocumentChunk{{Text: shared}})
				return err
			}
			_, err := store.ReplaceDocumentContent(ctx, userID, existing.ID, knowledge.DocumentReplacement{
				Format: domain.DocumentFormatText, ContentHash: hash, FileSize: int64(len(shared)),
				Chunks: []domain.DocumentChunk{{Text: shared}},
			})
			return err
		})
		defer burst.abandon()

		waited := blocked.awaitBlocked(t, db, schema, 2)
		blocked.commit(t)
		results := burst.awaitAll(t)

		// One success and one duplicate refusal, whichever order they serialised in:
		// the invariant is that a single content has exactly one live owner.
		admitted, duplicates := 0, 0
		for index, err := range results {
			switch {
			case err == nil:
				admitted++
			case errors.Is(err, knowledge.ErrDocumentContentDuplicate):
				duplicates++
			default:
				t.Errorf("writer %d = %v, want success or the content already stored", index, err)
			}
		}
		if admitted != 1 || duplicates != 1 {
			t.Errorf("admitted %d and refused %d duplicates, want 1 and 1 (%v writers waited)", admitted, duplicates, waited)
		}
		var rows int64
		if err := db.WithContext(bounded(t, mysqlOpTimeout)).
			Raw("SELECT COUNT(*) FROM knowledge_documents WHERE content_hash = ?", hash).Scan(&rows).Error; err != nil {
			t.Fatal(err)
		}
		if rows != 1 {
			t.Errorf("%d live rows carry the shared hash, want exactly one", rows)
		}
	})

	// This is the case that fails when the ordering reverts. With the old
	// read-then-lock code the rewrite's first non-locking read established its
	// REPEATABLE READ snapshot before it waited, so it judged itself against the
	// state the holder was about to replace: it saw the pre-holder hash, concluded
	// "changed", and advanced the version - re-embedding text that had not changed
	// and, worse, reporting a rewrite as a new version.
	t.Run("a rewrite judges the state the holder committed, not its own snapshot", func(t *testing.T) {
		truncateAll(t, db)
		const userID = "user-snapshot"
		seedUserRow(t, db, userID)
		store := NewKnowledgeRepository(db)
		collection := mustCollectionID(t, store, userID)
		const incoming = "改写到达的内容"
		hash := probeDigest(incoming)
		target, err := store.CreateDocument(bounded(t, mysqlOpTimeout), userID, &domain.Document{
			CollectionID: collection, Title: "目标.md", Format: domain.DocumentFormatText,
			ContentHash: probeDigest("原始内容"), FileSize: 12,
		}, []domain.DocumentChunk{{Text: "原始内容"}})
		if err != nil {
			t.Fatal(err)
		}

		blocked := holdUserRow(t, db, userID)
		defer blocked.release()
		// While the user row is held, the holder rewrites the very document the
		// queued writer is about to rewrite, to exactly the content that writer will
		// send. Nothing else about the queued request changes: only the truth it must
		// read after waiting.
		if err := blocked.tx.WithContext(bounded(t, mysqlOpTimeout)).Exec(
			"UPDATE knowledge_documents SET content_hash = ?, file_size = ? WHERE id = ?",
			hash, int64(len(incoming)), target.ID).Error; err != nil {
			blocked.release()
			t.Fatal(err)
		}

		done := make(chan error, 1)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), mysqlWaitingOpTimeout)
			defer cancel()
			_, err := store.ReplaceDocumentContent(ctx, userID, target.ID, knowledge.DocumentReplacement{
				Format: domain.DocumentFormatText, ContentHash: hash, FileSize: int64(len(incoming)),
				Chunks: []domain.DocumentChunk{{Text: incoming}},
			})
			done <- err
		}()

		waited := blocked.awaitBlocked(t, db, schema, 1)
		blocked.commit(t)
		err = awaitOne(t, done)
		if !errors.Is(err, knowledge.ErrDocumentUnchanged) {
			t.Fatalf("rewrite after the wait = %v, want the committed hash to be seen and reported unchanged (%v writer(s) observed waiting)", err, waited)
		}
		after, err := store.GetDocument(bounded(t, mysqlOpTimeout), userID, target.ID)
		if err != nil {
			t.Fatal(err)
		}
		if after.Version != target.Version+1 {
			t.Errorf("version = %d, want exactly the holder's one advance", after.Version)
		}
	})
}

// holderLock is a transaction that owns one user's row lock, plus the bounded tools
// the case needs around it. Releasing is idempotent because every case defers it and
// the failure paths release early on purpose.
type holderLock struct {
	tx      *gorm.DB
	release func()
}

// holdUserRow takes the same lock the store takes, so the case and the code under
// test are asking the engine for the identical thing.
func holdUserRow(t *testing.T, db *gorm.DB, userID string) *holderLock {
	t.Helper()
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	var once sync.Once
	release := func() { once.Do(func() { _ = tx.Rollback() }) }
	t.Cleanup(release)

	var locked []string
	ctx, cancel := context.WithTimeout(context.Background(), mysqlOpTimeout)
	defer cancel()
	if err := tx.WithContext(ctx).Table("users").Select("id").
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", userID).Limit(1).Scan(&locked).Error; err != nil {
		release()
		t.Fatalf("locking the users row: %v", err)
	}
	if len(locked) != 1 || locked[0] != userID {
		release()
		t.Fatalf("the holder locked %v, want exactly %q", locked, userID)
	}
	return &holderLock{tx: tx, release: release}
}

// commit ends the hold, publishing whatever the holder changed.
func (h *holderLock) commit(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), mysqlOpTimeout)
	defer cancel()
	if err := h.tx.WithContext(ctx).Commit().Error; err != nil {
		h.release()
		t.Fatalf("committing the holder: %v", err)
	}
	h.release()
}

// awaitBlocked waits until MySQL itself reports `want` transactions in LOCK WAIT on a
// locking read of the users row inside this run's schema, and returns the distinct
// connection ids it saw. Reaching the count is what makes the claims about separate
// connections and real contention observations rather than inferences; running out of
// time cancels the writers, releases the holder and fails with what it saw.
func (h *holderLock) awaitBlocked(t *testing.T, db *gorm.DB, schema string, want int) []int64 {
	t.Helper()
	deadline := time.Now().Add(mysqlObserveTimeout)
	var last []int64
	for time.Now().Before(deadline) {
		observed, err := lockWaiters(bounded(t, mysqlOpTimeout), db, schema)
		if err != nil {
			h.release()
			t.Fatalf("reading the lock-wait state needs the PROCESS privilege: %v", err)
		}
		last = observed
		if len(observed) >= want {
			return observed
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.release()
	t.Fatalf("only %v of %d writers were seen waiting on the users row lock within %s (%v)", len(last), want, mysqlObserveTimeout, last)
	return nil
}

// lockWaiters asks InnoDB which transactions are blocked, restricted to this run's
// throwaway schema so a query from another project on the same server cannot be
// mistaken for ours. A plain read would also match the holder and the probe; the
// LOCK WAIT state only ever belongs to a transaction waiting for someone else.
func lockWaiters(ctx context.Context, db *gorm.DB, schema string) ([]int64, error) {
	var ids []int64
	err := db.WithContext(ctx).Raw(
		"SELECT DISTINCT t.trx_mysql_thread_id FROM information_schema.innodb_trx t "+
			"JOIN performance_schema.threads th ON th.PROCESSLIST_ID = t.trx_mysql_thread_id "+
			"WHERE t.trx_state = 'LOCK WAIT' AND t.trx_query LIKE '%FOR UPDATE%' "+
			"AND t.trx_query LIKE '%users%' AND th.PROCESSLIST_SCHEMA = ?", schema).
		Scan(&ids).Error
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// integrationSchemaName resolves the schema this handle is bound to, so the
// observation cannot match work belonging to another project on the same server.
func integrationSchemaName(t *testing.T, db *gorm.DB) string {
	t.Helper()
	var schema string
	if err := db.WithContext(bounded(t, mysqlOpTimeout)).Raw("SELECT DATABASE()").Scan(&schema).Error; err != nil {
		t.Fatalf("reading the current schema: %v", err)
	}
	if !strings.HasPrefix(schema, "gopherlearn_it_") {
		t.Fatalf("the handle is bound to %q, want this suite's throwaway schema", schema)
	}
	return schema
}

// burst is a set of bounded writers that can always be reclaimed. Nothing here is
// offered as proof that the writers share nothing: connection independence and real
// lock contention are only claimed where lockWaiters observed them.
type burst struct {
	results chan indexedError
	cancel  context.CancelFunc
	want    int

	mu      sync.Mutex
	pending int
}

type indexedError struct {
	index int
	err   error
}

func launch(t *testing.T, n int, op func(ctx context.Context, index int) error) *burst {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), mysqlWaitingOpTimeout)
	b := &burst{results: make(chan indexedError, n), cancel: cancel, want: n, pending: n}
	var running sync.WaitGroup
	running.Add(n)
	for index := 0; index < n; index++ {
		go func(index int) {
			defer running.Done()
			// Per-writer context so one writer's deadline cannot cancel its siblings.
			writerCtx, writerCancel := context.WithTimeout(ctx, mysqlWaitingOpTimeout)
			defer writerCancel()
			b.results <- indexedError{index: index, err: op(writerCtx, index)}
		}(index)
	}
	// Reclaim even when the case fails before awaitAll is reached.
	t.Cleanup(b.abandon)
	return b
}

// awaitAll collects every result, giving up after a bounded wait. On timeout it
// cancels the writers first, waits again for them to land, and fails - a gated case
// must never leave goroutines attached to a finished test.
func (b *burst) awaitAll(t *testing.T) []error {
	t.Helper()
	results := make([]error, b.want)
	deadline := time.NewTimer(mysqlWaitingOpTimeout + mysqlReclaimTimeout)
	defer deadline.Stop()
	for collected := 0; collected < b.want; collected++ {
		select {
		case item := <-b.results:
			results[item.index] = item.err
			b.finish(1)
		case <-deadline.C:
			b.abandon()
			t.Fatalf("only %d of %d writers reported within %s", collected, b.want, mysqlWaitingOpTimeout+mysqlReclaimTimeout)
		}
	}
	return results
}

// abandon cancels the writers and reclaims whatever is still outstanding, within a
// deadline. Idempotent and safe from t.Cleanup: a case that already collected every
// result returns here without waiting, so failures do not pay the reclaim twice.
func (b *burst) abandon() {
	b.cancel()
	outstanding := b.outstanding()
	if outstanding == 0 {
		return
	}
	deadline := time.NewTimer(mysqlReclaimTimeout)
	defer deadline.Stop()
	for drained := 0; drained < outstanding; drained++ {
		select {
		case <-b.results:
			b.finish(1)
		case <-deadline.C:
			return
		}
	}
}

func (b *burst) outstanding() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pending
}

func (b *burst) finish(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending -= n
	if b.pending < 0 {
		b.pending = 0
	}
}

// awaitOne waits for the single result of a hand-written goroutine, releasing nothing
// on its own but never blocking forever.
func awaitOne(t *testing.T, done chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(mysqlWaitingOpTimeout + mysqlReclaimTimeout):
		t.Fatal("the queued rewrite never reported")
		return nil
	}
}

func tallyAdmitted(results []error) (admitted, refused int) {
	for _, err := range results {
		if err == nil {
			admitted++
			continue
		}
		refused++
	}
	return admitted, refused
}

// bounded hands out a child of the test context with its own deadline, so no gated
// operation can outlive the case that started it.
func bounded(t *testing.T, limit time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), limit)
	t.Cleanup(cancel)
	return ctx
}

func mustCollectionID(t *testing.T, store *KnowledgeRepository, userID string) string {
	t.Helper()
	collection, err := store.CreateCollection(bounded(t, mysqlOpTimeout), &domain.Collection{UserID: userID, Title: "手册"})
	if err != nil {
		t.Fatal(err)
	}
	return collection.ID
}

func mustUsage(t *testing.T, store *KnowledgeRepository, userID string) knowledge.Usage {
	t.Helper()
	usage, err := store.UsageByUser(bounded(t, mysqlOpTimeout), userID)
	if err != nil {
		t.Fatal(err)
	}
	return usage
}

// seedUserRow creates a real users row, the way the deployment's authentication
// would. Email stays NULL: the column is nullable-unique precisely so seeded
// principals cannot collide with each other.
func seedUserRow(t *testing.T, db *gorm.DB, id string) {
	t.Helper()
	if err := db.WithContext(bounded(t, mysqlOpTimeout)).
		Create(&UserRecord{ID: id, DisplayName: id, CreatedAt: time.Now().UTC()}).Error; err != nil {
		t.Fatalf("seed user %s: %v", id, err)
	}
}

// probeDigest is a content hash the seeded rows can be compared against. It is a
// plain SHA-256 hex, not the pipeline's document.ContentHash: these cases exercise
// uniqueness and locking, and normalisation has its own tests in internal/document.
func probeDigest(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}
