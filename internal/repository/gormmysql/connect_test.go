package gormmysql

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// stubDSN is never dialed: every test that uses it injects a dial function.
const stubDSN = "stub-user:stub-password@tcp(127.0.0.1:3306)/gopherlearn_stub"

func TestNormalizeDSNRequiresAParseableValue(t *testing.T) {
	cases := []struct {
		name string
		dsn  string
		want error
	}{
		{"empty", "", ErrMissingDSN},
		{"blank", "   ", ErrMissingDSN},
		{"no database", "stub-user:stub-password@tcp(127.0.0.1:3306)/", ErrInvalidDSN},
		{"malformed", "stub-user:stub-password@tcp(127.0.0.1:3306)", ErrInvalidDSN},
		{"plain text", "not a dsn at all", ErrInvalidDSN},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			normalized, err := NormalizeDSN(testCase.dsn, time.Second)
			if !errors.Is(err, testCase.want) {
				t.Fatalf("error = %v, want %v", err, testCase.want)
			}
			if normalized != "" {
				t.Fatalf("normalized = %q for a rejected DSN", normalized)
			}
			// A rejected value must never be echoed back: it carries credentials.
			if strings.TrimSpace(testCase.dsn) == "" {
				return
			}
			if strings.Contains(err.Error(), testCase.dsn) {
				t.Fatalf("error leaked the rejected DSN: %v", err)
			}
		})
	}
}

