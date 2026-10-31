package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"shared/playerredis"
	"shared/redisslot"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

func testPasswordQuota(t *testing.T, limit int) (*RedisPasswordFailureQuota, func(time.Duration)) {
	t.Helper()
	server := miniredis.RunT(t)
	now := time.Now()
	server.SetTime(now)
	rdb := redis.NewClient(&redis.Options{Addr: server.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	quota, err := NewRedisPasswordFailureQuota(rdb, limit, 15*time.Minute, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	quota.now = func() time.Time { return now }
	return quota, func(duration time.Duration) {
		now = now.Add(duration)
		server.FastForward(duration)
		server.SetTime(now)
	}
}

func mustPasswordBegin(t *testing.T, q *RedisPasswordFailureQuota, identity string) PasswordAttempt {
	t.Helper()
	attempt, err := q.Begin(context.Background(), KnownPasswordAccount, identity)
	if err != nil {
		t.Fatal(err)
	}
	return attempt
}

func mustPasswordFinish(t *testing.T, q *RedisPasswordFailureQuota, attempt PasswordAttempt, outcome PasswordAttemptOutcome) {
	t.Helper()
	if err := q.Finish(context.Background(), attempt, outcome); err != nil {
		t.Fatal(err)
	}
}

func TestPasswordQuotaFixedWindowsLockRecoveryAndSuccess(t *testing.T) {
	q, advance := testPasswordQuota(t, 2)
	first := mustPasswordBegin(t, q, "canonical")
	mustPasswordFinish(t, q, first, PasswordMismatch)
	advance(14 * time.Minute)
	second := mustPasswordBegin(t, q, "canonical")
	mustPasswordFinish(t, q, second, PasswordMismatch)
	if _, err := q.Begin(context.Background(), KnownPasswordAccount, "canonical"); !errors.Is(err, ErrPasswordQuotaDenied) {
		t.Fatalf("锁中请求=%v", err)
	}
	advance(4 * time.Minute)
	if _, err := q.Begin(context.Background(), KnownPasswordAccount, "canonical"); !errors.Is(err, ErrPasswordQuotaDenied) {
		t.Fatalf("拒绝不得提前解锁=%v", err)
	}
	advance(time.Minute)
	third := mustPasswordBegin(t, q, "canonical")
	mustPasswordFinish(t, q, third, PasswordMismatch)
	// 锁到期重新累积，上一轮阈值不能被一个失败无限续锁。
	fourth := mustPasswordBegin(t, q, "canonical")
	mustPasswordFinish(t, q, fourth, PasswordSuccess)
	fifth := mustPasswordBegin(t, q, "canonical")
	mustPasswordFinish(t, q, fifth, PasswordMismatch)
	sixth := mustPasswordBegin(t, q, "canonical")
	mustPasswordFinish(t, q, sixth, PasswordAbort)
	advance(15 * time.Minute)
	seventh := mustPasswordBegin(t, q, "canonical")
	mustPasswordFinish(t, q, seventh, PasswordMismatch)
	mustPasswordFinish(t, q, mustPasswordBegin(t, q, "canonical"), PasswordSuccess)
}

func TestPasswordQuotaDoesNotExtendFailureWindow(t *testing.T) {
	q, advance := testPasswordQuota(t, 3)
	mustPasswordFinish(t, q, mustPasswordBegin(t, q, "fixed"), PasswordMismatch)
	advance(14 * time.Minute)
	mustPasswordFinish(t, q, mustPasswordBegin(t, q, "fixed"), PasswordMismatch)
	advance(time.Minute)
	mustPasswordFinish(t, q, mustPasswordBegin(t, q, "fixed"), PasswordMismatch)
	mustPasswordFinish(t, q, mustPasswordBegin(t, q, "fixed"), PasswordMismatch)
	// 如果第二个失败续窗，这里已被锁。
	mustPasswordFinish(t, q, mustPasswordBegin(t, q, "fixed"), PasswordAbort)
}

func TestPasswordQuotaSingleInflightFencesExpiredAndReplacedTickets(t *testing.T) {
	q, advance := testPasswordQuota(t, 2)
	first := mustPasswordBegin(t, q, "one")
	if _, err := q.Begin(context.Background(), KnownPasswordAccount, "one"); !errors.Is(err, ErrPasswordQuotaDenied) {
		t.Fatalf("同账号并行=%v", err)
	}
	other := mustPasswordBegin(t, q, "other")
	mustPasswordFinish(t, q, other, PasswordSuccess)
	advance(PasswordAttemptLease)
	for _, outcome := range []PasswordAttemptOutcome{PasswordSuccess, PasswordMismatch, PasswordAbort} {
		if err := q.Finish(context.Background(), first, outcome); !errors.Is(err, ErrPasswordQuotaObsolete) {
			t.Fatalf("过期票%s=%v", outcome, err)
		}
	}
	current := mustPasswordBegin(t, q, "one")
	// 绕过客户端保守期限，独立证明 Redis 端 token fencing。
	first.deadline = q.now().Add(time.Minute)
	if err := q.Finish(context.Background(), first, PasswordSuccess); !errors.Is(err, ErrPasswordQuotaObsolete) {
		t.Fatalf("被取代票=%v", err)
	}
	if _, err := q.Begin(context.Background(), KnownPasswordAccount, "one"); !errors.Is(err, ErrPasswordQuotaDenied) {
		t.Fatalf("旧票不得释放新票=%v", err)
	}
	mustPasswordFinish(t, q, current, PasswordAbort)
}

func TestPasswordQuotaCompletedReceiptIsBoundedAndOutcomeImmutable(t *testing.T) {
	q, advance := testPasswordQuota(t, 2)
	first := mustPasswordBegin(t, q, "receipt")
	mustPasswordFinish(t, q, first, PasswordMismatch)
	mustPasswordFinish(t, q, first, PasswordMismatch)
	if err := q.Finish(context.Background(), first, PasswordSuccess); !errors.Is(err, ErrPasswordQuotaConflict) {
		t.Fatalf("换outcome=%v", err)
	}
	next := mustPasswordBegin(t, q, "receipt")
	if err := q.Finish(context.Background(), first, PasswordMismatch); !errors.Is(err, ErrPasswordQuotaObsolete) {
		t.Fatalf("新Begin清旧回执=%v", err)
	}
	mustPasswordFinish(t, q, next, PasswordMismatch)
	if _, err := q.Begin(context.Background(), KnownPasswordAccount, "receipt"); !errors.Is(err, ErrPasswordQuotaDenied) {
		t.Fatalf("重复不得多计/漏计=%v", err)
	}
	advance(PasswordAttemptLease)
	if err := q.Finish(context.Background(), next, PasswordMismatch); !errors.Is(err, ErrPasswordQuotaObsolete) {
		t.Fatalf("回执过期=%v", err)
	}
}

func TestPasswordQuotaIdentityDomainsAndOriginalBytes(t *testing.T) {
	q, _ := testPasswordQuota(t, 1)
	a := mustPasswordBegin(t, q, "Álice")
	digest := sha256.Sum256([]byte("Álice"))
	if a.key != "login:password:quota:v1:{known:"+hex.EncodeToString(digest[:])+"}" {
		t.Fatal("键必须使用canonical原字节完整SHA256")
	}
	if !redisslot.HasHashTag(a.key) {
		t.Fatal("登录域的键必须带 hash tag")
	}
	mustPasswordFinish(t, q, a, PasswordMismatch)
	b := mustPasswordBegin(t, q, "álice")
	mustPasswordFinish(t, q, b, PasswordAbort)
	u, err := q.Begin(context.Background(), UnknownPasswordAccount, "Álice")
	if err != nil {
		t.Fatal(err)
	}
	mustPasswordFinish(t, q, u, PasswordAbort)
}

func TestPasswordQuotaRejectsMixedConfigAndMalformedStateWithoutDeleting(t *testing.T) {
	q, _ := testPasswordQuota(t, 2)
	attempt := mustPasswordBegin(t, q, "state")
	mixed := &RedisPasswordFailureQuota{fingerprint: "v1:3:900000:300000:30000", limit: 3, window: q.window, lock: q.lock, run: q.run, now: q.now}
	if _, err := mixed.Begin(context.Background(), KnownPasswordAccount, "state"); !errors.Is(err, ErrPasswordQuotaUnavailable) {
		t.Fatalf("混配=%v", err)
	}
	// 同一个实际 Redis 接口修改状态，不能靠脚本自动删键恢复。
	original := q.run
	q.run = func(ctx context.Context, script *redis.Script, key string, args ...any) (any, error) {
		return original(ctx, redis.NewScript("redis.call('HSET',KEYS[1],'n','broken'); return {1,0}"), key)
	}
	_, _ = q.run(context.Background(), nil, attempt.key)
	q.run = original
	if err := q.Finish(context.Background(), attempt, PasswordSuccess); !errors.Is(err, ErrPasswordQuotaUnavailable) {
		t.Fatalf("坏状态=%v", err)
	}
	if _, err := q.Begin(context.Background(), KnownPasswordAccount, "state"); !errors.Is(err, ErrPasswordQuotaUnavailable) {
		t.Fatalf("坏状态不得被删除=%v", err)
	}
}

func TestPasswordQuotaUnknownRepliesCancellationAndLocalLease(t *testing.T) {
	for _, operation := range []string{"begin", "finish"} {
		t.Run(operation, func(t *testing.T) {
			q, advance := testPasswordQuota(t, 2)
			var attempt PasswordAttempt
			if operation == "finish" {
				attempt = mustPasswordBegin(t, q, "late")
			}
			original := q.run
			q.run = func(ctx context.Context, script *redis.Script, key string, args ...any) (any, error) {
				value, err := original(ctx, script, key, args...)
				advance(PasswordAttemptLease)
				return value, err
			}
			var err error
			if operation == "begin" {
				_, err = q.Begin(context.Background(), KnownPasswordAccount, "late")
			} else {
				err = q.Finish(context.Background(), attempt, PasswordSuccess)
			}
			if !errors.Is(err, ErrPasswordQuotaObsolete) {
				t.Fatalf("迟到确认不得认证=%v", err)
			}
		})
	}
	q, _ := testPasswordQuota(t, 2)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := q.Begin(ctx, KnownPasswordAccount, "cancelled"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	q.run = func(ctx context.Context, _ *redis.Script, _ string, _ ...any) (any, error) {
		<-ctx.Done()
		return []any{int64(1), int64(0)}, nil
	}
	started := time.Now()
	if _, err := q.Begin(context.Background(), KnownPasswordAccount, "timeout"); !errors.Is(err, ErrPasswordQuotaUnavailable) {
		t.Fatal(err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("Redis子预算失效")
	}
}

func TestPasswordQuotaRejectsUnsafeConstructorInputs(t *testing.T) {
	var nilClient *redis.Client
	if _, err := NewRedisPasswordFailureQuota(nilClient, 5, time.Minute, time.Minute); err == nil {
		t.Fatal("typed nil")
	}
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	defer rdb.Close()
	for _, args := range []struct {
		n    int
		w, l time.Duration
	}{{0, time.Minute, time.Minute}, {101, time.Minute, time.Minute}, {5, 0, time.Minute}, {5, time.Minute, 2 * time.Hour}, {5, time.Second + time.Nanosecond, time.Minute}} {
		if _, err := NewRedisPasswordFailureQuota(rdb, args.n, args.w, args.l); err == nil {
			t.Fatal("无效配额配置被接受")
		}
	}
}

func TestPasswordQuotaLostRepliesRemainFailClosedAndAllowSameOutcomeConfirmation(t *testing.T) {
	q, _ := testPasswordQuota(t, 2)
	original := q.run
	q.run = func(ctx context.Context, script *redis.Script, key string, args ...any) (any, error) {
		if _, err := original(ctx, script, key, args...); err != nil {
			return nil, err
		}
		return nil, errors.New("响应丢失")
	}
	if _, err := q.Begin(context.Background(), KnownPasswordAccount, "unknown-begin"); !errors.Is(err, ErrPasswordQuotaUnavailable) {
		t.Fatal(err)
	}
	q.run = original
	if _, err := q.Begin(context.Background(), KnownPasswordAccount, "unknown-begin"); !errors.Is(err, ErrPasswordQuotaDenied) {
		t.Fatal("Begin未知必须占有有限租约")
	}
	attempt := mustPasswordBegin(t, q, "unknown-finish")
	q.run = func(ctx context.Context, script *redis.Script, key string, args ...any) (any, error) {
		if _, err := original(ctx, script, key, args...); err != nil {
			return nil, err
		}
		return nil, errors.New("响应丢失")
	}
	if err := q.Finish(context.Background(), attempt, PasswordMismatch); !errors.Is(err, ErrPasswordQuotaUnavailable) {
		t.Fatal(err)
	}
	q.run = original
	mustPasswordFinish(t, q, attempt, PasswordMismatch)
	if err := q.Finish(context.Background(), attempt, PasswordAbort); !errors.Is(err, ErrPasswordQuotaConflict) {
		t.Fatal("已结算未知分支不能换abort")
	}
	second := mustPasswordBegin(t, q, "unknown-finish")
	mustPasswordFinish(t, q, second, PasswordMismatch)
	if _, err := q.Begin(context.Background(), KnownPasswordAccount, "unknown-finish"); !errors.Is(err, ErrPasswordQuotaDenied) {
		t.Fatal("幂等确认后仍须两次真实失败才锁")
	}
}

// 配额脚本走生产的登录域构造器:守卫拒绝不带 tag 的键,上了 Cluster 不会收到 CROSSSLOT。
func TestPasswordQuotaPassesTheClusterGuard(t *testing.T) {
	server := miniredis.RunT(t)
	rdb, err := playerredis.New(playerredis.Config{Addrs: []string{server.Addr()}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	quota, err := NewRedisPasswordFailureQuota(rdb, 2, 15*time.Minute, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	attempt := mustPasswordBegin(t, quota, "guarded")
	mustPasswordFinish(t, quota, attempt, PasswordMismatch)
	if !server.Exists(attempt.key) {
		t.Fatalf("配额记录不在预期的键上: %v", server.Keys())
	}
}

func TestPasswordQuotaRealRedisCrossReplica(t *testing.T) {
	if os.Getenv("LOGIN_TEST_LOCAL_AUTH_FIXTURES") != "1" {
		t.Skip("显式开启本机13379专用Redis夹具")
	}
	clients := []*redis.Client{redis.NewClient(&redis.Options{Addr: "127.0.0.1:13379", DB: 14, MaxRetries: -1, ContextTimeoutEnabled: true, DialTimeout: PasswordQuotaBudget, ReadTimeout: PasswordQuotaBudget, WriteTimeout: PasswordQuotaBudget}), redis.NewClient(&redis.Options{Addr: "127.0.0.1:13379", DB: 14, MaxRetries: -1, ContextTimeoutEnabled: true, DialTimeout: PasswordQuotaBudget, ReadTimeout: PasswordQuotaBudget, WriteTimeout: PasswordQuotaBudget})}
	// 账号带随机后缀:键按账号摘要算,用例之间互不相撞,结束时只删本例的键。
	account := "same-" + uuid.NewString()
	var quotas []*RedisPasswordFailureQuota
	for _, client := range clients {
		t.Cleanup(func() { _ = client.Close() })
		q, err := NewRedisPasswordFailureQuota(client, 5, 15*time.Minute, 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		quotas = append(quotas, q)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := clients[0].Ping(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var winner PasswordAttempt
	var winners int
	var wg sync.WaitGroup
	for index := 0; index < 24; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			attempt, err := quotas[index%2].Begin(ctx, KnownPasswordAccount, account)
			if err == nil {
				mu.Lock()
				winner = attempt
				winners++
				mu.Unlock()
			} else if !errors.Is(err, ErrPasswordQuotaDenied) {
				t.Errorf("Begin=%v", err)
			}
		}(index)
	}
	wg.Wait()
	if winners != 1 {
		t.Fatalf("同账号在途=%d", winners)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := clients[0].Del(cleanup, winner.key).Err(); err != nil {
			t.Error(err)
		}
	})
	mustPasswordFinish(t, quotas[0], winner, PasswordAbort)
	for index := 0; index < 5; index++ {
		q := quotas[index%2]
		mustPasswordFinish(t, q, mustPasswordBegin(t, q, account), PasswordMismatch)
	}
	for _, q := range quotas {
		if _, err := q.Begin(ctx, KnownPasswordAccount, account); !errors.Is(err, ErrPasswordQuotaDenied) {
			t.Fatalf("五次后锁定=%v", err)
		}
	}
	if count, err := clients[0].HLen(ctx, winner.key).Result(); err != nil || count != 10 {
		t.Fatalf("有限状态字段=%d,%v", count, err)
	}
	t.Log(fmt.Sprintf("真实Redis双客户端：24路同账号Begin仅1通过，5次失败后两副本均拒绝；自有键由cleanup删除"))
}
