package conversation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"GopherAI/model"
	"gorm.io/gorm"
)

// SeedMemory is a profile memory the learner confirmed before this attempt.
//
// A P1 scenario measures a conflict between such a memory and what the same
// learner says in this session. The memory has to exist before the
// conversation is prepared, so that the product sees it while the
// conversation is built, and it has to exist again in the database the answer
// is asked in, because that database is a fresh one.
//
// The row is written the way the product writes it: the product's own model,
// the product's own content hash, and the product's own validation. The tags
// are the one exception. The product keeps a fixed vocabulary of tags and
// refuses a row that carries another one, and a profile memory is injected
// whole and is never looked up by a tag, so a tag outside the vocabulary is
// dropped instead of failing the whole attempt. The dropped tags are
// reported, so the row and the scenario never differ silently.
type SeedMemory struct {
	Scope     string   `json:"scope"`
	Subject   string   `json:"subject"`
	Content   string   `json:"content"`
	Tags      []string `json:"tags"`
	Confirmed bool     `json:"confirmed"`
	SavedAt   string   `json:"saved_at"`
}

// SeedMemoryReport says what the seeded memories became.
type SeedMemoryReport struct {
	// Written is how many rows were inserted, AlreadyThere how many were
	// already present. A memory that is already there is not an error: the
	// write is idempotent so that a run that is restarted writes the same
	// row instead of a second one.
	Written      int `json:"written"`
	AlreadyThere int `json:"already_there"`
	// DroppedTags names the tags that the product's vocabulary does not
	// accept and that were therefore not written.
	DroppedTags string `json:"dropped_tags,omitempty"`
	// IDs are the identifiers of the rows that were written.
	IDs []string `json:"ids,omitempty"`
}

// SeedMemories writes the seeded profile memories of one attempt.
//
// The write is idempotent: the identifier of a memory is derived from its
// own content, so writing the same memory twice leaves one row. The memories
// are written with the save date of the scenario and not with the time of
// the run, because that date is what the product shows when it injects the
// memory, and a scenario that says "saved on 14 January" has to be rendered
// that way.
func (s *Sandbox) SeedMemories(ctx context.Context, userID int64, specs []SeedMemory) (SeedMemoryReport, error) {
	report := SeedMemoryReport{}
	if len(specs) == 0 {
		return report, nil
	}
	rows := make([]model.LongTermMemory, 0, len(specs))
	for _, spec := range specs {
		memory, dropped, err := buildSeedMemory(userID, spec)
		if err != nil {
			return report, err
		}
		if dropped != "" {
			if report.DroppedTags != "" {
				report.DroppedTags += "; "
			}
			report.DroppedTags += dropped
		}
		rows = append(rows, memory)
	}
	err := (guardedStore{s}).with(ctx, func(tx *gorm.DB) error {
		return seedMemoriesInTx(tx, userID, rows, &report)
	})
	if err != nil {
		return report, err
	}
	return report, nil
}

// seedMemoriesInTx writes the rows of one attempt.
//
// A row whose identifier is already there is left alone: the identifier is
// derived from the memory itself, so the same memory always has the same
// identifier and a second write cannot produce a second row.
func seedMemoriesInTx(tx *gorm.DB, userID int64, rows []model.LongTermMemory, report *SeedMemoryReport) error {
	for _, row := range rows {
		var existing int64
		if err := tx.Model(&model.LongTermMemory{}).Where("id = ?", row.ID).Count(&existing).Error; err != nil {
			return err
		}
		if existing > 0 {
			report.AlreadyThere++
			continue
		}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		report.Written++
		report.IDs = append(report.IDs, row.ID)
	}
	return nil
}

