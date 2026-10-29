//go:build hybrid_touch_mysql

package hybrid

import (
	originalMySQL "GopherAI/common/mysql"
	"GopherAI/config"
	messageDAO "GopherAI/dao/message"
	sessionDAO "GopherAI/dao/session"
	"GopherAI/entry"
	"GopherAI/model"
	"context"
	"flag"
	mysqlDriver "github.com/go-sql-driver/mysql"
	"github.com/xiaopang456/GopherLearn/conversation"
	mysqlDialector "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"os"
	"strings"
	"testing"
	"time"
)

var touchMySQLConfig = flag.String("hybrid-touch-mysql-config", "", "explicit independent preinitialized empty v2_chat_touch_test_* MySQL configuration; no migration")

// This gate is deliberately compiled, not executed, in this delivery. It opens
// ONLY an explicitly named disposable initialized database when invoked later.
// There is no AutoMigrate, Redis, Provider, sleep, or changed-row fault injection.
func TestMySQLAppendMessageSameTimestampZeroChangesCommits(t *testing.T) {
	if *touchMySQLConfig == "" {
		t.Fatal("explicit -hybrid-touch-mysql-config is required")
	}
	settings, err := entry.Load(*touchMySQLConfig)
	if err != nil {
		t.Fatal("explicit isolated configuration is invalid")
	}
	if *touchMySQLPasswordStdin {
		if err := prepareTouchPassword(settings, os.Stdin); err != nil {
			t.Fatal("unsafe touch stdin configuration rejected")
		}
	}
	if !strings.HasPrefix(settings.Config.MysqlDatabaseName, "v2_chat_touch_test_") || settings.Entry.Migrate {
		t.Fatal("preinitialized disposable v2_chat_touch_test_* database with migrate=false required")
	}
	if err := config.Use(settings.Config); err != nil {
		t.Fatal("cannot select explicit isolated configuration")
	}
	db, err := originalMySQL.OpenConfigured()
	if err != nil {
		t.Fatal("cannot open explicit disposable database")
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Error(err)
		}
	})
	if *touchMySQLPasswordStdin {
		var uuid string
		if err := db.Raw("SELECT @@server_uuid").Scan(&uuid).Error; err != nil || uuid != *touchMySQLServer {
			t.Fatal("owned touch MySQL identity mismatch")
		}
	}
	dialector, ok := db.Dialector.(*mysqlDialector.Dialector)
	if !ok || !dialector.DisableDatetimePrecision {
		t.Fatal("original MySQL dialector/time settings changed")
	}
	options, err := mysqlDriver.ParseDSN(dialector.DSN)
	if err != nil || options.ClientFoundRows {
		t.Fatal("original changed-row reporting required; clientFoundRows must remain false")
	}
	for _, table := range []string{"users", "sessions", "messages"} {
		var count int64
		if err := db.Table(table).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("preinitialized empty table required: %s", table)
		}
	}
	// Require original time definitions; never migrate or alter them here.
	type timeColumn struct {
		TableName         string
		ColumnName        string
		DataType          string
		DatetimePrecision *int64
	}
	var columns []timeColumn
	if err := db.Raw("SELECT TABLE_NAME, COLUMN_NAME, DATA_TYPE, DATETIME_PRECISION FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND ((TABLE_NAME = 'sessions' AND COLUMN_NAME IN ('created_at','updated_at')) OR (TABLE_NAME = 'messages' AND COLUMN_NAME = 'created_at'))").Scan(&columns).Error; err != nil {
		t.Fatal("cannot inspect original time columns")
	}
	if len(columns) != 3 {
		t.Fatal("original time columns missing")
	}
	for _, column := range columns {
		if column.DataType != "datetime" || column.DatetimePrecision == nil || *column.DatetimePrecision != 0 {
			t.Fatal("original datetime precision 0 fields required")
		}
	}
	fixed := time.Date(2026, 10, 7, 0, 0, 0, 0, time.Local)
	user := &model.User{ID: 1, Username: "touch-gate", Name: "touch-gate", Password: "synthetic-unused", Email: "touch-gate@invalid.example"}
	if err := db.Create(user).Error; err != nil {
		t.Fatal("cannot seed disposable original User")
	}
	if err := db.Create(&model.Session{ID: "touch-session", UserID: 1, UserName: "touch-gate", Mode: model.SessionModeNormal, CreatedAt: fixed, UpdatedAt: fixed}).Error; err != nil {
		t.Fatal("cannot seed disposable original Session")
	}
	stored, err := sessionDAO.NewRepository(db).GetByIDAndUserID(context.Background(), "touch-session", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.UpdatedAt.Equal(fixed) {
		t.Fatal("explicit whole-second timestamp did not round trip")
	}
	// An actual same-value UPDATE must produce zero with the original options.
	probe := db.Model(&model.Session{}).Where("id = ? AND user_id = ? AND mode = ?", "touch-session", 1, model.SessionModeNormal).UpdateColumn("updated_at", stored.UpdatedAt)
	if probe.Error != nil || probe.RowsAffected != 0 {
		t.Fatalf("same-value MySQL UPDATE must report 0: affected=%d", probe.RowsAffected)
	}
	var observed []int64
	if err := db.Callback().Update().After("gorm:update").Register("test:observe-real-mysql-touch", func(tx *gorm.DB) {
		if tx.Statement.Table == "sessions" && tx.Error == nil {
			observed = append(observed, tx.RowsAffected)
		}
	}); err != nil {
		t.Fatal(err)
	}
	store := &Store{db: db}
	for _, role := range []string{"user", "assistant"} {
		got, err := store.AppendMessage(context.Background(), "1", "touch-session", &conversation.Message{Role: role, Content: "explicit same timestamp " + role, CreatedAt: stored.UpdatedAt})
		if err != nil || got == nil || got.ID == "" {
			t.Fatalf("real unchanged touch did not commit: err=%v", err)
		}
	}
	if len(observed) != 2 || observed[0] != 0 || observed[1] != 0 {
		t.Fatalf("actual adapter MySQL touch counts: %v", observed)
	}
	page, err := messageDAO.NewRepository(db).ListOwnedHistoryPage(context.Background(), 1, "touch-session", 0, 100)
	if err != nil || len(page.Messages) != 2 || !page.Messages[0].IsUser || page.Messages[1].IsUser {
		t.Fatal("original MySQL message DAO did not read back both committed messages")
	}
	after, err := sessionDAO.NewRepository(db).GetByIDAndUserID(context.Background(), "touch-session", 1)
	if err != nil || !after.UpdatedAt.Equal(stored.UpdatedAt) {
		t.Fatal("session timestamp changed unexpectedly")
	}
}
