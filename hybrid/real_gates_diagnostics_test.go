//go:build v2_real_gates

package hybrid

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"GopherAI/config"
	"GopherAI/entry"
	mysqlDriver "github.com/go-sql-driver/mysql"
)

// Test instrumentation only: no free-form error message or configuration is emitted.
type i0Diagnostic struct {
	log        func(...any)
	step, last string
}
type i0Evidence struct {
	CheckTable       string `json:"check_table,omitempty"`
	CheckColumn      string `json:"check_column,omitempty"`
	CheckFailure     string `json:"check_failure,omitempty"`
	Step             string `json:"step"`
	Result           string `json:"result"`
	Last             string `json:"last_success"`
	Class            string `json:"class"`
	Assertion        string `json:"assertion,omitempty"`
	Number           uint16 `json:"mysql_number,omitempty"`
	State            string `json:"sqlstate,omitempty"`
	NetworkOperation string `json:"network_operation,omitempty"`
	NetworkSubtype   string `json:"network_subtype,omitempty"`
}

func newI0Diagnostic(log func(...any)) *i0Diagnostic { return &i0Diagnostic{log: log} }
func (d *i0Diagnostic) emit(e i0Evidence)            { b, _ := json.Marshal(e); d.log("I0_DIAG " + string(b)) }
func (d *i0Diagnostic) begin(step string) {
	d.step = step
	d.emit(i0Evidence{Step: step, Result: "begin", Last: d.last, Class: "none"})
}
func (d *i0Diagnostic) success() {
	d.last = d.step
	d.emit(i0Evidence{Step: d.step, Result: "success", Last: d.last, Class: "none"})
	// Original I0 calls this only after config.Use succeeds. Never call the
	// fallback loader on failed parse/safety paths or unconfigured unit tests.
	if d.step == "config_safety" && *realGateExpectedPort > 0 {
		b, _ := json.Marshal(compareI0Target(config.GetConfig(), *realGateExpectedPort, *realGateRun))
		d.log("I0_TARGET " + string(b))
	}
}

var realGateExpectedPort = flag.Int("real-gates-expected-port", 0, "diagnostic only: authorized disposable port; no guard override")

func compareI0Target(c *config.Config, expectedPort int, run string) map[string]bool {
	return map[string]bool{
		"config_available":        c != nil,
		"expected_port_available": expectedPort >= 49152 && expectedPort <= 65535,
		"host_loopback_match":     c != nil && c.MysqlHost == "127.0.0.1",
		"authorized_port_match":   c != nil && c.MysqlPort == expectedPort,
		"batch_schema_match":      c != nil && (c.MysqlDatabaseName == "v2_gate_test_"+run || c.MysqlDatabaseName == "v2_chat_touch_test_"+run),
	}
}
func (d *i0Diagnostic) fail(err error, assertion string) {
	e := classifyI0(err)
	e.Step = d.step
	e.Last = d.last
	e.Result = "failure"
	e.Assertion = assertion
	if err != nil && (d.step == "config_parse" || d.step == "config_safety") {
		e.Class = "configuration"
	}
	d.emit(e)
}
func classifyI0(err error) i0Evidence {
	e := i0Evidence{Class: "unknown"}
	if err == nil {
		e.Class = "assertion"
		return e
	}
	var me *mysqlDriver.MySQLError
	if errors.As(err, &me) {
		// Reviewed numeric categories; unreviewed server codes are explicitly unknown.
		switch me.Number {
		case 1045:
			e.Class = "authentication"
		case 1044, 1142, 1143, 1227:
			e.Class = "permission"
		case 1050, 1054, 1060, 1061, 1062, 1064, 1146:
			e.Class = "sql_execution"
		default:
			return e
		}
		e.Number = me.Number
		state := string(me.SQLState[:])
		valid := true
		for _, c := range state {
			if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z') {
				valid = false
			}
		}
		if valid && len(state) == 5 && (state == "28000" || state == "42000" || state == "42S01" || state == "42S02" || state == "42S21" || state == "42S22" || state == "23000") {
			e.State = state
		}
		return e
	}
	var ne net.Error
	if errors.As(err, &ne) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		e.Class = "network_connection"
		e.NetworkOperation, e.NetworkSubtype = classifyI0Network(err)
	}
	return e
}

func TestI0DiagnosticStepsAndPrivacy(t *testing.T) {
	steps := []string{"config_parse", "config_safety", "mysql_open", "connection_acquire", "instance_identity", "empty_schema", "initialize", "structure"}
	for i, step := range steps {
		var rows []i0Evidence
		d := newI0Diagnostic(func(args ...any) {
			var e i0Evidence
			if json.Unmarshal([]byte(args[0].(string)[8:]), &e) != nil {
				t.Fatal("invalid safe event")
			}
			rows = append(rows, e)
		})
		for _, previous := range steps[:i] {
			d.begin(previous)
			d.success()
		}
		d.begin(step)
		d.fail(errors.New("password=PRIVATE DSN JWT SQL secret"), "explicit isolated configuration invalid")
		last := rows[len(rows)-1]
		if last.Step != step || last.Result != "failure" {
			t.Fatal("failure step lost")
		}
		expected := ""
		if i > 0 {
			expected = steps[i-1]
		}
		if last.Last != expected {
			t.Fatal("last successful step lost")
		}
		if last.Class == "none" {
			t.Fatal("missing failure classification")
		}
	}
	for _, tc := range []struct {
		err    error
		class  string
		number uint16
	}{
		{nil, "assertion", 0}, {errors.New("password=PRIVATE"), "unknown", 0},
		{&net.OpError{Op: "dial", Err: errors.New("PRIVATE")}, "network_connection", 0},
		{&mysqlDriver.MySQLError{Number: 1045, SQLState: [5]byte{'2', '8', '0', '0', '0'}, Message: "PRIVATE"}, "authentication", 1045},
		{&mysqlDriver.MySQLError{Number: 1142, Message: "PRIVATE"}, "permission", 1142},
		{&mysqlDriver.MySQLError{Number: 1064, Message: "PRIVATE"}, "sql_execution", 1064},
		{&mysqlDriver.MySQLError{Number: 9999, Message: "PRIVATE"}, "unknown", 0},
	} {
		e := classifyI0(tc.err)
		if e.Class != tc.class || e.Number != tc.number {
			t.Fatal("wrong safe category")
		}
		b, _ := json.Marshal(e)
		if strings.Contains(string(b), "PRIVATE") {
			t.Fatal("private payload escaped")
		}
		for _, c := range string(b) {
			if c == '\n' {
				t.Fatal("unexpected payload")
			}
		}
	}
}

// Executor writes a synthetic config to a temporary file; Load is real and no resource is opened.
func TestI0ExecutorSyntheticConfigLoad(t *testing.T) {
	path := os.Getenv("I0_SYNTHETIC_CONFIG")
	if path == "" {
		path = filepath.Join("..", "tutorial", "v2-real-gates-plan", "i0-diagnostics", "synthetic.toml")
	}
	s, err := entry.Load(path)
	if err != nil {
		t.Fatal("synthetic executor configuration rejected by entry.Load")
	}
	if err = validateInitializer(s, "cccccccccccccccccccccccccccccccc", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"); err != nil {
		t.Fatal("synthetic safety rejected")
	}
}
