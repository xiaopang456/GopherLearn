//go:build v2_real_gates || hybrid_touch_mysql

package hybrid

import (
	"GopherAI/entry"
	"context"
	"errors"
	"io"
	"regexp"
)

// This input seam exists only in the tagged test binary. Production Load and
// configuration validation are unchanged. Input ownership includes timeout close.
func applyI0PasswordInput(ctx context.Context, s *entry.Settings, input io.ReadCloser) error {
	failure := errors.New("invalid isolated test password input")
	if s == nil || s.Config == nil || s.Config.MysqlPassword != "" || input == nil || ctx.Err() != nil {
		return failure
	}
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		data, err := io.ReadAll(io.LimitReader(input, 66))
		done <- result{data, err}
	}()
	select {
	case <-ctx.Done():
		_ = input.Close()
		return failure
	case r := <-done:
		if r.err != nil || ctx.Err() != nil || !regexp.MustCompile(`^[0-9a-f]{64}\n$`).Match(r.data) {
			return failure
		}
		s.Config.MysqlPassword = string(r.data[:64])
		return nil
	}
}
