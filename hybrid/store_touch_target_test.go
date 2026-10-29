//go:build v2_real_gates && hybrid_touch_mysql

package hybrid

import (
	"strings"
	"testing"
)

func TestI0TargetTouchSchemaOffline(t *testing.T) {
	s := testSettings(t)
	run := strings.Repeat("a", 32)
	s.Config.MysqlHost = "127.0.0.1"
	s.Config.MysqlPort = 65068
	s.Config.MysqlDatabaseName = "v2_chat_touch_test_" + run
	for _, match := range compareI0Target(s.Config, 65068, run) {
		if !match {
			t.Fatal("owned touch target falsely mismatched")
		}
	}
	s.Config.MysqlDatabaseName = "v2_chat_touch_test_other"
	if compareI0Target(s.Config, 65068, run)["batch_schema_match"] {
		t.Fatal("foreign touch schema matched")
	}
}
