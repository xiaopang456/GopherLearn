//go:build hybrid_touch_mysql

package hybrid

import (
	"GopherAI/entry"
	"context"
	"flag"
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"
)

var touchMySQLPasswordStdin = flag.Bool("hybrid-touch-password-stdin", false, "test-only bounded password input; config password must be empty")
var touchMySQLRun = flag.String("hybrid-touch-run-id", "", "exact disposable touch schema identity")
var touchMySQLServer = flag.String("hybrid-touch-server-uuid", "", "expected independently verified MySQL identity")

func prepareTouchPassword(s *entry.Settings, input io.ReadCloser) error {
	if s == nil || s.Config == nil || !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(*touchMySQLRun) ||
		!regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`).MatchString(*touchMySQLServer) ||
		s.Config.MysqlDatabaseName != "v2_chat_touch_test_"+*touchMySQLRun || s.Config.MysqlHost != "127.0.0.1" ||
		s.Config.MysqlPort < 49152 || s.Config.MysqlPort > 65535 || s.Entry.Migrate {
		return fmt.Errorf("explicit owned touch resource required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return applyI0PasswordInput(ctx, s, input)
}

func TestTouchPasswordInputOffline(t *testing.T) {
	previousRun, previousServer := *touchMySQLRun, *touchMySQLServer
	t.Cleanup(func() { *touchMySQLRun, *touchMySQLServer = previousRun, previousServer })
	*touchMySQLRun = strings.Repeat("a", 32)
	*touchMySQLServer = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	makeSettings := func() *entry.Settings {
		s := testSettings(t)
		s.Config.MysqlPassword = ""
		s.Config.MysqlHost = "127.0.0.1"
		s.Config.MysqlPort = 65068
		s.Config.MysqlDatabaseName = "v2_chat_touch_test_" + *touchMySQLRun
		s.Entry.Migrate = false
		return s
	}
	s := makeSettings()
	if prepareTouchPassword(s, io.NopCloser(strings.NewReader(strings.Repeat("d", 64)+"\n"))) != nil || s.Config.MysqlPassword != strings.Repeat("d", 64) {
		t.Fatal("valid stdin rejected")
	}
	for _, change := range []func(*entry.Settings){func(s *entry.Settings) { s.Config.MysqlHost = "other" }, func(s *entry.Settings) { s.Config.MysqlDatabaseName = "v2_chat_touch_test_other" }, func(s *entry.Settings) { s.Entry.Migrate = true }, func(s *entry.Settings) { s.Config.MysqlPassword = "existing" }} {
		s := makeSettings()
		change(s)
		if prepareTouchPassword(s, io.NopCloser(strings.NewReader(strings.Repeat("d", 64)+"\n"))) == nil {
			t.Fatal("unsafe input accepted")
		}
	}
	for _, value := range []string{"", "bad\n", strings.Repeat("d", 64) + "\nextra"} {
		if prepareTouchPassword(makeSettings(), io.NopCloser(strings.NewReader(value))) == nil {
			t.Fatal("malformed input accepted")
		}
	}
	*touchMySQLRun = "bad"
	if prepareTouchPassword(makeSettings(), io.NopCloser(strings.NewReader(""))) == nil {
		t.Fatal("bad run accepted")
	}
}
