package redis

import (
	"context"
	"fmt"
	"time"

	rediscli "github.com/redis/go-redis/v9"
)

var fixedWindowRateLimitScript = rediscli.NewScript(`
local current = redis.call("INCR", KEYS[1])
if current == 1 then
  redis.call("PEXPIRE", KEYS[1], ARGV[1])
end
return current
`)

// AllowFixedWindow provides a small distributed quota primitive for endpoints
// whose upstream calls have monetary or availability cost. Keys must contain
// only bounded server-derived identifiers, never user text or credentials.
func AllowFixedWindow(ctx context.Context, key string, limit int64, window time.Duration) (bool, error) {
	if Rdb == nil {
		return false, fmt.Errorf("redis client is not initialized")
	}
	if ctx == nil || key == "" || limit <= 0 || window <= 0 {
		return false, fmt.Errorf("invalid rate limit configuration")
	}
	current, err := fixedWindowRateLimitScript.Run(ctx, Rdb, []string{key}, window.Milliseconds()).Int64()
	if err != nil {
		return false, err
	}
	return current <= limit, nil
}
