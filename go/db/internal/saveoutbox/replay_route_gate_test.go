package saveoutbox

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"shared/placement"
	"shared/savebarrier"
)

// 第二轮审计 data-overwrite-rollback-audit-r2-20261019.md §3.7 r2-rr#5(数据侧)的回归用例。
// mapping Redis 重启或切主后，对账 leader 撤掉就绪标记，全部 zone 的 go/db 就绪闸关上、不从 Kafka 取消息。改动前补投器不看闸：
// 每个未 ACK 的意图照常按退避整行重投，broker 确认后计次，约 21 分钟进 exhausted；同时放大 Kafka 写入，保留缓冲被裁后原任务与
// 补投副本都没了，返场永远报 parent SQL barrier not committed。修复后补投器在本实例闸关着时整拍跳过、不认领、不计次，闸重开后照常补投。
// 时间经 Replay.now 注入，抖动置零，不依赖真实墙钟。
// 红证据(变异核对)：去掉 Tick 开头与循环里的 routeGateClosed 两处判定，第一条用例第一拍就发布 2 条，6 拍后 attempts=3、exhausted 计数大于 0。

// 闸关着的多拍(跨过整段退避、是次数上限的两倍)：不碰 broker、不计次、不推迟索引、不进 exhausted；闸重开后第一拍照常补投并计一次。
func TestRouteGateClosedSkipsReplayWithoutConsumingAttempts(t *testing.T) {
	rc := fixtureRedis(t)
	ctx := context.Background()
	in := testIntent(t, fixtureID, 7, 9, 1)
	storeObservedIntent(t, rc, fixtureID, 1, in)
	p := &recordingPublisher{}
	r := testReplayWith(t, rc, p, nil, Config{RetryAfter: time.Second, MaxRetryAfter: 3 * time.Second, MaxAttempts: 3, RecheckAfter: time.Minute})
	gate := &routeGateStub{} // mapping Redis 副本不可信，闸关着
	r.route = gate
	now := fixtureNowMs
	for i := 0; i < 6; i++ {
		at := now
		r.bucket, r.now = 0, func() time.Time { return time.UnixMilli(at) }
		n, err := r.Tick(ctx)
		require.NoError(t, err, "第 %d 拍:闸关着是预期的暂停,不报错刷日志", i+1)
		require.Zero(t, n)
		now += time.Minute.Milliseconds()
	}
	require.Zero(t, p.calls, "闸关着不碰 broker,不放大 Kafka 写入")
	require.Equal(t, "0", rc.HGet(ctx, savebarrier.Key(fixtureID), fieldAttempts).Val(), "闸关着不计次")
	require.Equal(t, int64(0), indexScore(t, rc, fixtureID), "闸关着不认领,索引到期时刻不动")
	require.Zero(t, resultCount(r, "exhausted"))
	require.Equal(t, 6.0, resultCount(r, "route_not_ready"))

	gate.open = true // 对账 leader 重新挂上标记，闸重开
	at := now
	r.bucket, r.now = 0, func() time.Time { return time.UnixMilli(at) }
	n, err := r.Tick(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, n, "闸重开后照常补投")
	require.Equal(t, []publishedTask{{in.topic, "64", in.first}, {in.topic, "64", in.second}}, p.tasks, "原 bytes / key 一字不改")
	require.Equal(t, "1", rc.HGet(ctx, savebarrier.Key(fixtureID), fieldAttempts).Val(), "重开后的第一次补投才计一次")
	require.Equal(t, at+time.Second.Milliseconds(), indexScore(t, rc, fixtureID), "退避从第一次补投重新开始")
	require.Equal(t, in.version, rc.HGet(ctx, savebarrier.Key(fixtureID), "pending").Val())
}

// 闸在本拍中途关上：已认领的那一名照常投完并计次，剩下的玩家不认领、不推迟、不计次(与重试循环每认领一条前看一次闸同口径)。
func TestRouteGateClosingMidTickStopsClaiming(t *testing.T) {
	rc := fixtureRedis(t)
	ctx := context.Background()
	const otherID uint64 = 128 // 与 fixtureID 同桶
	storeObservedIntent(t, rc, fixtureID, 1, testIntent(t, fixtureID, 7, 9, 1))
	storeObservedIntent(t, rc, otherID, 1, testIntent(t, otherID, 8, 1, 1))
	gate := openRouteGate()
	// 第一次发布时闸关上；testReplay 的 Batch=2，两名玩家同一拍到期。
	p := &recordingPublisher{hook: func() { gate.open = false }}
	r := testReplay(t, rc, p)
	r.route = gate
	n, err := r.Tick(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, n, "闸关上前已认领的那一名两条照常投完")
	require.Equal(t, p.tasks[0].key, p.tasks[1].key, "只投了一名玩家")
	first, err := strconv.ParseUint(p.tasks[0].key, 10, 64)
	require.NoError(t, err)
	rest := fixtureID
	if first == fixtureID {
		rest = otherID
	}
	require.Equal(t, "1", rc.HGet(ctx, savebarrier.Key(first), fieldAttempts).Val(), "已投完的那一名照常计一次")
	require.Equal(t, "0", rc.HGet(ctx, savebarrier.Key(rest), fieldAttempts).Val(), "闸关上后剩下的玩家不计次")
	require.Equal(t, int64(0), indexScore(t, rc, rest), "闸关上后剩下的玩家不认领")
	require.Equal(t, 1.0, resultCount(r, "route_not_ready"))
}

// failingCommandHook 让一个客户端上指定名字的命令一律返回注入的错误(模拟 mapping Redis 重启 / 切主期间的读失败)，不经网络。
type failingCommandHook struct {
	command string
	err     error
}

