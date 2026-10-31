package logic

import (
	"context"
	"errors"
	"math"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"guild/internal/constants"
	"guild/internal/data"
	pb "proto/guild"
	"shared/generated/pb/table"
)

func newTestApplicationQuota(t *testing.T, rdb redis.UniversalClient, limit uint32) *ApplicationQuota {
	t.Helper()
	q, err := NewApplicationQuota(rdb, limit)
	require.NoError(t, err)
	return q
}

func TestApplicationQuotaFixedWindowAndMissingTTL(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	q := newTestApplicationQuota(t, rdb, 2)
	ctx := context.Background()
	key := applicationQuotaKey(math.MaxUint64)
	require.Equal(t, "guild:{apply:18446744073709551615}", key)
	for i := 0; i < 3; i++ {
		allowed, err := q.Allow(ctx, math.MaxUint64)
		require.NoError(t, err)
		require.Equal(t, i < 2, allowed)
	}
	require.Equal(t, time.Minute, mr.TTL(key))
	mr.FastForward(20 * time.Second)
	allowed, err := q.Allow(ctx, math.MaxUint64)
	require.NoError(t, err)
	require.False(t, allowed)
	require.Equal(t, 40*time.Second, mr.TTL(key), "重复尝试不得延长窗口")
	mr.FastForward(40 * time.Second)
	allowed, err = q.Allow(ctx, math.MaxUint64)
	require.NoError(t, err)
	require.True(t, allowed)
	require.NoError(t, mr.Set(key, "8"))
	require.NoError(t, rdb.Persist(ctx, key).Err())
	allowed, err = q.Allow(ctx, math.MaxUint64)
	require.NoError(t, err)
	require.False(t, allowed)
	require.Equal(t, time.Minute, mr.TTL(key), "遗留无 TTL 计数须自动恢复过期")
}

func TestApplicationQuotaConcurrentInstancesShareBudget(t *testing.T) {
	mr := miniredis.RunT(t)
	rdbA := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	rdbB := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdbA.Close(); _ = rdbB.Close() })
	checkConcurrentApplicationQuota(t, newTestApplicationQuota(t, rdbA, 10), newTestApplicationQuota(t, rdbB, 10))
}

func checkConcurrentApplicationQuota(t *testing.T, a, b *ApplicationQuota) {
	t.Helper()
	var accepted, failures atomic.Int32
	var workers sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 40; i++ {
		workers.Add(1)
		go func(q *ApplicationQuota) {
			defer workers.Done()
			<-start
			allowed, err := q.Allow(context.Background(), 42)
			if err != nil {
				failures.Add(1)
			}
			if allowed {
				accepted.Add(1)
			}
		}([]*ApplicationQuota{a, b}[i%2])
	}
	close(start)
	workers.Wait()
	require.Zero(t, failures.Load())
	require.Equal(t, int32(10), accepted.Load(), "两个副本共用单个玩家预算")
	allowed, err := a.Allow(context.Background(), 43)
	require.NoError(t, err)
	require.True(t, allowed, "不同玩家独立计数")
}

func TestApplicationQuotaErrorsFailOpenButParentCancellationStops(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		err   error
	}{
		{"存储失败", nil, errors.New("Redis unavailable")},
		{"子预算超时", nil, context.DeadlineExceeded},
		{"空值", nil, nil}, {"类型异常", "1", nil}, {"非法计数", int64(0), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var outcome string
			q := &ApplicationQuota{limit: 10, observe: func(s string) { outcome = s }, eval: func(ctx context.Context, _ string) (any, error) {
				deadline, ok := ctx.Deadline()
				require.True(t, ok)
				require.LessOrEqual(t, time.Until(deadline), ApplicationQuotaBudget)
				return tc.value, tc.err
			}}
			allowed, err := q.Allow(context.Background(), 1)
			require.NoError(t, err)
			require.True(t, allowed)
			require.Equal(t, "error", outcome)
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	var observed int
	q := &ApplicationQuota{limit: 10, observe: func(string) { observed++ }, eval: func(context.Context, string) (any, error) {
		cancel()
		return nil, errors.New("Redis interrupted")
	}}
	allowed, err := q.Allow(ctx, 1)
	require.False(t, allowed)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, observed, "父取消不是配额故障放行")
	q.eval = func(context.Context, string) (any, error) { t.Fatal("取消后不应查询 Redis"); return nil, nil }
	_, err = q.Allow(ctx, 1)
	require.ErrorIs(t, err, context.Canceled)
}

func TestApplyJoinGuildQuotaRejectsBeforeRepositoryAndEffects(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	q := newTestApplicationQuota(t, rdb, 1)
	_, err := q.Allow(context.Background(), 42)
	require.NoError(t, err)
	notifier, bumper := &recordingNotifier{}, &recordingSyncBumper{}
	l := NewGuildLogic(nil, nil, nil, &fakeHomeZones{zone: 2},
		WithApplicationQuota(q), WithNotifier(notifier), WithSyncBumper(bumper))
	resp, err := l.ApplyJoinGuild(clientCtx(42), &pb.ApplyJoinGuildRequest{GuildId: 9})
	require.NoError(t, err)
	require.Equal(t, uint32(table.CommonError_kRateLimitExceeded), resp.GetErrorMessage().GetId())
	require.Zero(t, notifier.total())
	require.Zero(t, bumper.count())
}