// ReadSeedMemories reads the memories of one user back.
//
// The write is checked by reading it: a seed that was written but cannot be
// read back is not a seed, and the attempt has to be able to show what is
// actually in the database rather than what it meant to write.
func (s *Sandbox) ReadSeedMemories(ctx context.Context, userID int64) ([]model.LongTermMemory, error) {
	var rows []model.LongTermMemory
	err := (guardedStore{s}).with(ctx, func(tx *gorm.DB) error {
		var inner error
		rows, inner = readSeedMemories(ctx, tx, userID)
		return inner
	})
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// readSeedMemories reads the memories of one user out of one database.
func readSeedMemories(ctx context.Context, db *gorm.DB, userID int64) ([]model.LongTermMemory, error) {
	var rows []model.LongTermMemory
	if err := db.WithContext(ctx).Where("user_id = ?", userID).Order("id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// seedAttemptMemories writes the seeded profile memories of an attempt and
// checks the write by reading it back.
//
// The read is not a formality: a write that cannot be read back is not a
// write, and an attempt that measures a conflict with a profile memory has to
// be able to show that the memory was really there. The rows are returned so
// that the attempt can report them.
func seedAttemptMemories(ctx context.Context, s *Sandbox, userID int64, specs []SeedMemory) (SeedMemoryReport, []model.LongTermMemory, error) {
	report, err := s.SeedMemories(ctx, userID, specs)
	if err != nil {
		return report, nil, err
	}
	rows, err := s.ReadSeedMemories(ctx, userID)
	if err != nil {
		return report, nil, err
	}
	if len(rows) < len(specs) {
		return report, rows, fmt.Errorf("the seeded profile memories could not be read back")
	}
	return report, rows, nil
}

// buildSeedMemory turns one specification into the row the product would
// have written itself.
func buildSeedMemory(userID int64, spec SeedMemory) (model.LongTermMemory, string, error) {
	if userID <= 0 {
		return model.LongTermMemory{}, "", fmt.Errorf("a seeded memory needs an evaluation identity")
	}
	scope := model.MemoryScope(strings.TrimSpace(spec.Scope))
	if scope == "" {
		scope = model.MemoryScopeGlobal
	}
	if scope != model.MemoryScopeGlobal {
		return model.LongTermMemory{}, "", fmt.Errorf("a seeded profile memory must have the global scope")
	}
	subject := strings.TrimSpace(spec.Subject)
	content := strings.TrimSpace(spec.Content)
	if subject == "" || content == "" {
		return model.LongTermMemory{}, "", fmt.Errorf("a seeded memory needs a subject and a content")
	}
	savedAt, err := time.Parse("2006-01-02", strings.TrimSpace(spec.SavedAt))
	if err != nil {
		return model.LongTermMemory{}, "", fmt.Errorf("a seeded memory needs a save date")
	}
	savedAt = savedAt.UTC()
	tags, dropped := seedMemoryTags(subject, spec.Tags)
	memory := model.LongTermMemory{
		ID:             seedMemoryID(userID, scope, subject, content),
		UserID:         userID,
		Scope:          scope,
		Subject:        subject,
		Content:        content,
		ContentHash:    model.MemoryContentHash(content),
		StructuredJSON: "{}",
		Tags:           tags,
		Durability:     model.MemoryDurabilityStable,
		SourceRefs:     model.StringList{"evaluation-seed"},
		Confidence:     1,
		Importance:     5,
		Status:         model.LongTermMemoryActive,
		UserConfirmed:  spec.Confirmed,
		CreatedAt:      savedAt,
		UpdatedAt:      savedAt,
	}
	if err := memory.Validate(); err != nil {
		return model.LongTermMemory{}, "", fmt.Errorf("the seeded memory is not a memory the product accepts: %w", err)
	}
	return memory, dropped, nil
}

// seedMemoryID is the identifier of a seeded memory.
//
// It is derived from the identity of the memory and its own text, so the
// same memory always has the same identifier: the write is idempotent, and a
// run that is restarted writes the same row instead of a second one.
func seedMemoryID(userID int64, scope model.MemoryScope, subject, content string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%s|%s", userID, scope, subject, content)))
	hashed := hex.EncodeToString(sum[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", hashed[0:8], hashed[8:12], hashed[12:16], hashed[16:20], hashed[20:32])
}

// seedMemoryTags keeps the tags the product's vocabulary accepts.
//
// The product's tag vocabulary is a fixed list, and a memory that carries
// another tag is a memory the product itself refuses to write. A profile
// memory is injected whole and is never looked up by a tag, so a tag that is
// dropped changes nothing about what the model reads - but the drop is
// reported, because a reader has to know that the row and the scenario do
// not say exactly the same thing.
func seedMemoryTags(subject string, tags []string) (model.StringList, string) {
	kept := model.StringList{}
	seen := map[string]bool{}
	dropped := []string{}
	for _, tag := range tags {
		canonical := strings.ToLower(strings.TrimSpace(tag))
		if canonical == "" || !model.ValidMemoryTag(canonical) || seen[canonical] {
			dropped = append(dropped, fmt.Sprintf("%s: %s", subject, tag))
			continue
		}
		seen[canonical] = true
		kept = append(kept, canonical)
	}
	if len(kept) > 5 {
		for _, tag := range kept[5:] {
			dropped = append(dropped, fmt.Sprintf("%s: %s", subject, tag))
		}
		kept = kept[:5]
	}
	return kept, strings.Join(dropped, "; ")
}
