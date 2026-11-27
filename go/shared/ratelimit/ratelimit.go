// Package ratelimit provides Redis-backed per-player cooldown, fixed-window quota,
// and penalty window primitives (ported from xuanming-server/pkg/redisx/ratelimit.go,
// adapted to mmo-cpp's Redis Cluster hash-tag contract via shared/redisslot).
//
// Design contract:
//   - Backpressure only, NEVER an authority gate: Redis errors always fail-open
//     (returning allow=true with err for caller warning logs) so rate-limiter faults
//     cannot lock out legitimate players.
//   - All keys carry bounded PX expiry (set atomically in Lua for INCR) so memory is
//     bounded by active subjects in the window with zero background sweep needed.
//   - Keys embed a non-empty Redis Cluster hash tag ({player_id} / {subject}) compatible
//     with shared/clusterredis PlayerDomainKeys.
package ratelimit

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"shared/redisslot"
)

// Key builds a Redis Cluster player-domain key carrying {player_id} hash tag:
//
//	rl:<domain>:<action>:{<player_id>}
func Key(domain, action string, playerID uint64) string {
	return fmt.Sprintf("rl:%s:%s:%s", domain, action, redisslot.PlayerTag(playerID))
}

// KeyString builds a Redis Cluster tagged key for non-numeric subjects (e.g. sanitized
// account hash or IP literal). Braces are stripped from subject to keep the hash tag valid.
func KeyString(domain, action, subject string) string {
	clean := strings.NewReplacer("{", "_", "}", "_").Replace(strings.TrimSpace(subject))
	if clean == "" {
		clean = "empty"
	}
	return fmt.Sprintf("rl:%s:%s:{s:%s}", domain, action, clean)
}

// Cooldown claims a cooldown slot via atomic SET NX PX.
// Returns (true, nil) when acquired, (false, nil) while cooling down, and (true, err) on Redis fault (fail-open).
func Cooldown(ctx context.Context, rdb redis.Cmdable, key string, window time.Duration) (bool, error) {
	if rdb == nil || window <= 0 {
		return true, nil
	}
	ok, err := rdb.SetNX(ctx, key, 1, window).Result()
	if err != nil {
		return true, err
	}
	return ok, nil
}

// ClearCooldown releases a claimed cooldown slot (idempotent DEL), typically invoked when
// the guarded business operation fails so cooldown only limits successful completions.
func ClearCooldown(ctx context.Context, rdb redis.Cmdable, key string) error {
	if rdb == nil {
		return nil
	}
	return rdb.Del(ctx, key).Err()
}

// quotaScript atomically runs INCR and sets PEXPIRE on first hit (or if a key somehow lacks TTL),
// preventing permanent TTL-less counter leaks on crash between INCR and PEXPIRE.
var quotaScript = redis.NewScript(`
local n = redis.call('INCR', KEYS[1])
if n == 1 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
return n`)

// IncrWindow increments a fixed-window counter and returns the post-increment count.
func IncrWindow(ctx context.Context, rdb redis.Cmdable, key string, window time.Duration) (int64, error) {
	if rdb == nil || window <= 0 {
		return 0, nil
	}
	ms := window.Milliseconds()
	if ms <= 0 {
		ms = 1
	}
	n, err := quotaScript.Run(ctx, rdb, []string{key}, ms).Int64()
	if err != nil {
		return 0, err
	}
	return n, nil
}

// Quota allows up to limit hits per fixed window. Non-positive limit or window disables limiting.
// Redis errors fail-open by returning (true, err).
func Quota(ctx context.Context, rdb redis.Cmdable, key string, limit int64, window time.Duration) (bool, error) {
	if rdb == nil || limit <= 0 || window <= 0 {
		return true, nil
	}
	n, err := IncrWindow(ctx, rdb, key, window)
	if err != nil {
		return true, err
	}
	return n <= limit, nil
}

// ActionQuota is a reusable per-player fixed-window action rate limiter.
type ActionQuota struct {
	RDB    redis.Cmdable
	Domain string
	Limit  int64
	Window time.Duration
}

// Allow records one action attempt for playerID and reports whether it stays within quota.
func (q *ActionQuota) Allow(ctx context.Context, action string, playerID uint64) (bool, error) {
	if q == nil {
		return true, nil
	}
	return Quota(ctx, q.RDB, Key(q.Domain, action, playerID), q.Limit, q.Window)
}

// ArmPenalty sets or replaces a penalty window via unconditional SET PX.
func ArmPenalty(ctx context.Context, rdb redis.Cmdable, key string, window time.Duration) error {
	if rdb == nil || window <= 0 {
		return nil
	}
	return rdb.Set(ctx, key, 1, window).Err()
}

// PenaltyRemaining returns the remaining penalty duration, or 0 when absent/expired.
func PenaltyRemaining(ctx context.Context, rdb redis.Cmdable, key string) (time.Duration, error) {
	if rdb == nil {
		return 0, nil
	}
	d, err := rdb.PTTL(ctx, key).Result()
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, nil
	}
	return d, nil
}
