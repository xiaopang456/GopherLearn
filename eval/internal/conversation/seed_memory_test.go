package conversation

import (
	"context"
	"testing"
	"time"

	"GopherAI/model"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// memoryTestDB is an in-memory database with the product's own memory table.
func memoryTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(&model.LongTermMemory{}); err != nil {
		t.Fatal(err)
	}
	return db
}

// TestSeededMemoryIsWrittenOnceAndKeepsItsDate is the idempotency rule of the
// seeded profile memory: the same memory written twice is one row, and the
// row carries the save date of the scenario and not the time of the run.
func TestSeededMemoryIsWrittenOnceAndKeepsItsDate(t *testing.T) {
	db := memoryTestDB(t)
	spec := SeedMemory{
		Scope:     "global",
		Subject:   "常用编辑器",
		Content:   "常用编辑器: Neovim",
		Tags:      []string{"工具", "preference"},
		Confirmed: true,
		SavedAt:   "2026-01-14",
	}
	memory, dropped, err := buildSeedMemory(7, spec)
	if err != nil {
		t.Fatal(err)
	}
	if dropped != "常用编辑器: 工具" {
		t.Fatalf("dropped=%q", dropped)
	}
	want := time.Date(2026, time.January, 14, 0, 0, 0, 0, time.UTC)
	if !memory.CreatedAt.Equal(want) || !memory.UpdatedAt.Equal(want) {
		t.Fatalf("dates=%v %v", memory.CreatedAt, memory.UpdatedAt)
	}
	if memory.ContentHash != model.MemoryContentHash(memory.Content) {
		t.Fatal("content hash does not match the content")
	}
	if !memory.UserConfirmed || memory.Scope != model.MemoryScopeGlobal || memory.Status != model.LongTermMemoryActive {
		t.Fatalf("memory=%+v", memory)
	}
	if len(memory.Tags) != 1 || memory.Tags[0] != "preference" {
		t.Fatalf("tags=%v", memory.Tags)
	}
	if memory.Confidence < 0.6 {
		t.Fatalf("confidence=%v is below the product's recall threshold", memory.Confidence)
	}

	report := SeedMemoryReport{}
	if err = seedMemoriesInTx(db, 7, []model.LongTermMemory{memory}, &report); err != nil {
		t.Fatal(err)
	}
	if report.Written != 1 || report.AlreadyThere != 0 {
		t.Fatalf("first write=%+v", report)
	}
	// The second write is the same row: a run that is restarted writes the
	// same memory again, and the learner keeps one memory, not two.
	if err = seedMemoriesInTx(db, 7, []model.LongTermMemory{memory}, &report); err != nil {
		t.Fatal(err)
	}
	if report.Written != 1 || report.AlreadyThere != 1 {
		t.Fatalf("second write=%+v", report)
	}
	var count int64
	if err = db.Model(&model.LongTermMemory{}).Where("user_id = ?", 7).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("rows=%d", count)
	}
	var stored model.LongTermMemory
	if err = db.Where("id = ?", memory.ID).First(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if !stored.CreatedAt.Equal(want) || !stored.UpdatedAt.Equal(want) {
		t.Fatalf("stored dates=%v %v", stored.CreatedAt, stored.UpdatedAt)
	}
	if stored.Content != memory.Content || stored.ContentHash != memory.ContentHash {
		t.Fatalf("stored=%+v", stored)
	}
}

// TestSeededMemoryRefusals is the table of the specifications the driver has
// to refuse: a memory of the wrong scope, one without a date, one without a
// body, and one of another user.
func TestSeededMemoryRefusals(t *testing.T) {
	base := SeedMemory{Scope: "global", Subject: "常用编辑器", Content: "常用编辑器: Neovim", Confirmed: true, SavedAt: "2026-01-14"}
	cases := []struct {
		name string
		edit func(*SeedMemory)
	}{
		{"topic scope", func(m *SeedMemory) { m.Scope = "topic" }},
		{"no save date", func(m *SeedMemory) { m.SavedAt = "" }},
		{"no content", func(m *SeedMemory) { m.Content = "  " }},
		{"no subject", func(m *SeedMemory) { m.Subject = "" }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			spec := base
			testCase.edit(&spec)
			if _, _, err := buildSeedMemory(7, spec); err == nil {
				t.Fatal("accepted an invalid seeded memory")
			}
		})
	}
	if _, _, err := buildSeedMemory(0, base); err == nil {
		t.Fatal("accepted a seeded memory without an identity")
	}
}

