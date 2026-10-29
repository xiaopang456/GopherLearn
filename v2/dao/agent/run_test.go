package agent

import (
	"errors"
	"testing"

	mysqldriver "github.com/go-sql-driver/mysql"
)

func TestIsMySQLDeadlockUsesDriverCode(t *testing.T) {
	deadlock := &mysqldriver.MySQLError{Number: 1213, Message: "deadlock"}
	if !isMySQLDeadlock(deadlock) || !isMySQLDeadlock(errors.Join(errors.New("wrapped"), deadlock)) {
		t.Fatal("MySQL 1213 must be retryable")
	}
	if isMySQLDeadlock(&mysqldriver.MySQLError{Number: 1062, Message: "duplicate"}) || isMySQLDeadlock(errors.New("deadlock text only")) {
		t.Fatal("only typed MySQL 1213 must be retryable")
	}
}
