package model

import (
	"GopherAI/internal/testdb"
	"fmt"
	"testing"

	mysqlconfig "github.com/go-sql-driver/mysql"
	mysqldriver "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// TestMessageUserNameColumnStoresAFullLengthUsername is the real-database half of
// the column width: the declared width is applied by the migration, and a
// username that fills it is stored whole rather than refused or cut.
func TestMessageUserNameColumnStoresAFullLengthUsername(t *testing.T) {
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&User{}, &Session{}, &Message{}); err != nil {
		t.Fatal(err)
	}

	parsed, err := mysqlconfig.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	var width int
	if err := db.Raw("SELECT character_maximum_length FROM information_schema.columns "+
		"WHERE table_schema = ? AND table_name = ? AND column_name = ?",
		parsed.DBName, "messages", "user_name").Scan(&width).Error; err != nil {
		t.Fatal(err)
	}
	want := declaredWidth(t, &Message{}, "UserName")
	if width != want {
		t.Fatalf("messages.user_name is varchar(%d) in the database, want %d", width, want)
	}

	name := fmt.Sprintf("%050d", 1)
	message := Message{SessionID: "width-check", UserName: name, Content: "你好"}
	if err := db.Create(&message).Error; err != nil {
		t.Fatalf("store a full-length username: %v", err)
	}
	var stored Message
	if err := db.Where("id = ?", message.ID).First(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.UserName != name {
		t.Fatalf("stored username = %q, want %q", stored.UserName, name)
	}
}
