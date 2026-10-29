//go:build v2_real_gates

package hybrid

import (
	originalMySQL "GopherAI/common/mysql"
	"GopherAI/config"
	"GopherAI/entry"
	"context"
	"errors"
	"flag"
	"os"
	"regexp"
	"testing"
	"time"
)

var realGateConfig = flag.String("real-gates-config", "", "explicit disposable independent config; never default config")
var realGateRun = flag.String("real-gates-run-id", "", "explicit 32-hex owned test run identity")
var realGateServer = flag.String("real-gates-mysql-server-uuid", "", "expected disposable MySQL server UUID")
var realGatePasswordStdin = flag.Bool("real-gates-password-stdin", false, "test-only single password on stdin; explicit config must contain no password")

// Test-only guard. The server UUID is independently verified after connection
// and before any schema writes. It is not a declaration of live authorization.
func validateInitializer(s *entry.Settings, runID, serverUUID string) error {
	if s == nil || s.Config == nil || !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(runID) || !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`).MatchString(serverUUID) {
		return errors.New("explicit test identity required")
	}
	c := s.Config
	if c.MysqlHost != "127.0.0.1" || c.MysqlPort < 49152 || c.MysqlPort > 65535 || s.Entry.Migrate {
		return errors.New("explicit disposable loopback resources and migrate=false required")
	}
	if c.MysqlDatabaseName != "v2_gate_test_"+runID && c.MysqlDatabaseName != "v2_chat_touch_test_"+runID {
		return errors.New("exact owned random test schema required")
	}
	return nil
}

// New initialization gate ONLY for an independently created empty test schema.
// The original production initializer and original connection options execute;
// no model, SMTP, Redis, app workers or existing business data are involved.
func TestMySQLInitializeOriginalEmptySchema(t *testing.T) {
	if *realGateConfig == "" {
		t.Skip("set explicit -real-gates-config to initialize only an owned empty disposable schema")
	}
	d := newI0Diagnostic(t.Log)
	d.begin("config_parse")
	s, err := entry.Load(*realGateConfig)
	if err != nil {
		d.fail(err, "explicit isolated configuration invalid")
		t.Fatal("explicit isolated configuration invalid")
	}
	d.success()
	d.begin("config_safety")
	if *realGatePasswordStdin {
		passwordCtx, cancelPassword := context.WithTimeout(context.Background(), 5*time.Second)
		err = applyI0PasswordInput(passwordCtx, s, os.Stdin)
		cancelPassword()
		if err != nil {
			d.fail(nil, "unsafe isolated initialization rejected before connection")
			t.Fatal("unsafe isolated initialization rejected before connection")
		}
	}
	if err = validateInitializer(s, *realGateRun, *realGateServer); err != nil {
		d.fail(err, "unsafe isolated initialization rejected before connection")
		t.Fatal("unsafe isolated initialization rejected before connection")
	}
	if err = config.Use(s.Config); err != nil {
		d.fail(err, "explicit config rejected")
		t.Fatal("explicit config rejected")
	}
	d.success()
	d.begin("mysql_open")
	db, err := originalMySQL.OpenConfigured()
	if err != nil {
		d.fail(err, "cannot open owned disposable MySQL")
		t.Fatal("cannot open owned disposable MySQL")
	}
	d.success()
	d.begin("connection_acquire")
	sqlDB, err := db.DB()
	if err != nil {
		d.fail(err, "cannot obtain owned SQL connection")
		t.Fatal("cannot obtain owned SQL connection")
	}
	d.success()
	d.begin("instance_identity")
	t.Cleanup(func() {
		if sqlDB.Close() != nil {
			t.Error("owned SQL connection close failed")
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var uuid string
	if err = db.WithContext(ctx).Raw("SELECT @@server_uuid").Scan(&uuid).Error; err != nil || uuid != *realGateServer {
		d.fail(err, "owned MySQL identity mismatch; no initialization")
		t.Fatal("owned MySQL identity mismatch; no initialization")
	}
	d.success()
	d.begin("empty_schema")
	var tables int64
	if err = db.WithContext(ctx).Raw("SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE()").Scan(&tables).Error; err != nil || tables != 0 {
		d.fail(err, "initialization requires exact empty schema")
		t.Fatal("initialization requires exact empty schema")
	}
	d.success()
	d.begin("initialize")
	if err = originalMySQL.InitializeSchema(db.WithContext(ctx)); err != nil {
		d.fail(err, "original test schema initialization failed; discard this owned schema")
		t.Fatal("original test schema initialization failed; discard this owned schema")
	}
	d.success()
	d.begin("structure")
	if !checkI0RequiredTables(db.Migrator().HasTable, d) {
		t.Fatal("original required table missing")
	}
	if !db.Migrator().HasColumn("users", "email_normalized") {
		d.failStructure(nil, "original generated email column missing", "users", "email_normalized", "missing_column")
		t.Fatal("original generated email column missing")
	}
	for _, table := range []string{"users", "sessions", "messages"} {
		var count int64
		if err = db.WithContext(ctx).Table(table).Count(&count).Error; err != nil || count != 0 {
			d.failStructure(err, "initializer unexpectedly populated original tables", table, "", "nonempty_or_query_failed")
			t.Fatal("initializer unexpectedly populated original tables")
		}
	}
	d.success()
}

func TestRealGateInitializerRejectsUnsafeBeforeConnection(t *testing.T) {
	run := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	server := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	makeSettings := func() *entry.Settings {
		s := testSettings(t)
		s.Config.MysqlHost = "127.0.0.1"
		s.Config.MysqlPort = 55101
		s.Config.MysqlDatabaseName = "v2_gate_test_" + run
		s.Entry.Migrate = false
		return s
	}
	if validateInitializer(makeSettings(), run, server) != nil {
		t.Fatal("valid declarations rejected")
	}
	for _, change := range []func(*entry.Settings){func(s *entry.Settings) { s.Config.MysqlHost = "business.invalid" }, func(s *entry.Settings) { s.Config.MysqlPort = 3306 }, func(s *entry.Settings) { s.Config.MysqlDatabaseName = "business_test" }, func(s *entry.Settings) { s.Entry.Migrate = true }} {
		s := makeSettings()
		change(s)
		if validateInitializer(s, run, server) == nil {
			t.Fatal("unsafe declaration accepted")
		}
	}
	if validateInitializer(makeSettings(), "", server) == nil || validateInitializer(makeSettings(), run, "") == nil {
		t.Fatal("missing explicit identities accepted")
	}
}
