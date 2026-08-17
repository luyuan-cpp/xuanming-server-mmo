package locker

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"

	"shared/safego"
)

type RedisLocker struct {
	client *redis.Client
}

func NewRedisLocker(client *redis.Client) *RedisLocker {
	return &RedisLocker{client: client}
}

// TryLockWithRetry attempts to acquire a lock with retries.
// Returns the Lock and nil on success, or nil and an error if all retries fail.
func (l *RedisLocker) TryLockWithRetry(ctx context.Context, key string, ttl time.Duration, maxRetries int, retryInterval time.Duration) (*Lock, error) {
	for attempt := 1; attempt <= maxRetries; attempt++ {
		lock, err := l.TryLock(ctx, key, ttl)
		if err == nil && lock.IsLocked() {
			return lock, nil
		}
		if attempt < maxRetries {
			logx.Infof("lock retry %d/%d for key=%s (err=%v)", attempt, maxRetries, key, err)
			time.Sleep(retryInterval)
		}
	}
	return nil, fmt.Errorf("failed to acquire lock %s after %d retries", key, maxRetries)
}

// Lock represents a Redis-based distributed lock
type Lock struct {
	Key    string
	Value  string
	TTL    time.Duration
	locked bool
	client *redis.Client
}

// TryLock tries to acquire the lock
func (l *RedisLocker) TryLock(ctx context.Context, key string, ttl time.Duration) (*Lock, error) {
	lockValue := uuid.NewString()
	ok, err := l.client.SetNX(ctx, key, lockValue, ttl).Result()
	if err != nil {
		return nil, err
	}

	return &Lock{
		Key:    key,
		Value:  lockValue,
		TTL:    ttl,
		locked: ok,
		client: l.client,
	}, nil
}

// IsLocked returns whether the lock was successfully acquired
func (l *Lock) IsLocked() bool {
	return l.locked
}

// Release releases the lock if the current value matches (safe delete)
func (l *Lock) Release(ctx context.Context) (bool, error) {
	const luaScript = `
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("del", KEYS[1])
else
	return 0
end
`
	res, err := l.client.Eval(ctx, luaScript, []string{l.Key}, l.Value).Result()
	if err != nil {
		return false, err
	}

	return res.(int64) == 1, nil
}

// Renew extends the lock's TTL only if we still own it (Lua-atomic check).
// Returns true on successful extension, false if we no longer own the key
// (lost ownership = lock expired and was acquired by someone else, or it was
// released).
func (l *Lock) Renew(ctx context.Context, ttl time.Duration) (bool, error) {
	const luaScript = `
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("pexpire", KEYS[1], ARGV[2])
else
	return 0
end
`
	res, err := l.client.Eval(ctx, luaScript, []string{l.Key}, l.Value, ttl.Milliseconds()).Result()
	if err != nil {
		return false, err
	}
	return res.(int64) == 1, nil
}

// StartHeartbeat starts a background goroutine that renews the lock every
// `interval` until either:
//   - the returned `stop` function is called (typical: chain finished),
//   - renewal fails (we lost the lock),
//   - `onLost` is invoked when ownership is lost so the caller can abort the
//     critical section.
//
// The goroutine exits cleanly under any of these conditions.
//
// Recommended interval: ttl/3. With ttl=30s, interval=10s gives 3 chances to
// recover from a transient Redis blip before losing the lock.
func (l *Lock) StartHeartbeat(interval time.Duration, ttl time.Duration, onLost func(err error)) (stop func()) {
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})

	// safego:心跳 goroutine panic 掉整个进程,会把所有在线玩家一起带走。
	// `defer close(doneCh)` 在 panic 展开时照样执行,所以 stop() 不会挂住。
	safego.Go("login.player_lock_heartbeat", func() {
		defer close(doneCh)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				renewCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				ok, err := l.Renew(renewCtx, ttl)
				cancel()
				if err != nil {
					logx.Errorf("lock heartbeat renew error key=%s: %v", l.Key, err)
					// Transient error — keep trying until next tick or stop.
					continue
				}
				if !ok {
					if onLost != nil {
						onLost(fmt.Errorf("lost lock ownership for key=%s", l.Key))
					}
					return
				}
			}
		}
	})

	return func() {
		select {
		case <-stopCh:
			// already stopped
		default:
			close(stopCh)
		}
		<-doneCh
	}
}
