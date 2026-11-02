package conversation

import (
	"context"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"testing"
)

func TestObserveJobsReadsBothTablesAndFailsClosed(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"CREATE TABLE episode_projection_jobs (id TEXT, status TEXT, attempts INT)", "CREATE TABLE memory_extraction_jobs (id TEXT, status TEXT, attempts INT)", "INSERT INTO episode_projection_jobs VALUES ('a','pending',0)", "INSERT INTO memory_extraction_jobs VALUES ('b','completed',1)"} {
		if err = db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	jobs, counts, err := ObserveJobs(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 || counts["episode"].Pending != 1 || counts["memory"].Completed != 1 {
		t.Fatalf("jobs=%+v counts=%+v", jobs, counts)
	}
	if err = db.Exec("DROP TABLE memory_extraction_jobs").Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err = ObserveJobs(context.Background(), db); err == nil {
		t.Fatal("missing table accepted")
	}
}
