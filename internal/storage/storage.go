// Package storage turns environment configuration into the repository the
// services talk to. It never falls back to the in-memory store when a database
// was requested: a production process that cannot reach MySQL must fail to
// start rather than silently serve volatile storage.
package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/knowledge"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

// Mode selects which repository.Repository implementation is wired in.
type Mode string

const (
	// ModeMemory keeps conversations in process memory. It is the default for
	// local development and unit tests, and everything is lost on restart.
	ModeMemory Mode = "memory"
	// ModeMySQL persists through GORM.
	ModeMySQL Mode = "mysql"
)

// Environment variable names read by ConfigFromEnvironment.
const (
	EnvStorage             = "GOPHERLEARN_STORAGE"
	EnvMySQLDSN            = "GOPHERLEARN_MYSQL_DSN"
	EnvMySQLConnectTimeout = "GOPHERLEARN_MYSQL_CONNECT_TIMEOUT"
)

var (
	// ErrUnsupportedMode reports a GOPHERLEARN_STORAGE value other than memory
	// or mysql.
	ErrUnsupportedMode = errors.New("storage: unsupported GOPHERLEARN_STORAGE value")
	// ErrMissingDSN reports mysql mode without a connection string.
	ErrMissingDSN = errors.New("storage: GOPHERLEARN_MYSQL_DSN is required when GOPHERLEARN_STORAGE=mysql")
	// ErrNoMySQLConnector reports a wiring mistake: mysql mode was selected but
	// the process cannot build a MySQL repository.
	ErrNoMySQLConnector = errors.New("storage: no mysql connector configured")
	// ErrInvalidTimeout reports an unparseable connect timeout.
	ErrInvalidTimeout = errors.New("storage: GOPHERLEARN_MYSQL_CONNECT_TIMEOUT must be a Go duration")
)

// DefaultConnectTimeout bounds one MySQL connection attempt at startup.
const DefaultConnectTimeout = 5 * time.Second

// Config is the resolved storage configuration.
type Config struct {
	Mode           Mode
	MySQLDSN       string
	ConnectTimeout time.Duration
}

// ConfigFromEnvironment reads storage configuration. An unset or blank
// GOPHERLEARN_STORAGE means memory, which keeps local development working
// without a database; the resolved mode is still reported at startup so nobody
// mistakes a memory-backed process for a persistent one.
func ConfigFromEnvironment(lookup func(string) string) (Config, error) {
	if lookup == nil {
		lookup = func(string) string { return "" }
	}
	config := Config{Mode: ModeMemory, ConnectTimeout: DefaultConnectTimeout}

	switch mode := Mode(strings.ToLower(strings.TrimSpace(lookup(EnvStorage)))); mode {
	case "", ModeMemory:
		config.Mode = ModeMemory
	case ModeMySQL:
		config.Mode = ModeMySQL
	default:
		return Config{}, fmt.Errorf("%w: %q", ErrUnsupportedMode, string(mode))
	}

	config.MySQLDSN = strings.TrimSpace(lookup(EnvMySQLDSN))
	if raw := strings.TrimSpace(lookup(EnvMySQLConnectTimeout)); raw != "" {
		timeout, err := time.ParseDuration(raw)
		if err != nil || timeout <= 0 {
			return Config{}, ErrInvalidTimeout
		}
		config.ConnectTimeout = timeout
	}
	if config.Mode == ModeMySQL && config.MySQLDSN == "" {
		return Config{}, ErrMissingDSN
	}
	return config, nil
}

// Validate re-checks the invariants Connect depends on.
func (c Config) Validate() error {
	switch c.Mode {
	case ModeMemory:
		return nil
	case ModeMySQL:
		if strings.TrimSpace(c.MySQLDSN) == "" {
			return ErrMissingDSN
		}
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrUnsupportedMode, string(c.Mode))
	}
}

// Stores are the repositories a process runs on. Knowledge travels with the
// core store so a deployment cannot pair a persistent transcript with a
// volatile knowledge base.
type Stores struct {
	Repository repository.Repository
	Knowledge  knowledge.Store
}

// Options injects the MySQL construction path. Connect falls back to nil, so a
// binary that imports the GORM driver supplies it here.
type Options struct {
	NewMySQL func(ctx context.Context, config Config) (Stores, error)
}

// Sanitize strips this configuration's connection string, its credentials and
// its password out of an error message. Callers log through it so a failure
// from any lower layer cannot print a DSN: the repository scrubs its own
// messages, and this is the last net before text reaches stdout.
func (c Config) Sanitize(err error) error {
	if err == nil {
		return nil
	}
	dsn := strings.TrimSpace(c.MySQLDSN)
	message := err.Error()
	if dsn == "" {
		return err
	}
	redact := []string{dsn}
	// The credential prefix of "user:password@tcp(host)/db" carries the
	// password; the bare password is redacted on its own as well.
	if at := strings.LastIndex(dsn, "@"); at > 0 {
		credentials := dsn[:at]
		redact = append(redact, credentials)
		if colon := strings.LastIndex(credentials, ":"); colon >= 0 {
			if password := credentials[colon+1:]; password != "" {
				redact = append(redact, password)
			}
		}
	}
	for _, secret := range redact {
		message = strings.ReplaceAll(message, secret, "***")
	}
	return errors.New(message)
}

// Connect returns the repository selected by config. The context and timeout
// are handed to the connector so a slow database fails the startup instead of
// blocking it forever.
func Connect(ctx context.Context, config Config, opts Options) (Stores, error) {
	if ctx == nil {
		return Stores{}, errors.New("storage: context is required")
	}
	if err := config.Validate(); err != nil {
		return Stores{}, err
	}
	if config.Mode == ModeMemory {
		return Stores{
			Repository: repository.NewInMemoryRepository(),
			Knowledge:  knowledge.NewMemoryStore(),
		}, nil
	}
	if opts.NewMySQL == nil {
		return Stores{}, ErrNoMySQLConnector
	}
	// The startup budget belongs to this decision, so a connector cannot block
	// the process forever by ignoring the timeout it was configured with.
	timeout := config.ConnectTimeout
	if timeout <= 0 {
		timeout = DefaultConnectTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stores, err := opts.NewMySQL(runCtx, config)
	if err != nil {
		return Stores{}, err
	}
	if stores.Repository == nil || stores.Knowledge == nil {
		return Stores{}, ErrNoMySQLConnector
	}
	return stores, nil
}
