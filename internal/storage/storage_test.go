package storage

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/knowledge"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

func envLookup(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func TestConfigFromEnvironment(t *testing.T) {
	cases := []struct {
		name           string
		environment    map[string]string
		wantMode       Mode
		wantDSN        string
		wantTimeout    time.Duration
		wantErrMessage string
	}{
		{name: "unset means memory", environment: map[string]string{}, wantMode: ModeMemory, wantTimeout: DefaultConnectTimeout},
		{name: "blank means memory", environment: map[string]string{EnvStorage: "   "}, wantMode: ModeMemory, wantTimeout: DefaultConnectTimeout},
		{name: "explicit memory", environment: map[string]string{EnvStorage: "memory"}, wantMode: ModeMemory, wantTimeout: DefaultConnectTimeout},
		{name: "memory is case and space insensitive", environment: map[string]string{EnvStorage: "  MEMORY "}, wantMode: ModeMemory, wantTimeout: DefaultConnectTimeout},
		{
			name:        "mysql with dsn",
			environment: map[string]string{EnvStorage: "mysql", EnvMySQLDSN: "  user:pass@tcp(db:3306)/learn  "},
			wantMode:    ModeMySQL, wantDSN: "user:pass@tcp(db:3306)/learn", wantTimeout: DefaultConnectTimeout,
		},
		{
			name:           "mysql without dsn is refused",
			environment:    map[string]string{EnvStorage: "mysql"},
			wantErrMessage: ErrMissingDSN.Error(),
		},
		{
			name:           "mysql with a blank dsn is refused",
			environment:    map[string]string{EnvStorage: "mysql", EnvMySQLDSN: "   "},
			wantErrMessage: ErrMissingDSN.Error(),
		},
		{
			name:           "unknown mode is refused",
			environment:    map[string]string{EnvStorage: "postgres", EnvMySQLDSN: "whatever"},
			wantErrMessage: `unsupported GOPHERLEARN_STORAGE value: "postgres"`,
		},
		{
			name:        "custom connect timeout",
			environment: map[string]string{EnvStorage: "mysql", EnvMySQLDSN: "d", EnvMySQLConnectTimeout: "2s"},
			wantMode:    ModeMySQL, wantDSN: "d", wantTimeout: 2 * time.Second,
		},
		{
			name:           "zero timeout is refused",
			environment:    map[string]string{EnvStorage: "memory", EnvMySQLConnectTimeout: "0s"},
			wantErrMessage: ErrInvalidTimeout.Error(),
		},
		{
			name:           "negative timeout is refused",
			environment:    map[string]string{EnvStorage: "memory", EnvMySQLConnectTimeout: "-3s"},
			wantErrMessage: ErrInvalidTimeout.Error(),
		},
		{
			name:           "unparseable timeout is refused",
			environment:    map[string]string{EnvStorage: "memory", EnvMySQLConnectTimeout: "five seconds"},
			wantErrMessage: ErrInvalidTimeout.Error(),
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config, err := ConfigFromEnvironment(envLookup(testCase.environment))
			if testCase.wantErrMessage != "" {
				if err == nil || !strings.Contains(err.Error(), testCase.wantErrMessage) {
					t.Fatalf("error = %v, want it to mention %q", err, testCase.wantErrMessage)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if config.Mode != testCase.wantMode {
				t.Fatalf("mode = %q, want %q", config.Mode, testCase.wantMode)
			}
			if config.MySQLDSN != testCase.wantDSN {
				t.Fatalf("dsn = %q, want %q", config.MySQLDSN, testCase.wantDSN)
			}
			if config.ConnectTimeout != testCase.wantTimeout {
				t.Fatalf("timeout = %v, want %v", config.ConnectTimeout, testCase.wantTimeout)
			}
		})
	}
}

func TestConfigFromEnvironmentWithNilLookupFallsBackToMemory(t *testing.T) {
	config, err := ConfigFromEnvironment(nil)
	if err != nil {
		t.Fatal(err)
	}
	if config.Mode != ModeMemory {
		t.Fatalf("mode = %q, want memory", config.Mode)
	}
}

func TestConfigFromEnvironmentNeverEchoesTheDSN(t *testing.T) {
	secret := "user:super-secret@tcp(private-host:3306)/learn"
	_, err := ConfigFromEnvironment(envLookup(map[string]string{EnvStorage: "unknown-engine", EnvMySQLDSN: secret}))
	if err == nil {
		t.Fatal("expected a rejected mode")
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("configuration error leaked the DSN: %v", err)
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name   string
		config Config
		want   error
	}{
		{"memory", Config{Mode: ModeMemory}, nil},
		{"memory ignores a dsn", Config{Mode: ModeMemory, MySQLDSN: "anything"}, nil},
		{"mysql with dsn", Config{Mode: ModeMySQL, MySQLDSN: "d"}, nil},
		{"mysql without dsn", Config{Mode: ModeMySQL}, ErrMissingDSN},
		{"unknown mode", Config{Mode: Mode("cockroachdb")}, ErrUnsupportedMode},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.config.Validate()
			if !errors.Is(err, testCase.want) {
				t.Fatalf("error = %v, want %v", err, testCase.want)
			}
		})
	}
}

