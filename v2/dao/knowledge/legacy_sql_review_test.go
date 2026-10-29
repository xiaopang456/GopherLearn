package knowledge

import (
	"GopherAI/model"
	"context"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"strings"
	"testing"
)

// SQL generation only: no listener, socket or database is used.
func TestCodexLegacyInsertDoesNotRequireCandidateColumns(t *testing.T) {
	db, err := gorm.Open(mysql.New(mysql.Config{DSN: "review:review@tcp(127.0.0.1:1)/unused?parseTime=true", SkipInitializeWithVersion: true}), &gorm.Config{DryRun: true, DisableAutomaticPing: true, SkipDefaultTransaction: true})
	if err != nil {
		t.Fatal(err)
	}
	var statement string
	err = db.Callback().Create().After("gorm:create").Register("review:capture", func(tx *gorm.DB) { statement = tx.Statement.SQL.String() })
	if err != nil {
		t.Fatal(err)
	}
	err = NewRepository(db).Create(context.Background(), &model.KnowledgeDocument{ID: "legacy-doc", UserID: 7, OriginalName: "doc.txt", StoragePath: "7/legacy-doc/source-v1.txt", FileType: "txt", ContentHash: strings.Repeat("a", 64), FileSize: 4, Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	if statement == "" {
		t.Fatal("missing SQL; invalid review setup")
	}
	t.Log(statement)
	for _, column := range []string{"processing_job_id", "active_execution_id", "active_pipeline_fingerprint"} {
		if strings.Contains(statement, "`"+column+"`") {
			t.Errorf("legacy Repository.Create requires uninstalled candidate column %s", column)
		}
	}
}