func TestNormalizeDSNAppliesTheSettingsTheRepositoryNeeds(t *testing.T) {
	normalized, err := NormalizeDSN(stubDSN, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	config, err := driver.ParseDSN(normalized)
	if err != nil {
		t.Fatalf("normalized DSN no longer parses: %v", err)
	}
	if !config.ParseTime {
		t.Error("parseTime must be on: created_at is read back as time.Time")
	}
	if config.Loc != time.UTC {
		t.Errorf("loc = %v, want UTC", config.Loc)
	}
	if config.Timeout != 3*time.Second {
		t.Errorf("connect timeout = %v, want 3s", config.Timeout)
	}
	if config.ReadTimeout != DefaultIOTimeout || config.WriteTimeout != DefaultIOTimeout {
		t.Errorf("io timeouts = %v/%v, want %v", config.ReadTimeout, config.WriteTimeout, DefaultIOTimeout)
	}
	if config.Params["charset"] != "utf8mb4" {
		t.Errorf("charset = %q, want utf8mb4", config.Params["charset"])
	}
	if config.DBName != "gopherlearn_stub" || config.User != "stub-user" || config.Passwd != "stub-password" {
		t.Fatalf("normalization changed the target identity: %#v", config)
	}

	// Explicit values in the operator's DSN are respected.
	custom := "stub-user:stub-password@tcp(127.0.0.1:3306)/gopherlearn_stub?timeout=1s&readTimeout=2s&writeTimeout=2s"
	customNormalized, err := NormalizeDSN(custom, 9*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := driver.ParseDSN(customNormalized)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Timeout != time.Second || parsed.ReadTimeout != 2*time.Second {
		t.Fatalf("operator timeouts were overwritten: %#v", parsed)
	}

	// A supplied loc must not weaken the UTC invariant the (created_at, id)
	// ordering depends on.
	shifted, err := NormalizeDSN(stubDSN+"?loc=Asia%2FShanghai", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	parsedShifted, err := driver.ParseDSN(shifted)
	if err != nil {
		t.Fatal(err)
	}
	if parsedShifted.Loc != time.UTC {
		t.Fatalf("loc = %v, want UTC to be forced regardless of the DSN", parsedShifted.Loc)
	}
}

func TestConnectFailsWithoutTouchingTheNetwork(t *testing.T) {
	cases := []struct {
		name string
		dsn  string
		want error
	}{
		{"missing DSN", "", ErrMissingDSN},
		{"invalid DSN", "no-slash-no-database", ErrInvalidDSN},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			dialed := false
			store, err := Connect(context.Background(), testCase.dsn, Options{
				Dial: func(context.Context, string) (*gorm.DB, error) {
					dialed = true
					return nil, nil
				},
			})
			if !errors.Is(err, testCase.want) {
				t.Fatalf("error = %v, want %v", err, testCase.want)
			}
			if store != nil {
				t.Fatal("Connect returned a repository for a rejected DSN")
			}
			if dialed {
				t.Fatal("Connect opened a connection even though the DSN was rejected")
			}
		})
	}
}

func TestConnectSanitizesDialAndMigrationFailures(t *testing.T) {
	dialErr := errors.New("dial tcp " + stubDSN + ": connect: connection refused (password stub-password)")
	_, err := Connect(context.Background(), stubDSN, Options{
		Dial: func(context.Context, string) (*gorm.DB, error) { return nil, dialErr },
	})
	if err == nil {
		t.Fatal("Connect reported no failure when the dial failed")
	}
	message := err.Error()
	for _, secret := range []string{stubDSN, "stub-password", "gopherlearn_stub"} {
		if strings.Contains(message, secret) {
			t.Fatalf("startup error leaked %q: %s", secret, message)
		}
	}
	if !strings.Contains(message, "connection refused") {
		t.Fatalf("sanitizing removed the operator's diagnostics: %s", message)
	}

	// A database that opens but cannot be migrated must also fail startup.
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	repo, err := Connect(canceled, stubDSN, Options{
		Dial: func(context.Context, string) (*gorm.DB, error) { return newPlainTestDB(t), nil },
	})
	if err == nil {
		t.Fatal("Connect succeeded although migration failed")
	}
	if repo != nil {
		t.Fatal("Connect returned a repository although migration failed")
	}
	if !strings.Contains(err.Error(), "migrate") {
		t.Fatalf("error = %v, want it to name the failing step", err)
	}
}

func TestConnectBuildsAWorkingRepositoryThroughTheInjectedDial(t *testing.T) {
	db := newPlainTestDB(t)
	stores, err := Connect(context.Background(), stubDSN, Options{
		Dial: func(context.Context, string) (*gorm.DB, error) { return db, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	core := stores.Repository()
	user, err := core.CreateUser(ctx, &domain.User{Email: "connect@example.com", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := core.CreateSession(ctx, &domain.Session{UserID: user.ID, Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := core.GetSession(ctx, user.ID, session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := core.ListMessages(ctx, user.ID, session.ID); err != nil {
		t.Fatal(err)
	}
	// Connect also migrated the knowledge schema, so the paired store is usable
	// without a second migration step.
	collection, err := stores.Knowledge().CreateCollection(ctx, &domain.Collection{UserID: user.ID, Title: "Notes"})
	if err != nil {
		t.Fatalf("knowledge store after connect: %v", err)
	}
	if collection.ID == "" {
		t.Fatal("knowledge collection was not assigned an id")
	}
}

func TestConnectRequiresAContext(t *testing.T) {
	dialed := false
	// The injected dial also guarantees this test could not open a socket even
	// if the nil-context guard were removed.
	if _, err := Connect(nil, stubDSN, Options{Dial: func(context.Context, string) (*gorm.DB, error) { //nolint:staticcheck
		dialed = true
		return nil, nil
	}}); err == nil {
		t.Fatal("Connect with a nil context succeeded")
	}
	if dialed {
		t.Fatal("Connect dialed despite the missing context")
	}
}

// TestPoolLimitsAreTheDocumentedOnes pins the four numbers the MySQL deployment is
// described by, including the idle bound that exists because a dead connection can
// otherwise sit in the free list for half an hour. database/sql publishes only
// MaxOpenConnections through Stats(), so this can prove the policy and not the
// application of each setter: the application of SetConnMaxIdleTime is what
// TestRealMySQLIdleBoundRetiresAKilledConnection measures against a real server.
func TestPoolLimitsAreTheDocumentedOnes(t *testing.T) {
	if maxOpenConns != 25 {
		t.Fatalf("max open connections = %d, want 25", maxOpenConns)
	}
	if maxIdleConns != 25 {
		t.Fatalf("max idle connections = %d, want 25", maxIdleConns)
	}
	if connMaxLifetime != 30*time.Minute {
		t.Fatalf("connection lifetime = %v, want 30m: the idle bound is the tuning knob, the lifetime stays a backstop", connMaxLifetime)
	}
	if DefaultConnMaxIdleTime != 90*time.Second {
		t.Fatalf("default idle bound = %v, want 90s", DefaultConnMaxIdleTime)
	}
}

func TestConnMaxIdleTimeResolution(t *testing.T) {
	cases := []struct {
		name string
		in   Options
		want time.Duration
	}{
		{"unset takes the documented bound", Options{}, 90 * time.Second},
		{"a test may inject a short one", Options{ConnMaxIdleTime: 250 * time.Millisecond}, 250 * time.Millisecond},
		{"negative passes through as database/sql's own opt-out", Options{ConnMaxIdleTime: -time.Second}, -time.Second},
		// A zero ConnectTimeout must not drag the idle bound along with it: the two
		// knobs are resolved independently.
		{"a resolved connect timeout leaves the idle bound default", Options{ConnectTimeout: DefaultConnectTimeout}, 90 * time.Second},
	}
	for _, tc := range cases {
		if got := tc.in.connMaxIdleTime(); got != tc.want {
			t.Errorf("%s: connMaxIdleTime() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestConnectWithInjectedDialDoesNotClaimToSetPoolLimits guards the shape of the seam
// this package relies on: substituting Dial bypasses the production pool configuration,
// so a test that injected a dial cannot be evidence about pool limits either.
func TestConnectWithInjectedDialDoesNotReachTheProductionDial(t *testing.T) {
	db := newTestDB(t)
	reached := false
	if _, err := Connect(t.Context(), stubDSN, Options{Dial: func(_ context.Context, _ string) (*gorm.DB, error) {
		reached = true
		return db, nil
	}}); err != nil {
		t.Fatal(err)
	}
	if !reached {
		t.Fatal("the injected dial was not used, so a substituted seam would not be testable")
	}
}
