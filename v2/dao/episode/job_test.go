package episode

import (
	"errors"
	"testing"

	mysqldriver "github.com/go-sql-driver/mysql"
)

func TestIsEpisodeMySQLDeadlockUsesDriverCode(t *testing.T) {
	deadlock := &mysqldriver.MySQLError{Number: 1213, Message: "deadlock"}
	if !isEpisodeMySQLDeadlock(deadlock) || !isEpisodeMySQLDeadlock(errors.Join(errors.New("wrapped"), deadlock)) {
		t.Fatal("MySQL 1213 must be retryable")
	}
	if isEpisodeMySQLDeadlock(&mysqldriver.MySQLError{Number: 1062, Message: "duplicate"}) || isEpisodeMySQLDeadlock(errors.New("deadlock text only")) {
		t.Fatal("only typed MySQL 1213 must be retryable")
	}
}
