package gormmysql

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// TestLockingReadStatementIsWhatTheCeilingsAssume checks the generated SQL on the
// MySQL dialect without asking a server anything: the pool behind this handle refuses
// to run a statement and GORM is in dry-run mode, so all the test can produce is
// text. That is deliberate - an earlier version used the real driver with an
// unreachable DSN and found that gorm.Open dials during Initialise anyway.
//
// The statement shape is load-bearing twice. Scanning a SELECT * into one string is
// not something GORM promises, and a locking read that silently degraded into a plain
// read would let the store measure quotas against a snapshot another transaction was
// about to change - while still returning the right answers in every offline test,
// because SQLite serialises its writers regardless. Both failure modes look exactly
// like passing.
func TestLockingReadStatementIsWhatTheCeilingsAssume(t *testing.T) {
	db := dryRunMySQL(t)
	var ids []string
	// The production builder, not a copy of its clauses. Find rather than Scan only
	// because GORM builds SQL for Find in dry-run mode and refuses Scan - the clause
	// set being checked is identical either way.
	statement := userRowLockStatement(db, "user-1").Find(&ids).Statement
	if statement.Error != nil {
		t.Fatal(statement.Error)
	}
	built := statement.SQL.String()
	for _, want := range []string{"`users`", "FOR UPDATE", "LIMIT"} {
		if !strings.Contains(built, want) {
			t.Errorf("the locking read is %q, want it to mention %s", built, want)
		}
	}
	// One named column, not the whole row: see userRowLockStatement.
	if strings.Contains(built, "SELECT *") {
		t.Errorf("the locking read selects every column: %q", built)
	}
	if !strings.HasPrefix(built, "SELECT id") && !strings.HasPrefix(built, "SELECT `id`") {
		t.Errorf("the locking read does not select id alone: %q", built)
	}
}

// TestLockingReadIsNotIssuedOnEnginesThatIgnoreIt is the other half of the guard:
// what SQLite sees must contain no FOR UPDATE clause at all, because an engine that
// ignores it must not be led to report a lock it never observed.
func TestLockingReadIsNotIssuedOnEnginesThatIgnoreIt(t *testing.T) {
	db := openPlainTestDB(t, filepath.Join(t.TempDir(), "lock-guard.db"))
	if err := Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	if db.Dialector.Name() == "mysql" {
		t.Fatal("this case is about engines that ignore the clause")
	}
	var ids []string
	// lockUserRow is the production path: on this engine it returns without a query,
	// and no principal - real or absent - may be refused for missing a users row.
	if err := lockUserRow(db, "user-who-does-not-exist"); err != nil {
		t.Errorf("lockUserRow on %s = %v, want no claim about a lock", db.Dialector.Name(), err)
	}
	statement := db.Table("users").Select("id").Where("id = ?", "user-1").Limit(1).Find(&ids).Statement
	if strings.Contains(statement.SQL.String(), "FOR UPDATE") {
		t.Errorf("%s was asked to lock a row: %q", db.Dialector.Name(), statement.SQL.String())
	}
}

// dryRunMySQL builds a MySQL-dialect GORM handle that cannot reach a server: the
// connection pool refuses to run anything, so the only thing this can produce is
// generated SQL. An earlier version of this helper used a real driver with an
// unreachable DSN and discovered that gorm.Open still dials during Initialise, which
// is exactly what a test named "dry run" must not do.
func dryRunMySQL(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(mysql.New(mysql.Config{
		Conn:                      refuseToExecute{},
		SkipInitializeWithVersion: true,
	}), &gorm.Config{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if db.Dialector.Name() != "mysql" {
		t.Fatalf("dialector is %q, want mysql", db.Dialector.Name())
	}
	if !supportsRowLock(db.Dialector.Name()) {
		t.Fatal("mysql must be treated as a row-locking engine or the ceilings have no mechanism")
	}
	return db
}

// refuseToExecute is a gorm.ConnPool that never runs a statement.
type refuseToExecute struct{}

var errDryRunPool = errors.New("the dry-run pool must not execute SQL")

func (refuseToExecute) PrepareContext(context.Context, string) (*sql.Stmt, error) {
	return nil, errDryRunPool
}

func (refuseToExecute) ExecContext(context.Context, string, ...any) (sql.Result, error) {
	return nil, errDryRunPool
}

func (refuseToExecute) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	return nil, errDryRunPool
}

func (refuseToExecute) QueryRowContext(context.Context, string, ...any) *sql.Row {
	// There is no way to build a *sql.Row without a driver, and nothing should ask
	// for one: a caller that reaches this has tried to run SQL and will fail on the
	// nil rather than on a network.
	return nil
}
