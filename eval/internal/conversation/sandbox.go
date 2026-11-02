package conversation

import (
	"GopherAI/model"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var containerIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

const sandboxLabel = "org.gopherlearn.eval.nonce"

// Sandbox is a newly created MySQL container, not an externally supplied DSN.
// Its identity is checked before every mutation and before cleanup.
type Sandbox struct {
	id, nonce, password, addr string
	db                        *gorm.DB
}

// Connection describes only the disposable database created by this process.
// Password must not be included in manifests, logs, or test failure messages.
type Connection struct {
	Host     string
	Port     string
	Database string
	User     string
	Password string `json:"-"`
}

func (c Connection) String() string {
	return fmt.Sprintf("%s:%s/%s (user %s, password redacted)", c.Host, c.Port, c.Database, c.User)
}
func (c Connection) GoString() string { return c.String() }

// ConnectionInfo verifies both container and database identity before exposing
// credentials to the local product service. A caller must not persist them.
func (s *Sandbox) ConnectionInfo(ctx context.Context) (Connection, error) {
	var result Connection
	if err := s.verify(ctx); err != nil {
		return result, err
	}
	if s.db == nil {
		return result, fmt.Errorf("sandbox database unavailable")
	}
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error { return s.checkMarker(tx) }); err != nil {
		return result, err
	}
	host, port, err := net.SplitHostPort(s.addr)
	if err != nil || host != "127.0.0.1" {
		return result, fmt.Errorf("invalid sandbox endpoint")
	}
	return Connection{Host: host, Port: port, Database: "eval_sandbox", User: "root", Password: s.password}, nil
}

func docker(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		return "", fmt.Errorf("docker command failed: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// NewSandbox only creates a new container and does not connect to existing
// databases. The image must already exist locally; it is never pulled.
func NewSandbox(ctx context.Context) (*Sandbox, error) {
	nonce, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	password, err := randomHex(24)
	if err != nil {
		return nil, err
	}
	s := &Sandbox{nonce: nonce, password: password}
	id, err := docker(ctx, "run", "-d", "--pull=never", "--name", "gl-eval-"+nonce, "--label", sandboxLabel+"="+nonce, "-p", "127.0.0.1::3306", "-e", "MYSQL_ROOT_PASSWORD="+password, "-e", "MYSQL_DATABASE=eval_sandbox", "mysql:8.4")
	if err != nil {
		return nil, err
	}
	if !containerIDPattern.MatchString(id) {
		return nil, fmt.Errorf("invalid container id")
	}
	s.id = id
	fail := func(err error) (*Sandbox, error) {
		closeCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if closeErr := s.Close(closeCtx); closeErr != nil {
			return nil, fmt.Errorf("%v; cleanup: %w", err, closeErr)
		}
		return nil, err
	}
	if err = s.verify(ctx); err != nil {
		return fail(err)
	}
	port, err := docker(ctx, "port", s.id, "3306/tcp")
	if err != nil {
		return fail(err)
	}
	host, p, err := net.SplitHostPort(strings.TrimSpace(port))
	if err != nil || host != "127.0.0.1" || p == "" {
		return fail(fmt.Errorf("sandbox port is not loopback-only"))
	}
	s.addr = net.JoinHostPort(host, p)
	// The image first runs a temporary socket-only server during bootstrap.
	// Wait for TCP inside the owned container before opening the host port.
	for {
		_, readyErr := docker(ctx, "exec", "-e", "MYSQL_PWD="+s.password, s.id, "mysqladmin", "--protocol=tcp", "-h127.0.0.1", "-uroot", "ping", "--silent")
		if readyErr == nil {
			break
		}
		select {
		case <-ctx.Done():
			return fail(fmt.Errorf("sandbox readiness timed out: %w", ctx.Err()))
		case <-time.After(250 * time.Millisecond):
		}
	}
	dsn := fmt.Sprintf("root:%s@tcp(%s)/eval_sandbox?charset=utf8mb4&parseTime=True&loc=UTC", s.password, s.addr)
	for {
		db, openErr := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), DisableAutomaticPing: true})
		if openErr == nil {
			sqlDB, dbErr := db.DB()
			if dbErr == nil && sqlDB.PingContext(ctx) == nil {
				s.db = db
				if err := s.createMarker(ctx); err != nil {
					return fail(err)
				}
				if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error { return s.checkMarker(tx) }); err != nil {
					return fail(err)
				}
				return s, nil
			}
			if dbErr == nil {
				sqlDB.Close()
			}
		}
		select {
		case <-ctx.Done():
			return fail(fmt.Errorf("sandbox did not become ready: %w", ctx.Err()))
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// The marker is created through docker exec inside the verified container,
// rather than via the host port. A redirected host port cannot be mistaken for
// this instance unless it also holds the newly generated unpredictable nonce.
func (s *Sandbox) createMarker(ctx context.Context) error {
	if err := s.verify(ctx); err != nil {
		return err
	}
	query := "CREATE TABLE eval_instance_guard (nonce VARCHAR(32) NOT NULL); INSERT INTO eval_instance_guard (nonce) VALUES ('" + s.nonce + "')"
	_, err := docker(ctx, "exec", "-e", "MYSQL_PWD="+s.password, s.id, "mysql", "-uroot", "eval_sandbox", "-e", query)
	if err != nil {
		return fmt.Errorf("create sandbox marker: %w", err)
	}
	return nil
}
func (s *Sandbox) checkMarker(tx *gorm.DB) error {
	var values []string
	if err := tx.Table("eval_instance_guard").Pluck("nonce", &values).Error; err != nil {
		return fmt.Errorf("read sandbox marker: %w", err)
	}
	if len(values) != 1 || values[0] != s.nonce {
		return fmt.Errorf("sandbox database marker mismatch")
	}
	return nil
}

// Restore checks both the Docker identity and the database marker before any
// writes. The marker check and restore use the same transaction/connection.
func (s *Sandbox) Restore(ctx context.Context, snapshot Snapshot) error {
	if err := s.verify(ctx); err != nil {
		return err
	}
	if s.db == nil {
		return fmt.Errorf("sandbox database unavailable")
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := s.checkMarker(tx); err != nil {
			return err
		}
		return restoreIntoEmpty(ctx, tx, snapshot)
	})
}

