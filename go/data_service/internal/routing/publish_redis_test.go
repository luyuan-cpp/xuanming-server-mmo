package routing

// db_first 的 W1 发布专用客户端(publish_redis.go,A2-1 / LR-4)的契约:只在 db_first 构造、连接参数与 MappingRedis 同源、
// Router.Close 关闭、池大小走 RouteStore.PublishRedisPoolSize 并受 Validate 约束、读路径不与它共池,以及 ProbeWaitAOF 改用
// 同一构造口径的临时客户端。miniredis,不连真 Redis;「EVAL 与 WAITAOF 在同一条连接上」由 router_integration_test.go 的
// TestPublishRoute_RealRedisSameConnection 在真 Redis 上验。

import (
	"context"
	"reflect"
	"testing"
	"time"

	"data_service/internal/config"
	"data_service/internal/store"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// publishTestConfig 返回 mapping Redis 与 DevRedis 都指向 mr 的配置(WAITAOF 0/0:miniredis 不支持 WAITAOF)。
// tweak 可改 RouteStore 段。
func publishTestConfig(mr *miniredis.Miniredis, mode string, tweak func(*config.RouteStoreConfig)) config.Config {
	rc := config.RouteStoreConfig{Mode: mode, WaitAOFNumLocal: intPtr(0), WaitAOFNumReplicas: intPtr(0)}
	if tweak != nil {
		tweak(&rc)
	}
	return config.Config{
		MappingRedis:     redis.RedisConf{Host: mr.Addr(), Type: "node"},
		DevRedis:         config.DevRedisConfig{Host: mr.Addr()},
		PlayerLockTTLSec: 3,
		RouteStore:       rc,
	}
}

// TestMappingRedisOptions_SameSourceAsMappingRedis:发布 / 探针客户端的连接身份逐项取自同一份 MappingRedis(与 go-zero getClient
// 对同一配置的换算相同:Host→Addr、User、Pass、DB 恒 0、Tls=true 时 InsecureSkipVerify),池 / 读超时取调用方给的值,
// MaxRetries=-1、MinIdleConns=0、ContextTimeoutEnabled=true。Type 只许空或 node,Host 不许空,池大小必须为正。
func TestMappingRedisOptions_SameSourceAsMappingRedis(t *testing.T) {
	for _, typ := range []string{"", redis.NodeType} {
		opts, err := mappingRedisOptions(redis.RedisConf{Host: "redis.infra:6379", Type: typ, User: "u", Pass: "p", Tls: true}, 7, 2500*time.Millisecond)
		require.NoError(t, err, "Type=%q", typ)
		assert.Equal(t, "redis.infra:6379", opts.Addr)
		assert.Equal(t, "u", opts.Username)
		assert.Equal(t, "p", opts.Password)
		assert.Zero(t, opts.DB, "go-zero's RedisConf has no DB field: the mapping redis is always DB 0")
		require.NotNil(t, opts.TLSConfig, "Tls=true must enable TLS like go-zero does")
		assert.True(t, opts.TLSConfig.InsecureSkipVerify, "same handling of Tls=true as go-zero getClient")
		assert.Equal(t, 7, opts.PoolSize)
		assert.Zero(t, opts.MinIdleConns, "no idle connections are pre-built")
		assert.Equal(t, -1, opts.MaxRetries, "go-redis must not resend the whole pipeline; PublishAttempts is the only retry layer")
		assert.Equal(t, 2500*time.Millisecond, opts.ReadTimeout)
		assert.True(t, opts.ContextTimeoutEnabled, "waiting for a pooled connection and WAITAOF must not outlive the caller's deadline")
	}

	plain, err := mappingRedisOptions(redis.RedisConf{Host: "h:1", Type: redis.NodeType}, 1, time.Second)
	require.NoError(t, err)
	assert.Nil(t, plain.TLSConfig, "Tls=false: plaintext, same as go-zero")
	assert.Empty(t, plain.Username)
	assert.Empty(t, plain.Password)

	for name, tc := range map[string]struct {
		mapping redis.RedisConf
		pool    int
		want    string
	}{
		"cluster type": {redis.RedisConf{Host: "h:1", Type: redis.ClusterType}, 1, "single-instance"},
		"blank host":   {redis.RedisConf{Host: "  ", Type: redis.NodeType}, 1, "Host is empty"},
		"zero pool":    {redis.RedisConf{Host: "h:1", Type: redis.NodeType}, 0, "pool size"},
	} {
		_, err := mappingRedisOptions(tc.mapping, tc.pool, time.Second)
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), tc.want, name)
	}
}

