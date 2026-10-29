package gormmysql

import (
	"context"
	"errors"
	"strings"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/xiaopang456/GopherLearn/internal/knowledge"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

const (
	DefaultConnectTimeout = 5 * time.Second
	DefaultIOTimeout      = 10 * time.Second

	// DefaultConnMaxIdleTime retires a connection the pool has not used for a minute and
	// a half. It exists because a database restart, a firewall reaper or a server-side
	// KILL leaves a dead socket in the free list, and the driver can only notice that
	// before handing the connection to a query on platforms where it can probe the socket
	// - which does not include Windows. Retiring idle connections bounds how long that
	// dead socket can be handed to a request; it is not a zero-downtime guarantee, and a
	// genuinely unreachable database still answers with an error.
	DefaultConnMaxIdleTime = 90 * time.Second

	maxOpenConns    = 25
	maxIdleConns    = 25
	connMaxLifetime = 30 * time.Minute
)

var (
	// ErrMissingDSN reports that MySQL storage was selected without a DSN.
	ErrMissingDSN = errors.New("repository/gormmysql: mysql storage requires GOPHERLEARN_MYSQL_DSN")
	// ErrInvalidDSN reports a DSN that cannot be parsed. The value is never
	// included in the message because it carries credentials.
	ErrInvalidDSN = errors.New("repository/gormmysql: mysql DSN is invalid")
)

// Options overrides the production dial path. Tests substitute Dial so the
// startup failure paths are exercised without opening a socket.
type Options struct {
	Dial           func(ctx context.Context, dsn string) (*gorm.DB, error)
	ConnectTimeout time.Duration
	// ConnMaxIdleTime bounds how long an unused connection stays in the pool. Zero
	// means DefaultConnMaxIdleTime; a negative value disables the limit, which is what
	// database/sql itself means by it, so a deployment can opt out without a flag.
	ConnMaxIdleTime time.Duration
}

// connMaxIdleTime resolves the pool's idle bound. Unset means the documented
// default; a negative value passes through so database/sql reads it as "no idle
// limit", which is how a deployment opts out without a new flag.
func (o Options) connMaxIdleTime() time.Duration {
	if o.ConnMaxIdleTime == 0 {
		return DefaultConnMaxIdleTime
	}
	return o.ConnMaxIdleTime
}

// Stores hands out the repositories that share one database handle.
type Stores struct {
	db *gorm.DB
}

// Repository is the core user/session/message store.
func (s *Stores) Repository() repository.Repository { return NewRepository(s.db) }

// Knowledge is the collection/document/chunk store.
func (s *Stores) Knowledge() knowledge.Store { return NewKnowledgeRepository(s.db) }

// Connect normalizes the DSN, opens MySQL, migrates the schema and returns the
// repositories. Every returned message is scrubbed of the DSN and its password so
// a failed startup cannot leak credentials into a log.
func Connect(ctx context.Context, dsn string, opts Options) (*Stores, error) {
	if ctx == nil {
		return nil, errors.New("repository/gormmysql: context is required")
	}
	connectTimeout := opts.ConnectTimeout
	if connectTimeout <= 0 {
		connectTimeout = DefaultConnectTimeout
	}
	normalized, err := NormalizeDSN(dsn, connectTimeout)
	if err != nil {
		return nil, err
	}

	dial := opts.Dial
	if dial == nil {
		idle := opts.connMaxIdleTime()
		dial = func(ctx context.Context, dsn string) (*gorm.DB, error) {
			return dialMySQL(ctx, dsn, idle)
		}
	}
	runCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	db, err := dial(runCtx, normalized)
	if err != nil {
		return nil, sanitize("open", err, normalized)
	}
	for _, step := range []func(context.Context, *gorm.DB) error{Migrate, MigrateKnowledge} {
		if err := step(runCtx, db); err != nil {
			sanitized := sanitize("migrate", err, normalized)
			closeDB(db)
			return nil, sanitized
		}
	}
	return &Stores{db: db}, nil
}

// NormalizeDSN applies the settings the repository depends on: millisecond
// precision timestamps parsed into time.Time, UTC everywhere, bounded connect
// and read/write timeouts, and a utf8mb4 connection charset.
func NormalizeDSN(dsn string, connectTimeout time.Duration) (string, error) {
	trimmed := strings.TrimSpace(dsn)
	if trimmed == "" {
		return "", ErrMissingDSN
	}
	config, err := driver.ParseDSN(trimmed)
	if err != nil {
		// Deliberately fixed text: the parse error can quote the input value.
		return "", ErrInvalidDSN
	}
	if config.Net == "addr" {
		config.Net = "tcp"
	}
	if config.DBName == "" {
		return "", ErrInvalidDSN
	}
	if connectTimeout <= 0 {
		connectTimeout = DefaultConnectTimeout
	}

	config.ParseTime = true
	config.Loc = time.UTC
	if config.Timeout == 0 {
		config.Timeout = connectTimeout
	}
	if config.ReadTimeout == 0 {
		config.ReadTimeout = DefaultIOTimeout
	}
	if config.WriteTimeout == 0 {
		config.WriteTimeout = DefaultIOTimeout
	}
	if config.MaxAllowedPacket == 0 {
		config.MaxAllowedPacket = 12 << 20
	}
	if config.Params == nil {
		config.Params = map[string]string{}
	}
	config.Params["charset"] = "utf8mb4"
	return config.FormatDSN(), nil
}

// DialMySQL is the production dial path: it opens a pool and verifies the
// connection before the caller is allowed to serve traffic.
func DialMySQL(ctx context.Context, dsn string) (*gorm.DB, error) {
	return dialMySQL(ctx, dsn, DefaultConnMaxIdleTime)
}

func dialMySQL(ctx context.Context, dsn string, connMaxIdleTime time.Duration) (*gorm.DB, error) {
	db, err := gorm.Open(mysql.Open(dsn), gormConfig())
	if err != nil {
		return nil, err
	}
	sqlDB, err := db.DB()
	if err != nil {
		closeDB(db)
		return nil, err
	}
	sqlDB.SetMaxOpenConns(maxOpenConns)
	sqlDB.SetMaxIdleConns(maxIdleConns)
	sqlDB.SetConnMaxLifetime(connMaxLifetime)
	sqlDB.SetConnMaxIdleTime(connMaxIdleTime)
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return db, nil
}

func gormConfig() *gorm.Config {
	return &gorm.Config{
		// Timestamps are stored in UTC so the (created_at, id) ordering matches
		// the identifier clock.
		NowFunc: func() time.Time { return time.Now().UTC() },
		// TranslateError maps duplicate keys onto gorm.ErrDuplicatedKey.
		TranslateError: true,
		// The default logger prints SQL with bound values, which would place
		// message content and password hashes into stdout.
		Logger: gormlogger.Discard,
	}
}

func closeDB(db *gorm.DB) {
	if db == nil {
		return
	}
	if sqlDB, err := db.DB(); err == nil {
		_ = sqlDB.Close()
	}
}

// sanitize rewrites an error so it cannot carry the connection string. The
// driver repeats parts of the DSN in its own messages, and the parsed password
// appears in authentication failures.
func sanitize(operation string, err error, dsn string) error {
	message := err.Error()
	if dsn != "" {
		message = strings.ReplaceAll(message, dsn, "***")
	}
	if config, parseErr := driver.ParseDSN(dsn); parseErr == nil && config.Passwd != "" {
		message = strings.ReplaceAll(message, config.Passwd, "***")
	}
	if config, parseErr := driver.ParseDSN(dsn); parseErr == nil && config.DBName != "" {
		message = strings.ReplaceAll(message, config.DBName, "***")
	}
	return errors.New("repository/gormmysql: " + operation + " failed: " + message)
}