// TestSeededMemoryIsReadBack is the read after the write: the attempt has to
// be able to show what is really in the database.
func TestSeededMemoryIsReadBack(t *testing.T) {
	db := memoryTestDB(t)
	memory, _, err := buildSeedMemory(7, SeedMemory{Scope: "global", Subject: "常用编辑器", Content: "常用编辑器: Neovim", Confirmed: true, SavedAt: "2026-01-14"})
	if err != nil {
		t.Fatal(err)
	}
	report := SeedMemoryReport{}
	if err = seedMemoriesInTx(db, 7, []model.LongTermMemory{memory}, &report); err != nil {
		t.Fatal(err)
	}
	var rows []model.LongTermMemory
	if err = db.Where("user_id = ?", 7).Order("id ASC").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Content != memory.Content {
		t.Fatalf("rows=%+v", rows)
	}
}

// TestSeededMemoryOfAnotherUserIsNotRead is the isolation rule: the memories
// of one evaluation user are not the memories of another.
func TestSeededMemoryOfAnotherUserIsNotRead(t *testing.T) {
	db := memoryTestDB(t)
	memory, _, err := buildSeedMemory(7, SeedMemory{Scope: "global", Subject: "常用编辑器", Content: "常用编辑器: Neovim", Confirmed: true, SavedAt: "2026-01-14"})
	if err != nil {
		t.Fatal(err)
	}
	report := SeedMemoryReport{}
	if err = seedMemoriesInTx(db, 7, []model.LongTermMemory{memory}, &report); err != nil {
		t.Fatal(err)
	}
	var rows []model.LongTermMemory
	if err = db.Where("user_id = ?", 8).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows=%+v", rows)
	}
}

// TestBindRunsGivesEveryCallItsRun is the attribution rule of an attempt with
// several questions: a call belongs to the question whose window it was sent
// in, and a call of the background work belongs to the last run.
func TestBindRunsGivesEveryCallItsRun(t *testing.T) {
	start := time.Date(2026, time.January, 14, 10, 0, 0, 0, time.UTC)
	questions := []QuestionResult{
		{Index: 0, RunID: "run-1", StartedAt: start, FinishedAt: start.Add(time.Minute)},
		{Index: 1, RunID: "run-2", StartedAt: start.Add(time.Minute), FinishedAt: start.Add(2 * time.Minute)},
	}
	calls := []ModelCall{
		{ID: 1, Started: start.Add(10 * time.Second)},
		{ID: 2, Started: start.Add(70 * time.Second)},
		{ID: 3, Started: start.Add(3 * time.Minute)},
	}
	bound := BindRuns(calls, "session-1", questions)
	if bound[0].RunID != "run-1" || bound[1].RunID != "run-2" {
		t.Fatalf("bound=%+v", bound)
	}
	if bound[2].RunID != "run-2" {
		t.Fatalf("background call=%+v", bound[2])
	}
	for _, call := range bound {
		if call.SessionID != "session-1" {
			t.Fatalf("session=%+v", call)
		}
	}
}

// TestReadSeedMemoriesReadsWhatWasWritten checks the read on the sandbox
// path itself, on an in-memory database.
func TestReadSeedMemoriesReadsWhatWasWritten(t *testing.T) {
	db := memoryTestDB(t)
	memory, _, err := buildSeedMemory(7, SeedMemory{Scope: "global", Subject: "常用编辑器", Content: "常用编辑器: Neovim", Confirmed: true, SavedAt: "2026-01-14"})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Create(&memory).Error; err != nil {
		t.Fatal(err)
	}
	rows, err := readSeedMemories(context.Background(), db, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != memory.ID {
		t.Fatalf("rows=%+v", rows)
	}
}