// TestMappingRedisConfFieldsAreAllAccountedFor:go-zero 的 redis.RedisConf 一旦多出连接身份字段(DB、哨兵主名、拓扑之类),
// mappingRedisOptions 就不再与读路径的 go-zero 客户端同源 —— 这里先红,提醒同步改 mappingRedisOptions。
// NonBlock / PingTimeout 只管 go-zero 构造时要不要先 PING,不是连接身份(发布客户端本来就懒连接)。
func TestMappingRedisConfFieldsAreAllAccountedFor(t *testing.T) {
	typ := reflect.TypeOf(redis.RedisConf{})
	var got []string
	for i := 0; i < typ.NumField(); i++ {
		got = append(got, typ.Field(i).Name)
	}
	assert.ElementsMatch(t, []string{"Host", "Type", "User", "Pass", "Tls", "NonBlock", "PingTimeout"}, got,
		"go-zero RedisConf changed: re-check that mappingRedisOptions still dials what the go-zero mapping client dials")
}

// TestNewRouter_PublishClientOnlyUnderDBFirst:redis 模式不建发布客户端(行为与改动前逐字相同);db_first 由 NewRouter 自建,
// 地址取 MappingRedis,池取 PublishRedisPoolSize(没配 = 缺省),读超时 = WaitAOFTimeoutMs + 1s,不自动重发,构造本身不拨号。
// 注入了(withPublishRedis)就不自建。
func TestNewRouter_PublishClientOnlyUnderDBFirst(t *testing.T) {
	mr := miniredis.RunT(t)

	redisMode := NewRouter(publishTestConfig(mr, config.RouteStoreModeRedis, nil))
	t.Cleanup(redisMode.Close)
	assert.Nil(t, redisMode.publishRedis, "redis mode builds no publish client and opens no connection")

	r := NewRouter(publishTestConfig(mr, config.RouteStoreModeDBFirst, func(rc *config.RouteStoreConfig) { rc.WaitAOFTimeoutMs = 1200 }))
	t.Cleanup(r.Close)
	require.NotNil(t, r.publishRedis, "db_first must build the publish client")
	opts := r.publishRedis.Options()
	assert.Equal(t, mr.Addr(), opts.Addr, "same instance as MappingRedis")
	assert.Equal(t, config.DefaultRouteStorePublishRedisPoolSize, opts.PoolSize, "PublishRedisPoolSize unset = the default")
	assert.Equal(t, 1200*time.Millisecond+publishRedisReadTimeoutSlack, opts.ReadTimeout)
	assert.Zero(t, opts.MaxRetries, "go-redis turns MaxRetries=-1 into 0: no automatic resend")
	assert.Zero(t, opts.MinIdleConns)
	assert.True(t, opts.ContextTimeoutEnabled)
	assert.Zero(t, r.publishRedis.PoolStats().TotalConns, "constructing the publish client dials nothing")

	sized := NewRouter(publishTestConfig(mr, config.RouteStoreModeDBFirst, func(rc *config.RouteStoreConfig) { rc.PublishRedisPoolSize = 7 }))
	t.Cleanup(sized.Close)
	assert.Equal(t, 7, sized.publishRedis.Options().PoolSize, "PublishRedisPoolSize is honoured")

	injected := goredis.NewClient(&goredis.Options{Addr: mr.Addr(), PoolSize: 1, MaxRetries: -1})
	withInjected := NewRouter(publishTestConfig(mr, config.RouteStoreModeDBFirst, nil), withPublishRedis(injected))
	t.Cleanup(withInjected.Close)
	assert.Same(t, injected, withInjected.publishRedis, "an injected publish client must not be replaced")
}

