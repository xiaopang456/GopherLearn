package knowledge

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"GopherAI/model"
	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// The legacy recovery test intentionally updates ALL indexing rows. In B06 it
// must not share mutable rows with concurrently running candidate tests. Only
// explicit, marker-proven private B06 harness runs may provision this schema.
//
// This helper used to live next to the candidate-mode schema fixtures. It is
// kept because the legacy DAO integration test still depends on it.
func privateB06LegacySchema(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	config, err := driver.ParseDSN(dsn)
	if err != nil {
		t.Fatal("test DSN")
	}
	base, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal("test database connection")
	}
	sqlBase, _ := base.DB()
	defer sqlBase.Close()
	var token string
	expected := os.Getenv("DOC_B05_OWNER_TOKEN")
	if len(expected) < 32 || base.WithContext(ctx).Raw("SELECT token FROM doc_b05_test_owner WHERE singleton=1").Scan(&token).Error != nil || token != expected {
		t.Fatal("isolated schema requires owned B06 database")
	}
	name := "doc_b06_test_legacy_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if err = base.WithContext(ctx).Exec("CREATE DATABASE `" + name + "`").Error; err != nil {
		t.Fatal("private legacy schema creation")
	}
	config.DBName = name
	db, err := gorm.Open(mysql.Open(config.FormatDSN()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal("private legacy connection")
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { sqlDB.Close() })
	if err = db.WithContext(ctx).Exec("CREATE TABLE doc_b05_test_owner (singleton INT PRIMARY KEY, token VARCHAR(64) NOT NULL)").Error; err != nil {
		t.Fatal(err)
	}
	if err = db.WithContext(ctx).Exec("INSERT INTO doc_b05_test_owner VALUES(1,?)", expected).Error; err != nil {
		t.Fatal("private marker")
	}
	if err = db.WithContext(ctx).AutoMigrate(&model.User{}); err != nil {
		t.Fatal("legacy users schema")
	}
	t.Log("legacy recovery fixture isolated from candidate records in marker-owned private schema")
	return db
}
