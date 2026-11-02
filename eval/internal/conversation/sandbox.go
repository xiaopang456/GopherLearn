package conversation

import (
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
	dsn := fmt.Sprintf("root:%s@tcp(%s)/eval_sandbox?charset=utf8mb4&parseTime=True&loc=UTC", s.password, s.addr)
	for {
		db, openErr := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), DisableAutomaticPing: true})
		if openErr == nil {
			sqlDB, dbErr := db.DB()
			if dbErr == nil && sqlDB.PingContext(ctx) == nil {
				s.db = db
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