// TestRouterClose_ClosesPublishClient:发布客户端(自建的与注入的)归 Router 所有,Router.Close 关闭它;之后再用回 ErrClosed。
// 这里不登记 t.Cleanup(r.Close),免得同一个 Router 关两次。
func TestRouterClose_ClosesPublishClient(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)

	r := NewRouter(publishTestConfig(mr, config.RouteStoreModeDBFirst, nil))
	pub := r.publishRedis
	require.NotNil(t, pub)
	require.NoError(t, pub.Ping(ctx).Err())
	r.Close()
	assert.ErrorIs(t, pub.Ping(ctx).Err(), goredis.ErrClosed, "Router.Close must close the self-built publish client")

	injected := goredis.NewClient(&goredis.Options{Addr: mr.Addr(), PoolSize: 1, MaxRetries: -1})
	r = NewRouter(publishTestConfig(mr, config.RouteStoreModeDBFirst, nil), withPublishRedis(injected))
	require.NoError(t, injected.Ping(ctx).Err())
	r.Close()
	assert.ErrorIs(t, injected.Ping(ctx).Err(), goredis.ErrClosed, "an injected publish client is owned by the Router too")
}

// TestNewRouter_PublishPoolSizeValidated:PublishRedisPoolSize 越界([1, 128] 之外;0 由 Normalize 换成缺省)在 NewRouter 里就 panic,
// 与 Mode 无关(同 TestNewRouterPanicsOnInvalidRouteStore:panic 先于任何连接,地址不会被拨);边界值本身照常构造。
func TestNewRouter_PublishPoolSizeValidated(t *testing.T) {
	for _, mode := range []string{config.RouteStoreModeRedis, config.RouteStoreModeDBFirst} {
		for _, size := range []int{-1, config.MaxRouteStorePublishRedisPoolSize + 1} {
			c := config.Config{
				MappingRedis:     redis.RedisConf{Host: "127.0.0.1:1", Type: "node"},
				PlayerLockTTLSec: 3,
				RouteStore:       config.RouteStoreConfig{Mode: mode, PublishRedisPoolSize: size},
			}
			assert.Panics(t, func() { NewRouter(c) }, "Mode=%s PublishRedisPoolSize=%d", mode, size)
		}
	}

	mr := miniredis.RunT(t)
	for _, size := range []int{1, config.MaxRouteStorePublishRedisPoolSize} {
		r := NewRouter(publishTestConfig(mr, config.RouteStoreModeDBFirst, func(rc *config.RouteStoreConfig) { rc.PublishRedisPoolSize = size }))
		t.Cleanup(r.Close)
		assert.Equal(t, size, r.publishRedis.Options().PoolSize)
	}
}

