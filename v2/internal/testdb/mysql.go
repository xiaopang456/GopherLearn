package testdb

import (
	"fmt"
	"os"
	"strings"
	"testing"

	mysqlconfig "github.com/go-sql-driver/mysql"
)

const mysqlDSNEnvironment = "GOPHERAI_MYSQL_TEST_DSN"

// RequireMySQLDSN returns the opt-in integration-test DSN after proving that
// it names a dedicated test database. An unset variable skips the calling test;
// an unsafe or malformed value fails before a connection can be opened.
func RequireMySQLDSN(t testing.TB) string {
	t.Helper()
	dsn := os.Getenv(mysqlDSNEnvironment)
	if dsn == "" {
		t.Skipf("set %s to run MySQL integration tests", mysqlDSNEnvironment)
	}
	if err := ValidateMySQLDSN(dsn); err != nil {
		t.Fatalf("refusing unsafe %s: %v", mysqlDSNEnvironment, err)
	}
	return dsn
}

// ValidateMySQLDSN accepts only database names with an explicit "test" token,
// such as GopherAI_test_stabilize or GopherAI_stabilize_test. Merely containing
// those four letters inside another word is not enough.
func ValidateMySQLDSN(dsn string) error {
	parsed, err := mysqlconfig.ParseDSN(dsn)
	if err != nil {
		return fmt.Errorf("parse DSN: %w", err)
	}
	if parsed.DBName == "" {
		return fmt.Errorf("database name is empty")
	}
	for _, token := range strings.FieldsFunc(strings.ToLower(parsed.DBName), func(r rune) bool {
		return r == '_' || r == '-'
	}) {
		if token == "test" {
			return nil
		}
	}
	return fmt.Errorf("database name %q has no explicit test token", parsed.DBName)
}
