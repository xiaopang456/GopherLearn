//go:build v2_real_gates

package hybrid

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	originalMySQL "GopherAI/common/mysql"
	"GopherAI/config"
	"GopherAI/entry"
	mysqlDriver "github.com/go-sql-driver/mysql"
)

func classifyI0Network(err error) (string, string) {
	operation, subtype := "unknown", "unknown"
	var op *net.OpError
	if errors.As(err, &op) {
		switch op.Op {
		case "dial", "read", "write", "accept":
			operation = op.Op
		}
	}
	var invalid net.InvalidAddrError
	var address *net.AddrError
	var ne net.Error
	switch {
	case errors.Is(err, context.Canceled):
		subtype = "cancelled"
	case errors.As(err, &invalid), errors.As(err, &address):
		subtype = "address_invalid"
	case errors.Is(err, syscall.ECONNREFUSED):
		subtype = "connection_refused"
	case errors.Is(err, syscall.ETIMEDOUT), errors.Is(err, context.DeadlineExceeded):
		subtype = "timeout"
	default:
		subtype = i0PlatformNetworkCode(err)
		if subtype == "unknown" && errors.As(err, &ne) && ne.Timeout() {
			subtype = "timeout"
		}
	}
	return operation, subtype
}

func TestI0NetworkTypesAndPrivacy(t *testing.T) {
	for _, tc := range []struct {
		err                error
		operation, subtype string
	}{
		{&net.OpError{Op: "dial", Addr: &net.TCPAddr{IP: net.IPv4(10, 11, 12, 13), Port: 12345}, Err: syscall.ECONNREFUSED}, "dial", "connection_refused"},
		{&net.OpError{Op: "read", Err: context.DeadlineExceeded}, "read", "timeout"},
		{&net.AddrError{Err: "PASSWORD_PRIVATE", Addr: "DSN_PRIVATE"}, "unknown", "address_invalid"},
		{net.InvalidAddrError("JWT_PRIVATE"), "unknown", "address_invalid"},
		{&net.OpError{Op: "PASSWORD_PRIVATE", Err: errors.New("JWT_PRIVATE")}, "unknown", "unknown"},
		{context.Canceled, "unknown", "cancelled"},
	} {
		e := classifyI0(tc.err)
		if e.Class != "network_connection" || e.NetworkOperation != tc.operation || e.NetworkSubtype != tc.subtype {
			t.Fatal("safe network category mismatch")
		}
		b, _ := json.Marshal(e)
		for _, private := range []string{"PRIVATE", "10.11.12.13", "12345"} {
			if strings.Contains(string(b), private) {
				t.Fatal("network payload leaked")
			}
		}
	}
}

// Driver hook is scoped to a separate test process, never a production switch.
// The real original GORM/driver path executes; custom tcp dial always returns an
// error before a socket can exist. Child stdout is never published (the original
// logger could include endpoint text). Only fixed assertions leave the parent.
func TestI0OriginalConnectionTargetWithoutDial(t *testing.T) {
	if os.Getenv("I0_CAPTURE_CHILD") != "yes" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestI0OriginalConnectionTargetWithoutDial$", "-test.count=1")
		for _, key := range []string{"PATH", "SYSTEMROOT", "WINDIR", "TEMP", "TMP", "USERPROFILE", "LOCALAPPDATA", "APPDATA", "PROGRAMDATA", "HOMEDRIVE", "HOMEPATH", "COMSPEC", "PATHEXT", "I0_SYNTHETIC_CONFIG"} {
			if v, ok := os.LookupEnv(key); ok {
				cmd.Env = append(cmd.Env, key+"="+v)
			}
		}
		cmd.Env = append(cmd.Env, "I0_CAPTURE_CHILD=yes")
		if _, err := cmd.CombinedOutput(); err != nil {
			t.Fatal("isolated original target capture failed; child output not published")
		}
		return
	}
	path := os.Getenv("I0_SYNTHETIC_CONFIG")
	if path == "" {
		t.Skip("set I0_SYNTHETIC_CONFIG to a synthetic configuration file to run this gate")
	}
	s, err := entry.Load(path)
	if err != nil {
		t.Fatal("synthetic config parse failed")
	}
	if s.Config.MysqlHost != "127.0.0.1" || s.Config.MysqlPort != 55107 {
		t.Fatal("synthetic target fields mismatch")
	}
	if err = config.Use(s.Config); err != nil {
		t.Fatal("synthetic config installation failed")
	}
	calls := 0
	expected := net.JoinHostPort(s.Config.MysqlHost, strconv.Itoa(s.Config.MysqlPort))
	mysqlDriver.RegisterDialContext("tcp", func(ctx context.Context, addr string) (net.Conn, error) {
		calls++
		if addr != expected {
			t.Fatal("original dial target differs from executor config")
		}
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	})
	defer mysqlDriver.DeregisterDialContext("tcp")
	db, err := originalMySQL.OpenConfigured()
	if db != nil || err == nil || calls != 1 {
		t.Fatal("original connection did not stop at intercepted dial")
	}
	e := classifyI0(err)
	if e.Class != "network_connection" || e.NetworkSubtype != "connection_refused" || e.NetworkOperation != "dial" {
		t.Fatal("original dial error not classified safely")
	}
}