// TestDBFirstPublishPoolExhaustedDoesNotBlockReads 是 A2-1 要的隔离用例:发布池只有 1 条连接且被占住时,读路径
// (GetPlayerHomeZone / 带围栏的单查 / 批量 / 围栏预检)照常成功;W1 只在自己的池上排队,到调用方截止时间失败
// (库已提交、缓存零写入,错误认得出是截止时间,不是库故障)。改动前两者共用 go-zero 的同一个池,这里读会一起排队。
// 截止时间只是 W1 等待的上界:被占住的连接在用例结束前不会归还,结论不取决于调度。
func TestDBFirstPublishPoolExhaustedDoesNotBlockReads(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	opts, err := mappingRedisOptions(redis.RedisConf{Host: mr.Addr(), Type: redis.NodeType}, 1, time.Second)
	require.NoError(t, err)
	pub := goredis.NewClient(opts)
	fake := &routeFakeStore{result: committed(store.RouteRegisterInserted, 9611, 5, 0, 0, 1)}
	r := NewRouter(publishTestConfig(mr, config.RouteStoreModeDBFirst, nil), WithRouteStore(fake), withPublishRedis(pub))
	t.Cleanup(r.Close)

	// 占住发布池唯一的一条连接(粘滞连接不归还,直到 Close)。晚于 r.Close 登记 → 先于它执行。
	held := pub.Conn()
	t.Cleanup(func() { _ = held.Close() })
	require.NoError(t, held.Ping(ctx).Err())
	require.Zero(t, pub.PoolStats().IdleConns, "the publish pool's only connection is checked out")

	require.NoError(t, mr.Set(mappingKey(9610), "5"))
	zone, err := r.GetPlayerHomeZone(ctx, 9610)
	require.NoError(t, err, "reads do not share the publish pool")
	assert.Equal(t, uint32(5), zone)
	zone, merging, err := r.GetPlayerHomeZoneAndMergeFence(ctx, 9610)
	require.NoError(t, err)
	assert.Equal(t, uint32(5), zone)
	assert.False(t, merging)
	batch, err := r.BatchGetPlayerHomeZone(ctx, []uint64{9610, 9611})
	require.NoError(t, err)
	assert.Equal(t, map[uint64]uint32{9610: 5}, batch)
	fenced, err := r.IsMergeInProgress(ctx, 5)
	require.NoError(t, err)
	assert.False(t, fenced)

	wctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	err = r.RegisterPlayerZone(wctx, 9611, 5, 0)
	require.Error(t, err, "W1 waits on its own exhausted pool until the caller's deadline")
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.NotErrorIs(t, err, ErrRouteStore, "a publish that never got a connection is not a store failure")
	assert.Len(t, fake.reqs, 1, "the store commit happened; only the publish waited")
	assert.Equal(t, [3]string{}, routeKeysOf(t, mr, 9611), "nothing was published")
}

// TestProbeWaitAOF_OwnClientAndErrorKinds:WAITAOF 探针用自己的临时客户端(与发布客户端同一构造口径,不经 go-zero),
// 两项都是 0 时什么都不发;Type 不是 node 在拨号之前就拒绝;连不上报「没执行」(不误报成 AOF 配置问题);
// Redis 回错误回复(miniredis 没有 WAITAOF 命令,等同 Redis < 7.2)才报能力问题。
func TestProbeWaitAOF_OwnClientAndErrorKinds(t *testing.T) {
	ctx := context.Background()
	off := config.RouteStoreConfig{Mode: config.RouteStoreModeDBFirst, WaitAOFNumLocal: intPtr(0), WaitAOFNumReplicas: intPtr(0)}
	require.NoError(t, ProbeWaitAOF(ctx, redis.RedisConf{Host: "127.0.0.1:1", Type: redis.NodeType}, off),
		"WAITAOF 0/0 sends nothing and must not touch the address")

	on := config.RouteStoreConfig{Mode: config.RouteStoreModeDBFirst}
	err := ProbeWaitAOF(ctx, redis.RedisConf{Host: "127.0.0.1:1", Type: redis.ClusterType}, on)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "single-instance")

	mr := miniredis.RunT(t)
	addr := mr.Addr()
	err = ProbeWaitAOF(ctx, redis.RedisConf{Host: addr, Type: redis.NodeType}, on)
	require.Error(t, err, "miniredis has no WAITAOF")
	assert.Contains(t, err.Error(), "needs appendonly yes", "an error reply is a capability problem")

	mr.Close()
	err = ProbeWaitAOF(ctx, redis.RedisConf{Host: addr, Type: redis.NodeType}, on)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not executed", "an unreachable mapping redis must not be reported as an AOF misconfiguration")
}
