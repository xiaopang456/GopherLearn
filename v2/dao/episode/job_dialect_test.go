package episode

import (
	"GopherAI/model"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// openSQLiteJobDB builds the schema the job table really needs. The isolated
// fixtures migrate it, but a table that is present in one fixture and absent in
// another is exactly how a dialect bug stays invisible until it is exercised.
func openSQLiteJobDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared&_pragma=foreign_keys(0)"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.Session{}, &model.EpisodeProjectionJob{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func createSQLiteJobOwner(t *testing.T, db *gorm.DB) (model.User, model.Session) {
	t.Helper()
	user := model.User{Username: "upsert-" + strings.ToLower(t.Name()), Password: "secret"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	session := model.Session{ID: "session-" + user.Username, UserID: user.ID, UserName: user.Username, Mode: model.SessionModeAgent, Title: "upsert"}
	if err := db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Unscoped().Where("user_id = ?", user.ID).Delete(&model.EpisodeProjectionJob{})
		db.Unscoped().Where("id = ?", session.ID).Delete(&model.Session{})
		db.Unscoped().Where("id = ?", user.ID).Delete(&model.User{})
	})
	return user, session
}

// TestUpsertDesiredRepeatedEnqueueIsIdempotentOnSQLite is the regression for the
// SQLite dialect. The upsert used to emit MySQL-only expressions (VALUES(),
// IF(), GREATEST()), so every enqueue failed on SQLite with a syntax error and
// the episode projection was silently dropped.
func TestUpsertDesiredRepeatedEnqueueIsIdempotentOnSQLite(t *testing.T) {
	db := openSQLiteJobDB(t)
	owner, session := createSQLiteJobOwner(t, db)
	repository := NewRepository(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	first, err := repository.UpsertDesired(ctx, owner.ID, session.ID, 10, now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	if first.DesiredMessageID != 10 || first.Status != model.EpisodeProjectionPending {
		t.Fatalf("first enqueue state = %#v", first)
	}

	// Repeating the same desired target must not move the job.
	again, err := repository.UpsertDesired(ctx, owner.ID, session.ID, 10, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("repeated enqueue: %v", err)
	}
	if !again.AvailableAt.Equal(first.AvailableAt) {
		t.Fatalf("repeated enqueue moved available_at from %v to %v", first.AvailableAt, again.AvailableAt)
	}
	if again.DesiredMessageID != 10 || again.Status != model.EpisodeProjectionPending || again.Attempts != 0 {
		t.Fatalf("repeated enqueue changed the job: %#v", again)
	}
	if again.ID != first.ID {
		t.Fatalf("repeated enqueue created a second row: %q then %q", first.ID, again.ID)
	}

	// A lower desired target must never move the job backwards.
	lower, err := repository.UpsertDesired(ctx, owner.ID, session.ID, 9, now.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("lower enqueue: %v", err)
	}
	if lower.DesiredMessageID != 10 || !lower.AvailableAt.Equal(first.AvailableAt) {
		t.Fatalf("lower enqueue moved the job: %#v", lower)
	}

	// A higher desired target advances the row.
	higher, err := repository.UpsertDesired(ctx, owner.ID, session.ID, 20, now.Add(3*time.Hour))
	if err != nil {
		t.Fatalf("higher enqueue: %v", err)
	}
	if higher.DesiredMessageID != 20 || higher.Status != model.EpisodeProjectionPending {
		t.Fatalf("higher enqueue state = %#v", higher)
	}
	if !higher.AvailableAt.Equal(now.Add(3 * time.Hour)) {
		t.Fatalf("higher enqueue did not postpone available_at: %#v", higher)
	}

	// Exactly one durable row survives the whole sequence.
	var count int64
	if err := db.Model(&model.EpisodeProjectionJob{}).Where("user_id = ? AND session_id = ?", owner.ID, session.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("job rows = %d, want 1", count)
	}
}

// TestUpsertDesiredDoesNotStealALiveLeaseOnSQLite keeps the conflict-update
// semantics identical to MySQL: a new desired target never takes a lease away
// from the worker that is currently projecting.
func TestUpsertDesiredDoesNotStealALiveLeaseOnSQLite(t *testing.T) {
	db := openSQLiteJobDB(t)
	owner, session := createSQLiteJobOwner(t, db)
	repository := NewRepository(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	job, err := repository.UpsertDesired(ctx, owner.ID, session.ID, 10, now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := repository.Claim(ctx, owner.ID, job.ID, now, time.Minute, 3)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Status != model.EpisodeProjectionProcessing || claimed.ClaimedMessageID != 10 {
		t.Fatalf("claim state = %#v", claimed)
	}

	pending, err := repository.UpsertDesired(ctx, owner.ID, session.ID, 20, now.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != model.EpisodeProjectionProcessing || pending.ClaimedMessageID != 10 || pending.DesiredMessageID != 20 {
		t.Fatalf("new desired target stole the live lease: %#v", pending)
	}
	if pending.LeaseToken != claimed.LeaseToken {
		t.Fatalf("lease token changed under a live lease: %#v", pending)
	}
}

// TestEpisodeProjectionJobUpsertUpdatesPerDialect pins the generated SQL. The
// MySQL branch must keep its original shape; the SQLite branch must not contain
// any MySQL-only function.
func TestEpisodeProjectionJobUpsertUpdatesPerDialect(t *testing.T) {
	mysql := episodeProjectionJobUpsertUpdates("mysql")
	if len(mysql) != 10 {
		t.Fatalf("mysql assignments = %d, want 10", len(mysql))
	}
	if mysql[len(mysql)-1].Column.Name != "desired_message_id" {
		t.Fatalf("desired_message_id must be assigned last, got %q", mysql[len(mysql)-1].Column.Name)
	}
	mysqlSQL := renderAssignments(t, mysql)
	for _, required := range []string{"IF(VALUES(desired_message_id)", "GREATEST(desired_message_id, VALUES(desired_message_id))"} {
		if !strings.Contains(mysqlSQL, required) {
			t.Fatalf("mysql upsert lost %q: %s", required, mysqlSQL)
		}
	}

	sqliteUpdates := episodeProjectionJobUpsertUpdates("sqlite")
	if len(sqliteUpdates) != 10 {
		t.Fatalf("sqlite assignments = %d, want 10", len(sqliteUpdates))
	}
	if sqliteUpdates[len(sqliteUpdates)-1].Column.Name != "desired_message_id" {
		t.Fatalf("desired_message_id must be assigned last, got %q", sqliteUpdates[len(sqliteUpdates)-1].Column.Name)
	}
	sqliteSQL := renderAssignments(t, sqliteUpdates)
	for _, forbidden := range []string{"VALUES(", "GREATEST(", "IF("} {
		if strings.Contains(sqliteSQL, forbidden) {
			t.Fatalf("sqlite upsert contains MySQL-only %q: %s", forbidden, sqliteSQL)
		}
	}
	for _, required := range []string{"excluded.desired_message_id", "MAX(desired_message_id, excluded.desired_message_id)", "CASE WHEN"} {
		if !strings.Contains(sqliteSQL, required) {
			t.Fatalf("sqlite upsert lost %q: %s", required, sqliteSQL)
		}
	}
}

// renderAssignments renders the assignments through GORM so the check sees the
// SQL the driver actually emits rather than a hand-written approximation.
func renderAssignments(t *testing.T, assignments clause.Set) string {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file::memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	statement := db.Session(&gorm.Session{DryRun: true}).
		Model(&model.EpisodeProjectionJob{}).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "user_id"}, {Name: "session_id"}},
			DoUpdates: assignments,
		}).
		Create(&model.EpisodeProjectionJob{ID: "shape", UserID: 1, SessionID: "s", DesiredMessageID: 1})
	if statement.Error != nil {
		t.Fatal(statement.Error)
	}
	return statement.Statement.SQL.String()
}