func (h failingCommandHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h failingCommandHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == h.command {
			cmd.SetErr(h.err)
			return h.err
		}
		return next(ctx, cmd)
	}
}

func (h failingCommandHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// redisReplyError 模拟服务端错误回复：实现 redis.Error，与 go-redis 解析出的回复错误属于同一类。
type redisReplyError string

func (e redisReplyError) Error() string { return string(e) }

func (redisReplyError) RedisError() {}

// 跨 zone 索引迁移读权威 home_zone 时 mapping Redis 读不到：传输故障、重启切主期间的服务端回复(LOADING)、权限配置错误(NOPERM)
// 都与意图本身无关，不计次、不迁索引、报错留待下一次到期。服务端回复里只有 WRONGTYPE 按确定性数据错误计次
// (TestHomeZoneWrongTypeConsumesAttempt)；键缺失或不匹配照常计次(TestHomeZoneIndexMigrationRequiresAuthoritativeMapping)。
func TestHomeZoneReadErrorDoesNotConsumeAttempt(t *testing.T) {
	for _, tc := range []struct {
		name     string
		injected error
	}{
		{"transport", errors.New("injected: mapping Redis connection refused")},
		{"loading", redisReplyError("LOADING Redis is loading the dataset in memory")},
		{"noperm", redisReplyError("NOPERM User go_db has no permissions to access the 'player:zone:64' key")},
	} {
		injected := tc.injected
		t.Run(tc.name, func(t *testing.T) {
			rc := fixtureRedis(t)
			ctx := context.Background()
			in := testIntent(t, fixtureID, 7, 9, 2) // 归属已迁到 zone 2，旧 zone 1 的补投桶里还留着它
			storeObservedIntent(t, rc, fixtureID, 1, in)
			mapping := redis.NewClient(&redis.Options{Addr: rc.Options().Addr})
			t.Cleanup(func() { _ = mapping.Close() })
			mapping.AddHook(failingCommandHook{command: "get", err: injected})
			p := &recordingPublisher{}
			r, err := New(rc, mapping, p, nil, openRouteGate(), Config{ZoneID: 1, Batch: 2, Budget: 100 * time.Millisecond, RetryAfter: time.Second,
				Interval: 10 * time.Millisecond}, prometheus.NewRegistry())
			require.NoError(t, err)
			r.now = func() time.Time { return time.UnixMilli(fixtureNowMs) }
			r.jitter = func() time.Duration { return 0 }
			_, err = r.Tick(ctx)
			require.ErrorIs(t, err, injected, "读不到权威 home_zone 必须报错并带上原因,不能当成不匹配")
			require.Empty(t, p.tasks)
			require.Equal(t, "0", rc.HGet(ctx, savebarrier.Key(fixtureID), fieldAttempts).Val(), "mapping 读失败不计次")
			require.Equal(t, int64(1), rc.ZCard(ctx, pendingIndexKey(1, 0)).Val(), "不迁索引")
			require.Zero(t, rc.ZCard(ctx, pendingIndexKey(2, 0)).Val())
			require.Equal(t, in.first, rc.HGet(ctx, savebarrier.Key(fixtureID), fieldFirst).Val(), "原意图保留")
			require.Equal(t, 1.0, resultCount(r, "redis_error"))
			require.Zero(t, resultCount(r, "invalid"))
		})
	}
}

// 权威 home_zone 键被写成别的类型(真实 GET 稳定回 WRONGTYPE，miniredis 同此行为)：这是该玩家映射数据的确定性错误，重读不会变好。
// 必须与键缺失同口径计次，次数用完进 exhausted 并告警；若当成基础设施故障不计次，意图会永远停在基数退避上，返场一直被拒而没有告警。
// 红证据(变异核对)：把 replay.go 里 WRONGTYPE 从计次分支挪回故障分支，本用例因 Tick 报错、attempts=0 而失败。
func TestHomeZoneWrongTypeConsumesAttempt(t *testing.T) {
	rc := fixtureRedis(t)
	ctx := context.Background()
	in := testIntent(t, fixtureID, 7, 9, 2) // 归属已迁到 zone 2，旧 zone 1 的补投桶里还留着它
	storeObservedIntent(t, rc, fixtureID, 1, in)
	require.NoError(t, rc.HSet(ctx, placement.HomeZoneKey(fixtureID), "zone", "2").Err(), "映射键被误写成 hash")
	p := &recordingPublisher{}
	r := testReplay(t, rc, p) // mapping 与 rc 同一个 miniredis
	_, err := r.Tick(ctx)
	require.NoError(t, err, "确定性的数据错误按意图不合法计次,不当作本拍故障")
	require.Empty(t, p.tasks, "缺权威 home_zone 时不发布任何任务")
	require.Equal(t, "1", rc.HGet(ctx, savebarrier.Key(fixtureID), fieldAttempts).Val(), "与键缺失同口径计一次")
	require.Equal(t, int64(1), rc.ZCard(ctx, pendingIndexKey(1, 0)).Val(), "不迁索引")
	require.Zero(t, rc.ZCard(ctx, pendingIndexKey(2, 0)).Val())
	require.Equal(t, in.first, rc.HGet(ctx, savebarrier.Key(fixtureID), fieldFirst).Val(), "原意图保留")
	require.Equal(t, 1.0, resultCount(r, "invalid"))
	require.Zero(t, resultCount(r, "redis_error"))
}

// 装配漏传就绪闸必须拒启，不能静默当作恒放行。
func TestNewRequiresRouteGate(t *testing.T) {
	rc := fixtureRedis(t)
	_, err := New(rc, rc, &recordingPublisher{}, nil, nil, Config{ZoneID: 1}, prometheus.NewRegistry())
	require.Error(t, err)
}