func TestConnectMemoryNeverNeedsADatabase(t *testing.T) {
	called := false
	stores, err := Connect(context.Background(), Config{Mode: ModeMemory, MySQLDSN: "ignored"}, Options{
		NewMySQL: func(context.Context, Config) (Stores, error) {
			called = true
			return Stores{}, errors.New("should not run")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := stores.Repository.(*repository.InMemoryRepository); !ok {
		t.Fatalf("memory mode returned %T, want the in-memory repository", stores.Repository)
	}
	if stores.Knowledge == nil {
		t.Fatal("memory mode returned no knowledge store, so a grounded turn could not run locally")
	}
	if called {
		t.Fatal("memory mode must not build a MySQL repository")
	}
}

// TestConnectNeverDowngradesMySQLProtects the product promise behind the
// configuration: asking for MySQL and failing to reach it must stop the
// process, not quietly serve volatile storage.
func TestConnectNeverDowngradesMySQL(t *testing.T) {
	cases := []struct {
		name string
		opts Options
		want error
	}{
		{"no connector wired", Options{}, ErrNoMySQLConnector},
		{"connector fails", Options{NewMySQL: func(context.Context, Config) (Stores, error) {
			return Stores{}, errors.New("connection refused")
		}}, nil},
		{"connector returns nothing", Options{NewMySQL: func(context.Context, Config) (Stores, error) {
			return Stores{}, nil
		}}, ErrNoMySQLConnector},
		{"connector returns a core store but no knowledge store", Options{NewMySQL: func(context.Context, Config) (Stores, error) {
			return Stores{Repository: &repository.InMemoryRepository{}}, nil
		}}, ErrNoMySQLConnector},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			stores, err := Connect(context.Background(), Config{Mode: ModeMySQL, MySQLDSN: "d", ConnectTimeout: time.Second}, testCase.opts)
			if err == nil {
				t.Fatal("mysql mode succeeded without a working database")
			}
			if stores.Repository != nil || stores.Knowledge != nil {
				t.Fatalf("mysql mode returned stores after a failure: %#v", stores)
			}
			if testCase.want != nil && !errors.Is(err, testCase.want) {
				t.Fatalf("error = %v, want %v", err, testCase.want)
			}
			if _, isInMemory := stores.Repository.(*repository.InMemoryRepository); isInMemory {
				t.Fatal("mysql mode fell back to the in-memory repository")
			}
		})
	}
}

func TestConnectUsesTheInjectedConnectorForMySQL(t *testing.T) {
	var got Config
	var gotTimeout time.Duration
	fake := &repository.InMemoryRepository{}
	fakeKnowledge := knowledge.NewMemoryStore()
	opts := Options{NewMySQL: func(ctx context.Context, config Config) (Stores, error) {
		got = config
		if deadline, ok := ctx.Deadline(); ok {
			gotTimeout = time.Until(deadline)
		}
		return Stores{Repository: fake, Knowledge: fakeKnowledge}, nil
	}}
	stores, err := Connect(context.Background(), Config{Mode: ModeMySQL, MySQLDSN: "d", ConnectTimeout: 2 * time.Second}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if stores.Repository != repository.Repository(fake) || stores.Knowledge != knowledge.Store(fakeKnowledge) {
		t.Fatalf("stores = %#v, want the connector result", stores)
	}
	if got.MySQLDSN != "d" || got.Mode != ModeMySQL {
		t.Fatalf("connector received %#v", got)
	}
	if gotTimeout <= 0 || gotTimeout > 2*time.Second {
		t.Fatalf("connector context deadline = %v, want within the configured 2s", gotTimeout)
	}
}

func TestConnectValidatesBeforeBuilding(t *testing.T) {
	called := false
	_, err := Connect(context.Background(), Config{Mode: ModeMySQL}, Options{
		NewMySQL: func(context.Context, Config) (Stores, error) { called = true; return Stores{}, nil },
	})
	if !errors.Is(err, ErrMissingDSN) {
		t.Fatalf("error = %v, want ErrMissingDSN", err)
	}
	if called {
		t.Fatal("an invalid configuration reached the connector")
	}
	if _, err := Connect(nil, Config{Mode: ModeMemory}, Options{}); err == nil {
		t.Fatal("a nil context was accepted")
	}
}

func TestConfigSanitizeRemovesCredentialBearingText(t *testing.T) {
	cases := []struct {
		name    string
		config  Config
		in      string
		secret  []string
		keeps   string
		wantNil bool
	}{
		{
			name:   "whole dsn and password",
			config: Config{Mode: ModeMySQL, MySQLDSN: "app-user:app-pass@tcp(db.internal:3306)/learn"},
			in:     "dial failed: open app-user:app-pass@tcp(db.internal:3306)/learn refused",
			secret: []string{"app-user:app-pass@tcp(db.internal:3306)/learn", "app-pass", "app-user"},
			keeps:  "dial failed",
		},
		{
			name:   "password alone",
			config: Config{Mode: ModeMySQL, MySQLDSN: "app-user:app-pass@tcp(db.internal:3306)/learn"},
			in:     "access denied for user using password app-pass",
			secret: []string{"app-pass"},
			keeps:  "access denied",
		},
		{
			name:   "memory mode has nothing to hide",
			config: Config{Mode: ModeMemory},
			in:     "table messages is missing",
			keeps:  "table messages is missing",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := testCase.config.Sanitize(errors.New(testCase.in))
			if got == nil {
				t.Fatal("Sanitize returned nil for a non-nil error")
			}
			for _, secret := range testCase.secret {
				if strings.Contains(got.Error(), secret) {
					t.Fatalf("message still carries %q: %v", secret, got)
				}
			}
			if testCase.keeps != "" && !strings.Contains(got.Error(), testCase.keeps) {
				t.Fatalf("Sanitize removed the diagnosis: %v", got)
			}
		})
	}
	if (Config{Mode: ModeMySQL, MySQLDSN: "d"}).Sanitize(nil) != nil {
		t.Fatal("Sanitize(nil) must stay nil")
	}
	// An unrelated error must come back unchanged, not reworded.
	original := errors.New("context deadline exceeded")
	if (Config{Mode: ModeMemory}).Sanitize(original).Error() != original.Error() {
		t.Fatal("memory-mode Sanitize rewrote an unrelated error")
	}
}