// Capture and ObserveJobs enforce the same instance checks as Restore.
func (s *Sandbox) Capture(ctx context.Context, userID int64, sessionID string) (Snapshot, error) {
	var snapshot Snapshot
	if err := s.verify(ctx); err != nil {
		return snapshot, err
	}
	if s.db == nil {
		return snapshot, fmt.Errorf("sandbox database unavailable")
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := s.checkMarker(tx); err != nil {
			return err
		}
		var err error
		snapshot, err = Capture(ctx, tx, userID, sessionID)
		return err
	})
	if err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}
func (s *Sandbox) ObserveJobs(ctx context.Context) ([]JobState, map[string]JobCounts, error) {
	if err := s.verify(ctx); err != nil {
		return nil, nil, err
	}
	if s.db == nil {
		return nil, nil, fmt.Errorf("sandbox database unavailable")
	}
	var jobs []JobState
	var counts map[string]JobCounts
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := s.checkMarker(tx); err != nil {
			return err
		}
		var err error
		jobs, counts, err = ObserveJobs(ctx, tx)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return jobs, counts, nil
}

// InitSynthetic creates the small schema for the offline demo only. It does
// not replace the full product migration required for HTTP evaluation.
func (s *Sandbox) InitSynthetic(ctx context.Context) error {
	if err := s.verify(ctx); err != nil {
		return err
	}
	if s.db == nil {
		return fmt.Errorf("sandbox database unavailable")
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := s.checkMarker(tx); err != nil {
			return err
		}
		for _, table := range []string{"users", "sessions", "messages", "session_summaries"} {
			if tx.Migrator().HasTable(table) {
				return fmt.Errorf("synthetic schema already exists")
			}
		}
		if err := tx.AutoMigrate(&model.User{}, &model.Session{}, &model.Message{}, &model.SessionSummary{}); err != nil {
			return err
		}
		if err := tx.Create(&model.User{ID: 7, Username: "synthetic-learner"}).Error; err != nil {
			return err
		}
		return tx.Create(&model.Session{ID: "synthetic-evaluation", UserID: 7, UserName: "synthetic-learner", Mode: model.SessionModeAgent, Title: "Synthetic learning conversation"}).Error
	})
}
func (s *Sandbox) verify(ctx context.Context) error {
	if err := s.verifyIdentity(ctx); err != nil {
		return err
	}
	value, err := docker(ctx, "inspect", "--format", `{{.State.Running}}`, s.id)
	if err != nil || value != "true" {
		return fmt.Errorf("sandbox is not running")
	}
	if s.addr != "" {
		port, err := docker(ctx, "port", s.id, "3306/tcp")
		if err != nil || strings.TrimSpace(port) != s.addr {
			return fmt.Errorf("sandbox port changed")
		}
	}
	return nil
}
func (s *Sandbox) verifyIdentity(ctx context.Context) error {
	if s == nil || !containerIDPattern.MatchString(s.id) || len(s.nonce) != 32 {
		return fmt.Errorf("invalid sandbox identity")
	}
	value, err := docker(ctx, "inspect", "--format", `{{index .Config.Labels "org.gopherlearn.eval.nonce"}}`, s.id)
	if err != nil || value != s.nonce {
		return fmt.Errorf("sandbox identity mismatch")
	}
	return nil
}

// Close removes only the exact container created by this object after checking
// its random label. The -v flag only removes that container's anonymous volume.
func (s *Sandbox) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if err := s.verifyIdentity(ctx); err != nil {
		return err
	}
	if s.db != nil {
		db, err := s.db.DB()
		if err != nil {
			return err
		}
		if err = db.Close(); err != nil {
			return err
		}
		s.db = nil
	}
	_, err := docker(ctx, "rm", "-f", "-v", s.id)
	if err != nil {
		return err
	}
	s.id = ""
	return nil
}