func TestApplyJoinGuildQuotaCountsRepositoryRejectionsAndRefreshAttempts(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	q := newTestApplicationQuota(t, rdb, 2)
	repo := data.NewGuildRepo(rdb, nil, time.Minute)
	l := NewGuildLogic(repo, nil, nil, &fakeHomeZones{zone: 2}, WithApplicationQuota(q))
	// 缓存预检的非法值仓储拒绝发生在配额之后；相同 guild 的重试仍各消耗一次。
	require.NoError(t, rdb.Set(context.Background(), "player_guild:v2:42", "bad-guild-id", 0).Err())
	for i := 0; i < 2; i++ {
		_, err := l.ApplyJoinGuild(clientCtx(42), &pb.ApplyJoinGuildRequest{GuildId: 9})
		require.Error(t, err)
	}
	resp, err := l.ApplyJoinGuild(clientCtx(42), &pb.ApplyJoinGuildRequest{GuildId: 9})
	require.NoError(t, err)
	require.Equal(t, uint32(table.CommonError_kRateLimitExceeded), resp.GetErrorMessage().GetId())
	// 不经申请入口的撤回/其他方法没有扣减：配额只在 ApplyJoinGuild 中接线。
	count, readErr := mr.Get(applicationQuotaKey(42))
	require.NoError(t, readErr)
	require.Equal(t, "3", count)
}

func TestApplyJoinGuildQuotaAuthenticationAndTargetPrecedeConsumption(t *testing.T) {
	q := &ApplicationQuota{eval: func(context.Context, string) (any, error) { t.Fatal("非法请求不得消费配额"); return nil, nil }}
	l := NewGuildLogic(nil, nil, nil, &fakeHomeZones{zone: 2}, WithApplicationQuota(q))
	_, err := l.ApplyJoinGuild(context.Background(), &pb.ApplyJoinGuildRequest{GuildId: 9})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	resp, err := l.ApplyJoinGuild(clientCtx(42), &pb.ApplyJoinGuildRequest{})
	require.NoError(t, err)
	require.Equal(t, constants.ErrGuildNotFound, resp.GetErrorMessage().GetId())
	ctx, cancel := context.WithCancel(clientCtx(42))
	cancel()
	_, err = l.ApplyJoinGuild(ctx, &pb.ApplyJoinGuildRequest{GuildId: 9})
	require.Equal(t, codes.Canceled, status.Code(err))
}

func TestApplicationQuotaRealRedis(t *testing.T) {
	addr := os.Getenv("GUILD_QUOTA_TEST_REDIS")
	if addr == "" {
		t.Skip("设置 GUILD_QUOTA_TEST_REDIS 以验证独立真 Redis")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr, PoolSize: 4, MaxActiveConns: 4, MaxRetries: -1, DialerRetries: 1,
		ContextTimeoutEnabled: true, DialTimeout: ApplicationQuotaBudget, ReadTimeout: ApplicationQuotaBudget,
		WriteTimeout: ApplicationQuotaBudget, PoolTimeout: ApplicationQuotaBudget})
	t.Cleanup(func() { _ = rdb.Close() })
	prefix := "codex:guild-quota:" + uuid.NewString() + ":"
	wrap := func(q *ApplicationQuota) *ApplicationQuota {
		eval := q.eval
		q.eval = func(ctx context.Context, key string) (any, error) { return eval(ctx, prefix+key) }
		return q
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.NoError(t, rdb.Del(ctx, prefix+applicationQuotaKey(42), prefix+applicationQuotaKey(43)).Err())
	})
	checkConcurrentApplicationQuota(t, wrap(newTestApplicationQuota(t, rdb, 10)), wrap(newTestApplicationQuota(t, rdb, 10)))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	key := prefix + applicationQuotaKey(42)
	require.NoError(t, rdb.Persist(ctx, key).Err())
	q := wrap(newTestApplicationQuota(t, rdb, 10))
	allowed, err := q.Allow(ctx, 42)
	require.NoError(t, err)
	require.False(t, allowed)
	ttl, err := rdb.PTTL(ctx, key).Result()
	require.NoError(t, err)
	require.Greater(t, ttl, time.Duration(0))
	require.LessOrEqual(t, ttl, time.Minute)
	require.NoError(t, rdb.PExpire(ctx, key, 10*time.Second).Err())
	_, err = q.Allow(ctx, 42)
	require.NoError(t, err)
	ttl, err = rdb.PTTL(ctx, key).Result()
	require.NoError(t, err)
	require.LessOrEqual(t, ttl, 10*time.Second, "真实 Redis 重复调用不得延长窗口")
}
