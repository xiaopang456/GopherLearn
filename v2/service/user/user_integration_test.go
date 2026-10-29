package user

import (
	"GopherAI/common/code"
	commonmysql "GopherAI/common/mysql"
	"GopherAI/internal/testdb"
	"GopherAI/model"
	"GopherAI/utils"
	"crypto/md5"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	mysqldriver "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestRealMySQLLegacyLoginAndConcurrentRegistration(t *testing.T) {
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasColumn("users", "email_normalized") {
		t.Fatal("run the account migration against the dedicated test database first")
	}
	previousDB := commonmysql.DB
	commonmysql.DB = db
	t.Cleanup(func() { commonmysql.DB = previousDB })

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	legacyEmail := "legacy-" + suffix + "@example.invalid"
	concurrentEmail := "concurrent-" + suffix + "@example.invalid"
	registerEmail := "register-" + suffix + "@example.invalid"
	t.Cleanup(func() {
		_ = db.Unscoped().Where("email IN ?", []string{legacyEmail, concurrentEmail, registerEmail}).Delete(&model.User{}).Error
	})

	legacyPassword := "legacy-pass"
	legacyHash := fmt.Sprintf("%x", md5.Sum([]byte(legacyPassword))) // #nosec G401 -- legacy migration fixture.
	legacyUser := model.User{Username: "legacy-" + suffix[:20], Email: legacyEmail, Password: legacyHash}
	if err := db.Create(&legacyUser).Error; err != nil {
		t.Fatal(err)
	}
	deps := defaultDependencies()
	deps.generateToken = func(int64, string) (string, error) { return "token", nil }
	if _, got := login(deps, " "+strings.ToUpper(legacyEmail)+" ", legacyPassword); got != code.CodeSuccess {
		t.Fatalf("legacy email login code=%d", got)
	}
	var migrated model.User
	if err := db.First(&migrated, legacyUser.ID).Error; err != nil {
		t.Fatal(err)
	}
	if valid, upgrade := utils.VerifyPassword(migrated.Password, legacyPassword); !valid || upgrade {
		t.Fatalf("migrated password valid=%v upgrade=%v", valid, upgrade)
	}

	concurrentUser := model.User{Username: "concurrent-" + suffix[:20], Email: concurrentEmail, Password: legacyHash}
	if err := db.Create(&concurrentUser).Error; err != nil {
		t.Fatal(err)
	}
	var loginWG sync.WaitGroup
	loginCodes := make(chan code.Code, 2)
	for range 2 {
		loginWG.Add(1)
		go func() {
			defer loginWG.Done()
			_, result := login(deps, concurrentEmail, legacyPassword)
			loginCodes <- result
		}()
	}
	loginWG.Wait()
	close(loginCodes)
	for result := range loginCodes {
		if result != code.CodeSuccess {
			t.Fatalf("concurrent legacy login code=%d", result)
		}
	}
	migrated = model.User{}
	if err := db.First(&migrated, concurrentUser.ID).Error; err != nil {
		t.Fatal(err)
	}
	if valid, upgrade := utils.VerifyPassword(migrated.Password, legacyPassword); !valid || upgrade {
		t.Fatalf("concurrent migrated password valid=%v upgrade=%v", valid, upgrade)
	}

	passwordHash, err := utils.HashPassword("password-123")
	if err != nil {
		t.Fatal(err)
	}
	registerDeps := defaultDependencies()
	registerDeps.verifyCaptcha = func(string, string) (bool, error) { return true, nil }
	registerDeps.consumeCaptcha = func(string, string) (bool, error) { return true, nil }
	registerDeps.generateToken = func(int64, string) (string, error) { return "token", nil }
	registerDeps.hashPassword = func(string) (string, error) { return passwordHash, nil }
	var usernameCounter uint64
	registerDeps.randomDigits = func(int) (string, error) {
		return fmt.Sprintf("%011d", atomic.AddUint64(&usernameCounter, 1)+80000000000), nil
	}
	var registerWG sync.WaitGroup
	registerCodes := make(chan code.Code, 2)
	for range 2 {
		registerWG.Add(1)
		go func() {
			defer registerWG.Done()
			_, _, result := register(registerDeps, registerEmail, "password-123", "123456")
			registerCodes <- result
		}()
	}
	registerWG.Wait()
	close(registerCodes)
	successes, conflicts := 0, 0
	for result := range registerCodes {
		switch result {
		case code.CodeSuccess:
			successes++
		case code.CodeUserExist:
			conflicts++
		default:
			t.Fatalf("concurrent registration code=%d", result)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("registration successes=%d conflicts=%d", successes, conflicts)
	}
	var registeredCount int64
	if err := db.Model(&model.User{}).Where("email_normalized = ?", registerEmail).Count(&registeredCount).Error; err != nil {
		t.Fatal(err)
	}
	if registeredCount != 1 {
		t.Fatalf("registered user count=%d", registeredCount)
	}
}
