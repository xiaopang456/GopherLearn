package gormmysql

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/knowledge"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

// CollectionRecord is the knowledge_collections table.
type CollectionRecord struct {
	ID        string    `gorm:"column:id;type:varchar(96);primaryKey"`
	UserID    string    `gorm:"column:user_id;type:varchar(96);not null;index:idx_collections_user"`
	Title     string    `gorm:"column:title;type:varchar(200);not null"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

func (CollectionRecord) TableName() string { return "knowledge_collections" }

// DocumentRecord is the knowledge_documents table. Version is the only retrieval
// switch a rewrite has to make: the chunk rows kept for a document are the
// current version's, and the previous version's rows are deleted in the same
// transaction, so a stale chunk cannot come back through a different query.
//
// ContentHash and FileSize are nullable because ingestion only started recording
// them: a row written earlier keeps its NULLs, which the quota reads as
// "unmeasured" rather than as a free zero-byte document.
type DocumentRecord struct {
	ID           string  `gorm:"column:id;type:varchar(96);primaryKey"`
	CollectionID string  `gorm:"column:collection_id;type:varchar(96);not null;index:idx_documents_collection"`
	Title        string  `gorm:"column:title;type:varchar(200);not null"`
	Format       string  `gorm:"column:format;type:varchar(16);not null"`
	Version      int     `gorm:"column:version;not null;default:1"`
	ChunkCount   int     `gorm:"column:chunk_count;not null;default:0"`
	ContentHash  *string `gorm:"column:content_hash;type:char(64);index:idx_documents_content_hash"`
	FileSize     *int64  `gorm:"column:file_size"`
	// ActiveVersion is the version answering queries; Version is the staged one. The
	// pair is what lets a rewrite that never indexed leave the previous text serving.
	ActiveVersion   int        `gorm:"column:active_version;not null;default:0"`
	IndexStatus     string     `gorm:"column:index_status;type:varchar(32)"`
	ErrorMessage    string     `gorm:"column:error_message;type:text"`
	CleanupVersion  int        `gorm:"column:cleanup_version;not null;default:0"`
	CleanupAttempts int        `gorm:"column:cleanup_attempts;not null;default:0"`
	CleanupAfter    *time.Time `gorm:"column:cleanup_after"`
	CreatedAt       time.Time  `gorm:"column:created_at"`
	UpdatedAt       time.Time  `gorm:"column:updated_at"`
}

func (DocumentRecord) TableName() string { return "knowledge_documents" }

// DocumentChunkRecord is the knowledge_chunks table. `seq` rather than `order`
// because ORDER is reserved in MySQL.
type DocumentChunkRecord struct {
	ID           string    `gorm:"column:id;type:varchar(128);primaryKey"`
	DocumentID   string    `gorm:"column:document_id;type:varchar(96);not null;index:idx_chunks_document_seq,priority:1"`
	CollectionID string    `gorm:"column:collection_id;type:varchar(96);not null;index:idx_chunks_collection"`
	Version      int       `gorm:"column:version;not null"`
	Seq          int       `gorm:"column:seq;not null;index:idx_chunks_document_seq,priority:2"`
	Section      string    `gorm:"column:section;type:varchar(500)"`
	Text         string    `gorm:"column:text;type:mediumtext;not null"`
	StartLine    int       `gorm:"column:start_line"`
	EndLine      int       `gorm:"column:end_line"`
	CreatedAt    time.Time `gorm:"column:created_at"`
}

func (DocumentChunkRecord) TableName() string { return "knowledge_chunks" }

// MigrateKnowledge adds the three knowledge tables. It is additive like Migrate
// and never rewrites existing rows.
func MigrateKnowledge(ctx context.Context, db *gorm.DB) error {
	if ctx == nil || db == nil {
		return repository.ErrInvalidArgument
	}
	models := []any{&CollectionRecord{}, &DocumentRecord{}, &DocumentChunkRecord{}}
	if err := db.WithContext(ctx).AutoMigrate(models...); err != nil {
		return fmt.Errorf("repository/gormmysql: migrate knowledge: %w", err)
	}
	return nil
}

// KnowledgeRepository implements knowledge.Store on top of GORM.
type KnowledgeRepository struct {
	db     *gorm.DB
	limits knowledge.Limits
}

func NewKnowledgeRepository(db *gorm.DB) *KnowledgeRepository {
	return &KnowledgeRepository{db: db, limits: knowledge.DefaultLimits()}
}

// WithLimits replaces the per-user ceilings this repository enforces. It exists
// so a test can reach a quota boundary without uploading a hundred documents;
// production runs on knowledge.DefaultLimits().
func (r *KnowledgeRepository) WithLimits(limits knowledge.Limits) *KnowledgeRepository {
	r.limits = limits
	return r
}

var _ knowledge.Store = (*KnowledgeRepository)(nil)

func (r *KnowledgeRepository) CreateCollection(ctx context.Context, collection *domain.Collection) (*domain.Collection, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if r.db == nil || collection == nil {
		return nil, repository.ErrInvalidArgument
	}
	record := CollectionRecord{
		ID:     collection.ID,
		UserID: strings.TrimSpace(collection.UserID),
		Title:  strings.TrimSpace(collection.Title),
	}
	if strings.TrimSpace(record.UserID) == "" || validateTitle(record.Title) != nil {
		return nil, repository.ErrInvalidArgument
	}
	record.CreatedAt, record.UpdatedAt = clockTime(collection.CreatedAt), clockTime(collection.UpdatedAt)
	if record.ID == "" {
		record.ID = newID("collection")
	}
	if err := translate(r.db.WithContext(ctx).Create(&record).Error); err != nil {
		return nil, err
	}
	created := record.toDomain()
	return &created, nil
}

func (r *KnowledgeRepository) ListCollections(ctx context.Context, userID string) ([]domain.Collection, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if r.db == nil || strings.TrimSpace(userID) == "" {
		return nil, repository.ErrInvalidArgument
	}
	var records []CollectionRecord
	err := r.db.WithContext(ctx).
		Where("user_id = ?", userID).
		Order("created_at ASC, id ASC").
		Find(&records).Error
	if err := translate(err); err != nil {
		return nil, err
	}
	collections := make([]domain.Collection, 0, len(records))
	for _, record := range records {
		collections = append(collections, record.toDomain())
	}
	return collections, nil
}

func (r *KnowledgeRepository) GetCollection(ctx context.Context, userID, collectionID string) (*domain.Collection, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if r.db == nil || strings.TrimSpace(userID) == "" || strings.TrimSpace(collectionID) == "" {
		return nil, repository.ErrInvalidArgument
	}
	var record CollectionRecord
	err := r.db.WithContext(ctx).
		Where("id = ? AND user_id = ?", collectionID, userID).
		Take(&record).Error
	if err := translate(err); err != nil {
		return nil, err
	}
	collection := record.toDomain()
	return &collection, nil
}

// CreateDocument is the synchronous composition of the lifecycle: stage the first
// version, then claim and publish it. It is not a second implementation of
// "published" - the staged methods above are the state machine, and this only drives
// it with nothing external to wait for.
func (r *KnowledgeRepository) CreateDocument(ctx context.Context, userID string, document *domain.Document, chunks []domain.DocumentChunk) (*domain.Document, error) {
	staged, err := r.StageDocument(ctx, userID, document, chunks)
	if err != nil {
		return nil, err
	}
	return r.publishStaged(ctx, userID, staged)
}

func (r *KnowledgeRepository) GetDocument(ctx context.Context, userID, documentID string) (*domain.Document, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if r.db == nil || strings.TrimSpace(userID) == "" || strings.TrimSpace(documentID) == "" {
		return nil, repository.ErrInvalidArgument
	}
	var record DocumentRecord
	// The live filter is what makes a deletion-in-progress read as absent, the way
	// v2's null active_name does: a client must not be able to tell a document that
	// is being cleared out from one that was never there.
	err := r.db.WithContext(ctx).Model(&DocumentRecord{}).
		Joins("JOIN knowledge_collections ON knowledge_collections.id = knowledge_documents.collection_id").
		Where("knowledge_documents.id = ? AND knowledge_collections.user_id = ?", documentID, userID).
		Where(liveDocumentScope).
		Take(&record).Error
	if err := translate(err); err != nil {
		return nil, err
	}
	document := record.toDomain()
	return &document, nil
}

func (r *KnowledgeRepository) ListDocuments(ctx context.Context, userID, collectionID string) ([]domain.Document, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if r.db == nil || strings.TrimSpace(userID) == "" || strings.TrimSpace(collectionID) == "" {
		return nil, repository.ErrInvalidArgument
	}
	var records []DocumentRecord
	err := r.transaction(ctx, func(tx *gorm.DB) error {
		if err := requireOwnedCollection(tx, userID, collectionID); err != nil {
			return err
		}
		return tx.Model(&DocumentRecord{}).Where("collection_id = ?", collectionID).
			Where(liveDocumentScope).Order(orderByCreation).Find(&records).Error
	})
	if err := translate(err); err != nil {
		return nil, err
	}
	documents := make([]domain.Document, 0, len(records))
	for _, record := range records {
		documents = append(documents, record.toDomain())
	}
	return documents, nil
}

// DeleteDocument removes a document and its chunks in one transaction. Chunk
// rows of retired versions are removed too, because they are keyed by document
// and would otherwise outlive the only thing that explains them.
// DeleteDocument is the composition of the two lifecycle steps a caller has no reason
// to sequence by hand: make the document invisible and release its uniqueness, then
// carry out the removal that was recorded on the way.
func (r *KnowledgeRepository) DeleteDocument(ctx context.Context, userID, documentID string) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(documentID) == "" {
		return repository.ErrInvalidArgument
	}
	if _, err := r.MarkDeleting(ctx, userID, documentID); err != nil {
		return translate(err)
	}
	return r.CompleteCleanup(ctx, userID, documentID)
}

func (r *KnowledgeRepository) ReplaceDocumentContent(ctx context.Context, userID, documentID string, replacement knowledge.DocumentReplacement) (*domain.Document, error) {
	staged, err := r.stageReplacementTx(ctx, userID, documentID, knowledge.StageOntoCurrentVersion, replacement)
	if err != nil {
		return staged, err
	}
	return r.publishStaged(ctx, userID, staged)
}

func (r *KnowledgeRepository) ChunksFor(ctx context.Context, userID string, collectionIDs []string) ([]domain.DocumentChunk, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if r.db == nil || strings.TrimSpace(userID) == "" {
		return nil, repository.ErrInvalidArgument
	}
	if len(collectionIDs) == 0 {
		return nil, nil
	}
	var records []DocumentChunkRecord
	err := r.transaction(ctx, func(tx *gorm.DB) error {
		if err := requireOwnedCollections(tx, userID, collectionIDs); err != nil {
			return err
		}
		// A chunk is retrievable exactly when it belongs to the version its document
		// is serving. Staged and superseded rows live in this table too, and serving
		// them would let a rewrite that never indexed replace working text. The
		// expression is the SQL form of domain.Document.ServingVersion, so a row
		// written before the lifecycle columns existed keeps answering from its own
		// version instead of vanishing at migration time.
		return tx.Table("knowledge_chunks").
			Joins("JOIN knowledge_documents ON knowledge_documents.id = knowledge_chunks.document_id").
			Where("knowledge_chunks.collection_id IN ?", collectionIDs).
			Where("knowledge_chunks.version = "+servingVersionSQL).
			Where(servingVersionSQL+" > 0").
			Where("knowledge_documents.index_status NOT IN (?, ?) OR knowledge_documents.index_status IS NULL OR knowledge_documents.index_status = ''",
				string(domain.DocumentStatusDeleting), string(domain.DocumentStatusDeleted)).
			Order("knowledge_chunks.collection_id ASC, knowledge_chunks.document_id ASC, knowledge_chunks.seq ASC").
			Find(&records).Error
	})
	if err := translate(err); err != nil {
		return nil, err
	}
	chunks := make([]domain.DocumentChunk, 0, len(records))
	for _, record := range records {
		chunks = append(chunks, record.toDomain())
	}
	return chunks, nil
}

func (r *KnowledgeRepository) DocumentTitles(ctx context.Context, userID string, documentIDs []string) (map[string]string, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	titles := map[string]string{}
	if r.db == nil || strings.TrimSpace(userID) == "" || len(documentIDs) == 0 {
		return titles, nil
	}
	var records []DocumentRecord
	query := r.db.WithContext(ctx).Model(&DocumentRecord{}).
		Joins("JOIN knowledge_collections ON knowledge_collections.id = knowledge_documents.collection_id").
		Where("knowledge_collections.user_id = ?", userID).
		Where(liveDocumentScope)
	err := translate(query.Where("knowledge_documents.id IN ?", documentIDs).Find(&records).Error)
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		titles[record.ID] = record.Title
	}
	return titles, nil
}

func (r *KnowledgeRepository) EnsureReadable(ctx context.Context, userID string, collectionIDs []string) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if r.db == nil || strings.TrimSpace(userID) == "" {
		return repository.ErrInvalidArgument
	}
	if len(collectionIDs) == 0 {
		return nil
	}
	return translate(r.transaction(ctx, func(tx *gorm.DB) error {
		return requireOwnedCollections(tx, userID, collectionIDs)
	}))
}

func (r *KnowledgeRepository) transaction(ctx context.Context, fn func(tx *gorm.DB) error) error {
	return r.db.WithContext(ctx).Transaction(fn)
}

// UsageByUser adds up one user's whole knowledge base across their collections.
// The three numbers answer different questions: DocumentCount is what the count
// ceiling bounds, TotalBytes is what the storage ceiling bounds and only covers
// rows whose size was recorded, and Unmeasured says how many rows are missing
// from that sum. A caller that reads Unmeasured as zero would report a quota
// headroom the database cannot vouch for.
func (r *KnowledgeRepository) UsageByUser(ctx context.Context, userID string) (knowledge.Usage, error) {
	if err := checkContext(ctx); err != nil {
		return knowledge.Usage{}, err
	}
	if r.db == nil || strings.TrimSpace(userID) == "" {
		return knowledge.Usage{}, repository.ErrInvalidArgument
	}
	usage, err := usageForUser(r.db.WithContext(ctx), userID)
	if err != nil {
		return knowledge.Usage{}, translate(err)
	}
	return usage, nil
}

// liveDocumentScope is v2's `active_name IS NOT NULL` as a status test: a document
// being deleted, or already gone, holds no name, no content hash and no quota. The
// empty-status branch is what keeps rows written before the lifecycle columns existed
// counted - they were published by the synchronous path.
// The status values are this package's own constants, so they are spelled out rather
// than bound: the scope is attached with Where(scope) and no arguments.
const liveDocumentScope = "(knowledge_documents.index_status IS NULL OR knowledge_documents.index_status = '' OR " +
	"knowledge_documents.index_status NOT IN ('" + string(domain.DocumentStatusDeleting) +
	"', '" + string(domain.DocumentStatusDeleted) + "'))"

// servingVersionSQL is the column-side twin of domain.Document.ServingVersion: an
// untracked row serves its own version, a tracked one serves ActiveVersion. Keeping
// it in one constant is what stops the retrieval filter and the Go view from
// disagreeing about which rows are live.
const servingVersionSQL = "(CASE WHEN knowledge_documents.index_status IS NULL OR knowledge_documents.index_status = '' " +
	"THEN knowledge_documents.version ELSE knowledge_documents.active_version END)"

func usageForUser(tx *gorm.DB, userID string) (knowledge.Usage, error) {
	var row struct {
		DocumentCount int64
		TotalBytes    int64
		Unmeasured    int64
	}
	err := tx.Table("knowledge_documents").
		Select("COUNT(*) AS document_count, COALESCE(SUM(file_size), 0) AS total_bytes, "+
			"SUM(CASE WHEN file_size IS NULL OR file_size <= 0 THEN 1 ELSE 0 END) AS unmeasured").
		Joins("JOIN knowledge_collections ON knowledge_collections.id = knowledge_documents.collection_id").
		Where("knowledge_collections.user_id = ?", userID).
		Where(liveDocumentScope).
		Scan(&row).Error
	if err != nil {
		return knowledge.Usage{}, err
	}
	return knowledge.Usage{DocumentCount: row.DocumentCount, TotalBytes: row.TotalBytes, Unmeasured: row.Unmeasured}, nil
}

// lockUserRow serialises one user's knowledge writes the way v2's
// CreateWithinLimits does: take a row lock on the owning user, then read, then
// measure, then write, all in the same transaction. The users row is the only single
// point every document of that user shares - the documents themselves are spread over
// collections - so on an engine that can lock a row, no row means no serialisation,
// and continuing would let the store report a per-user ceiling that nothing held. A
// missing row is therefore refused: a request whose principal has no users row comes
// from a deployment whose authentication and storage disagree, not from a user who
// can fix anything.
func lockUserRow(tx *gorm.DB, userID string) error {
	dialect := tx.Dialector.Name()
	if !supportsRowLock(dialect) {
		return nil
	}
	var ids []string
	result := userRowLockStatement(tx, userID).Scan(&ids)
	if result.Error != nil {
		return result.Error
	}
	return ownerLockOutcome(dialect, len(ids) > 0)
}

// userRowLockStatement is the locking read itself, split out so a test can ask for
// the SQL this code path produces instead of keeping a second copy of the clause
// that could drift from it. One named column into a slice: SELECT * would return six
// columns, and scanning that into a single string is not something GORM promises -
// v2 reads "id" for the same reason.
func userRowLockStatement(tx *gorm.DB, userID string) *gorm.DB {
	return tx.Table("users").
		Select("id").
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", userID).
		Limit(1)
}

// ownerLockOutcome is the decision lockUserRow makes, separated from the connection
// so it can be tested where no engine is listening: an engine that honours FOR UPDATE
// must fail when there is no row to lock, and an engine that ignores the clause must
// not report a failure it never observed.
func ownerLockOutcome(dialect string, rowFound bool) error {
	if !supportsRowLock(dialect) {
		return nil
	}
	if !rowFound {
		return knowledge.ErrOwnerUnresolved
	}
	return nil
}

// supportsRowLock names the engines where SELECT ... FOR UPDATE is a real lock.
// SQLite, which the offline suites run on, ignores the clause, so the guard is
// about not emitting SQL the engine would reject rather than about semantics.
func supportsRowLock(dialect string) bool {
	switch dialect {
	case "mysql", "postgres":
		return true
	default:
		return false
	}
}

// requireUniqueDocument refuses a title or a recorded content hash that another
// live document of this user already holds, which is what v2's two unique indexes
// (user_id, active_name) and (user_id, active_hash) enforce. excludeDocumentID
// lets a rewrite be measured against everything except itself.
//
// This is a lookup rather than an index because the target's documents hang off
// collections, so a per-user uniqueness index does not exist yet; see the
// migration note about adding one.
func requireUniqueDocument(tx *gorm.DB, userID string, candidate DocumentRecord, excludeDocumentID string) error {
	scoped := func() *gorm.DB {
		query := tx.Table("knowledge_documents").
			Joins("JOIN knowledge_collections ON knowledge_collections.id = knowledge_documents.collection_id").
			Where("knowledge_collections.user_id = ?", userID).
			Where(liveDocumentScope)
		if excludeDocumentID != "" {
			query = query.Where("knowledge_documents.id <> ?", excludeDocumentID)
		}
		return query
	}
	var sameTitle int64
	if err := scoped().Where("knowledge_documents.title = ?", candidate.Title).Count(&sameTitle).Error; err != nil {
		return err
	}
	if sameTitle > 0 {
		return knowledge.ErrDocumentNameTaken
	}
	if hash := dereferenceString(candidate.ContentHash); hash != "" {
		var sameHash int64
		if err := scoped().Where("knowledge_documents.content_hash = ?", hash).Count(&sameHash).Error; err != nil {
			return err
		}
		if sameHash > 0 {
			return knowledge.ErrDocumentContentDuplicate
		}
	}
	return nil
}

// documentMetadata turns the two values into the columns' nullable form. An
// empty hash or a non-positive size is stored as NULL: "not measured" has to stay
// distinguishable from "measured as nothing", or a legacy row would look like a
// zero-byte document that any number of duplicates could reuse.
func documentMetadata(contentHash string, fileSize int64) (*string, *int64) {
	return optionalString(contentHash), optionalInt64(fileSize)
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func optionalInt64(value int64) *int64 {
	if value <= 0 {
		return nil
	}
	return &value
}

func dereferenceString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func positiveBytes(value *int64) int64 {
	if value == nil || *value <= 0 {
		return 0
	}
	return *value
}

func requireOwnedCollection(tx *gorm.DB, userID, collectionID string) error {
	var count int64
	err := tx.Model(&CollectionRecord{}).
		Where("id = ? AND user_id = ?", collectionID, userID).
		Count(&count).Error
	if err != nil {
		return err
	}
	if count == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func requireOwnedCollections(tx *gorm.DB, userID string, collectionIDs []string) error {
	unique := make([]string, 0, len(collectionIDs))
	seen := make(map[string]bool, len(collectionIDs))
	for _, collectionID := range collectionIDs {
		trimmed := strings.TrimSpace(collectionID)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		unique = append(unique, trimmed)
	}
	if len(unique) == 0 {
		return repository.ErrInvalidArgument
	}
	var count int64
	if err := tx.Model(&CollectionRecord{}).
		Where("id IN ? AND user_id = ?", unique, userID).
		Count(&count).Error; err != nil {
		return err
	}
	// Anything short of a full match is refused without saying which id was
	// missing or foreign, so the endpoint cannot be used to enumerate them.
	if count != int64(len(unique)) {
		return repository.ErrNotFound
	}
	return nil
}

// validateTitle and validateChunkSet keep the GORM store's refusals identical to
// the in-memory store's. Length limits cannot be left to the column types:
// SQLite ignores varchar widths, and MySQL would answer with a driver error that
// maps to a 500 rather than a rejected request.
func validateTitle(title string) error {
	if title == "" || utf8.RuneCountInString(title) > 200 {
		return repository.ErrInvalidArgument
	}
	return nil
}

func validateChunkSet(chunks []domain.DocumentChunk) error {
	if len(chunks) == 0 {
		return repository.ErrInvalidArgument
	}
	for _, chunk := range chunks {
		if strings.TrimSpace(chunk.Text) == "" {
			return repository.ErrInvalidArgument
		}
	}
	return nil
}

func chunkRecordsFor(documentID, collectionID string, version int, chunks []domain.DocumentChunk) []DocumentChunkRecord {
	records := make([]DocumentChunkRecord, 0, len(chunks))
	for index, chunk := range chunks {
		id := strings.TrimSpace(chunk.ID)
		if id == "" {
			id = fmt.Sprintf("%s-v%d-c%d", documentID, version, index+1)
		}
		createdAt := chunk.CreatedAt
		if createdAt.IsZero() {
			createdAt = time.Now().UTC()
		}
		records = append(records, DocumentChunkRecord{
			ID:           id,
			DocumentID:   documentID,
			CollectionID: collectionID,
			Version:      version,
			Seq:          index + 1,
			Section:      chunk.Section,
			Text:         chunk.Text,
			StartLine:    chunk.StartLine,
			EndLine:      chunk.EndLine,
			CreatedAt:    createdAt.UTC(),
		})
	}
	return records
}

func (r CollectionRecord) toDomain() domain.Collection {
	return domain.Collection{ID: r.ID, UserID: r.UserID, Title: r.Title, CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC()}
}

func (r DocumentRecord) toDomain() domain.Document {
	document := domain.Document{
		ID:           r.ID,
		CollectionID: r.CollectionID,
		Title:        r.Title,
		Format:       domain.DocumentFormat(r.Format),
		Version:      r.Version,
		ChunkCount:   r.ChunkCount,
		CreatedAt:    r.CreatedAt.UTC(),
		UpdatedAt:    r.UpdatedAt.UTC(),
	}
	// A NULL column stays "not measured": inventing a hash or a size here would
	// hand the quota free space and let a fabricated digest win a duplicate check.
	if r.ContentHash != nil {
		document.ContentHash = *r.ContentHash
	}
	if r.FileSize != nil {
		document.FileSize = *r.FileSize
	}
	// IndexStatus is carried out exactly as stored, including the empty value that
	// marks a row written before the lifecycle columns existed: domain.Document
	// .ServingVersion is the single place that decides which version such a row
	// serves. Rewriting it here would make the Go view and the SQL filter disagree.
	document.IndexStatus = domain.DocumentIndexStatus(r.IndexStatus)
	document.ActiveVersion = r.ActiveVersion
	document.ErrorMessage = r.ErrorMessage
	document.CleanupVersion, document.CleanupAttempts = r.CleanupVersion, r.CleanupAttempts
	if r.CleanupAfter != nil {
		document.CleanupAfter = *r.CleanupAfter
	}
	return document
}

func (r DocumentChunkRecord) toDomain() domain.DocumentChunk {
	return domain.DocumentChunk{
		ID:           r.ID,
		DocumentID:   r.DocumentID,
		CollectionID: r.CollectionID,
		Version:      r.Version,
		Order:        r.Seq,
		Section:      r.Section,
		Text:         r.Text,
		StartLine:    r.StartLine,
		EndLine:      r.EndLine,
		CreatedAt:    r.CreatedAt.UTC(),
	}
}

func clockTime(value time.Time) time.Time {
	if value.IsZero() {
		return time.Now().UTC()
	}
	return value.UTC()
}
