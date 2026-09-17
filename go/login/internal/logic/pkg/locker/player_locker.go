package locker

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"

	"shared/leader"
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

	// acquiredAt 是 SETNX **发出**的时刻:服务端写入不早于它,key 最早也在
	// acquiredAt+TTL 才过期。心跳自我降级从这里起算,而不是从 StartHeartbeat
	// 被调用时起算 —— 调用方在拿锁和起心跳之间还会做 Redis 往返。
	acquiredAt time.Time
}

// TryLock tries to acquire the lock
func (l *RedisLocker) TryLock(ctx context.Context, key string, ttl time.Duration) (*Lock, error) {
	lockValue := uuid.NewString()
	sentAt := time.Now()
	ok, err := l.client.SetNX(ctx, key, lockValue, ttl).Result()
	if err != nil {
		return nil, err
	}

	return &Lock{
		Key:        key,
		Value:      lockValue,
		TTL:        ttl,
		locked:     ok,
		client:     l.client,
		acquiredAt: sentAt,
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

// StartHeartbeat 起一个续期 goroutine,每 interval 续一次 ttl,推荐 interval = ttl/3。
// 以下任一情况 goroutine 退出:
//   - 调用返回的 stop()(典型:临界区正常结束);
//   - 续期成功但读到 0:属主已换人,立刻 onLost;
//   - 续期持续报错:距上次续期成功太久就自我降级,onLost。与 shared/leader 同一
//     模式 —— 不能只把错误当瞬时抖动无限重试:Redis 单边不可达时续期永远读不到 0,
//     服务端 key 照常过期、被别的副本 / 登录链抢走,就是双属主。
//
// onLost 至多调用一次,在心跳 goroutine 里同步执行;stop() 会等它返回,所以 onLost
// 里不能同步调 stop()(互相等待会死锁)。stop() 返回后 onLost 不会再被调用。
// stop() 可重复、可并发调用。
func (l *Lock) StartHeartbeat(interval time.Duration, ttl time.Duration, onLost func(err error)) (stop func()) {
	// 按 interval = ttl/3 的约定互相补齐。ttl<=0 不能沿用旧行为:Renew 会发
	// PEXPIRE key 0,Redis 直接删 key,别人当场就能抢到锁。
	if ttl <= 0 && interval > 0 {
		ttl = interval * 3
	}
	if interval <= 0 && ttl > 0 {
		interval = ttl / 3
	}

	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	var stopOnce sync.Once
	stop = func() {
		stopOnce.Do(func() { close(stopCh) })
		<-doneCh
	}

	if interval <= 0 || ttl <= 0 {
		// 两者都非法(典型:PlayerLockTTL 漏配成 0):续期会无间隔地连发,也算不出降级
		// 时点,只能不起心跳。不调 onLost:ttl=0 时 TryLock 走 SETNX,拿到的 key
		// 永不过期,服务端不会过期易主,没有双属主可防;硬降级只会让每次进游戏都
		// 失败、dispatcher 抢锁空转。
		logx.Errorf("lock heartbeat not started key=%s: invalid interval=%s ttl=%s", l.Key, interval, ttl)
		close(doneCh)
		return stop
	}

	// 降级门槛(都是为了赶在服务端 key 过期前退位):
	//   - lastOK 记续期请求发出的时刻:PEXPIRE 不早于它执行,key 最早在 lastOK+ttl 过期。
	//     按回包时刻记会晚一个成功往返,出错回包又比成功快(连接被拒)时就漏过第 2 个出错拍。
	//   - renewTimeout 只是续期 ctx 的超时,封不住一次调用:客户端没开 ContextTimeoutEnabled
	//     时 socket 读写不认 ctx 截止时间,另受读写超时之和约束(生产默认 3s+3s);写之前的
	//     连接池排队 / 重试退避 / 拨号又先吃掉最多 renewTimeout。所以 maxCall 取两段之和,
	//     见 leader.CallBudget(GoRedisMaxCallDuration 返回 0 表示认 ctx,即 renewTimeout);
	//     新建连接时的 HELLO / AUTH 握手读写仍不在其内。
	//   - 门槛公式与三条约束见 leader.FenceAfter:按 interval=ttl/3 且 ttl/3 > maxCall(留余量),
	//     失联后第 1 个出错拍不降级、第 2 个出错拍必定降级且赶在 key 过期前;满足不了时退化为
	//     任一续期出错即降级。生产客户端默认选项 maxCall = 2s+6s = 8s:PlayerLockTTL 120s → 80s,
	//     DispatcherLockTTL 30s → 20s(降级最晚 28s 完成)。
	renewTimeout := min(2*time.Second, ttl/8)
	maxCall := leader.CallBudget(renewTimeout, leader.GoRedisMaxCallDuration(l.client.Options()))
	fenceAfter, guaranteed := leader.FenceAfter(ttl, interval, maxCall)
	if !guaranteed {
		logx.Errorf("lock heartbeat key=%s ttl=%s interval=%s: ttl too short for redis client worst-case call %s, "+
			"cannot guarantee self-fencing before lock expiry; fencing on any renew error; set ttl > 3x%s with margin",
			l.Key, ttl, interval, maxCall, maxCall)
	}

	// safego:心跳 goroutine panic 掉整个进程,会把所有在线玩家一起带走。
	// `defer close(doneCh)` 在 panic 展开时照样执行,所以 stop() 不会挂住。
	safego.Go("login.player_lock_heartbeat", func() {
		defer close(doneCh)
		lastOK := l.acquiredAt
		if lastOK.IsZero() {
			lastOK = time.Now()
		}
		// 按 lastOK 排拍,不按本 goroutine 起动时刻起 ticker:调用方在拿锁和起心跳之间还有 Redis
		// 往返,错相位的节拍会让第 2 个出错拍拖过 key 过期(见 leader.FenceAfter 前提)。
		next := lastOK.Add(interval)
		timer := time.NewTimer(time.Until(next))
		defer timer.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-timer.C:
				// select 在 stopCh 与定时器同时就绪时随机选;先让 stop 生效,
				// 避免临界区已结束还续期、甚至晚到一次 onLost。
				select {
				case <-stopCh:
					return
				default:
				}
				sentAt := time.Now()
				renewCtx, cancel := context.WithTimeout(context.Background(), renewTimeout)
				ok, err := l.Renew(renewCtx, ttl)
				cancel()
				// 续期在途时 stop 可能已被调用:调用方已开始收尾,此后不得再 onLost。
				select {
				case <-stopCh:
					return
				default:
				}
				if err != nil {
					logx.Errorf("lock heartbeat renew error key=%s: %v", l.Key, err)
					since := time.Since(lastOK)
					if since < fenceAfter {
						next = next.Add(interval)
						timer.Reset(time.Until(next))
						continue
					}
					if onLost != nil {
						onLost(fmt.Errorf("lock key=%s: no successful renew for %s (>= %s, ttl %s), self-fencing: %w",
							l.Key, since, fenceAfter, ttl, err))
					}
					return
				}
				if !ok {
					if onLost != nil {
						onLost(fmt.Errorf("lost lock ownership for key=%s", l.Key))
					}
					return
				}
				lastOK = sentAt
				next = sentAt.Add(interval)
				timer.Reset(time.Until(next))
			}
		}
	})

	return stop
}
