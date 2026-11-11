package svc

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"scene_manager/internal/config"
	"shared/playerredis"

	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// 铸造通道在真 Redis 上的端到端用例(docs/design/zone-redis-regression-guard-20261022.md §4.3.3、§4.3.4)。
//
// miniredis 不认 WAITAOF,owner_epoch_mint_test.go 只走得到「WAITAOF 被拒」一支;logic 层用的是替身。这里在真 Redis 上钉住
// 铸造通道依赖的几条服务端行为:
//   - 同一条 pipeline 里的 WAITAOF 覆盖紧挨着的 EVAL(确认落盘);
//   - 单实例要 1 个副本确认时等满超时、回份数不足,耗时受 WAITAOF 的超时约束;
//   - 启动探针在正确配置下通过,配错副本数时失败;
//   - 值不变的写回照样传播、推进复制偏移,新连接上的 WAITAOF 因此覆盖别的连接更早的写 —— 铸造的重放识别与两段回执的
//     确认脚本都靠这一条;只读分支、CAS 不过的分支不推进偏移,这时 WAITAOF 的「确认」什么都没证明;
//   - 池被占满时取不到连接,通道回 ErrOwnerEpochMintNotSent,不把它当成脚本回复。
//
// 只在设置 MMORPG_TEST_REDIS_ADDR(host:port)时运行,未设就跳过。必须指向**专用**的一次性单实例 Redis(≥ 7.2,
// --appendonly yes --appendfsync everysec,没有副本,不设口令):用例按复制偏移断言,别的客户端同时写会让它们误判。
// 键都带本次运行的随机 tag,用例结束时删掉。
//
// 偏移断言只放在 WAITAOF 不会阻塞的地方:单实例上每一次阻塞的 WAITAOF 都会向「副本」发 GETACK,没有副本时 Redis 照样把
// master_repl_offset 加 1(replication.c replicationFeedSlaves 的无副本分支),会掩盖「这次脚本有没有写」。
// ---------------------------------------------------------------------------

const (
	// realSetScript:把 KEYS[1] 设成 ARGV[1] 并回它(铸造的形状:一次写 + 回整数)。
	realSetScript = `redis.call('SET', KEYS[1], ARGV[1])
return tonumber(ARGV[1])`
	// realReplayScript:键已是 ARGV[1] 时把原值写回一次并回负值(铸造重放识别、两段回执确认脚本的形状),否则同 realSetScript。
	realReplayScript = `local cur = redis.call('GET', KEYS[1])
if cur == ARGV[1] then
    redis.call('SET', KEYS[1], cur)
    return -tonumber(cur)
end
redis.call('SET', KEYS[1], ARGV[1])
return tonumber(ARGV[1])`
	// realReadOnlyReplayScript:同 realReplayScript,但识别分支只读不写 —— 用来证明不写回时偏移不动。
	realReadOnlyReplayScript = `if redis.call('GET', KEYS[1]) == ARGV[1] then
    return -tonumber(ARGV[1])
end
redis.call('SET', KEYS[1], ARGV[1])
return tonumber(ARGV[1])`
	// realCasScript:键是 ARGV[1] 才改成 ARGV[2] 并回 1,否则回 0、一个字节都不写(CAS 不过的分支)。
	realCasScript = `if redis.call('GET', KEYS[1]) ~= ARGV[1] then
    return 0
end
redis.call('SET', KEYS[1], ARGV[2])
return 1`
)

// realRedisAddrOrSkip 读 MMORPG_TEST_REDIS_ADDR,未设就跳过本用例。
func realRedisAddrOrSkip(t *testing.T) string {
	t.Helper()
	addr := strings.TrimSpace(os.Getenv("MMORPG_TEST_REDIS_ADDR"))
	if addr == "" {
		t.Skip("需要专用 MMORPG_TEST_REDIS_ADDR(开启 AOF、没有副本的一次性单实例 Redis ≥ 7.2)")
	}
	return addr
}

// newRealRedisInspector 是断言用的普通客户端(不经铸造通道):读复制偏移、读键、清理。先核对测试实例的形态。
func newRealRedisInspector(t *testing.T, addr string) *goredis.Client {
	t.Helper()
	rdb := goredis.NewClient(&goredis.Options{Addr: addr, PoolSize: 2, MaxRetries: -1, DialTimeout: time.Second,
		ReadTimeout: 3 * time.Second, WriteTimeout: time.Second, ContextTimeoutEnabled: true})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	persistence, err := rdb.Info(ctx, "persistence").Result()
	require.NoError(t, err)
	require.Contains(t, persistence, "aof_enabled:1", "测试实例必须开启 AOF(--appendonly yes)")
	replication, err := rdb.Info(ctx, "replication").Result()
	require.NoError(t, err)
	require.Contains(t, replication, "role:master", "测试实例必须是主节点")
	require.Contains(t, replication, "connected_slaves:0", "测试实例不能带副本:用例按单实例的份数与复制偏移断言")
	return rdb
}

// masterReplOffset 读 INFO replication 的 master_repl_offset。
func masterReplOffset(t *testing.T, rdb *goredis.Client) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	info, err := rdb.Info(ctx, "replication").Result()
	require.NoError(t, err)
	for _, line := range strings.Split(info, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "master_repl_offset:"); ok {
			n, parseErr := strconv.ParseInt(v, 10, 64)
			require.NoError(t, parseErr)
			return n
		}
	}
	t.Fatalf("INFO replication has no master_repl_offset: %q", info)
	return 0
}

// realRedisKey 给本用例一个带随机 tag 的键(铸造通道发送前按玩家域规则要求键带 hash tag),用例结束时删掉。
func realRedisKey(t *testing.T, rdb *goredis.Client) string {
	t.Helper()
	key := fmt.Sprintf("codex:owner-epoch-mint:{%016x}:epoch", rand.Uint64())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = rdb.Del(ctx, key).Err()
	})
	return key
}

// newRealMinter 按给定份数与超时建生产形态的铸造通道(NewRedisOwnerEpochMinter,真实墙钟)。
func newRealMinter(t *testing.T, addr string, numLocal, numReplicas, timeoutMs int) *RedisOwnerEpochMinter {
	t.Helper()
	m, err := NewRedisOwnerEpochMinter(playerredis.Config{Addrs: []string{addr}},
		config.OwnerEpochMintConfig{WaitAOFNumLocal: intPtr(numLocal), WaitAOFNumReplicas: intPtr(numReplicas), WaitAOFTimeoutMs: timeoutMs},
		WallClockMs)
	require.NoError(t, err)
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// (a) 单实例 1 0:EVAL 与 WAITAOF 在同一条 pipeline,本地刷盘确认之后回 Confirmed;回复就是脚本回复,写确实传播了。
func TestRealRedisMintConfirmedByLocalFsync(t *testing.T) {
	addr := realRedisAddrOrSkip(t)
	rdb := newRealRedisInspector(t, addr)
	key := realRedisKey(t, rdb)
	m := newRealMinter(t, addr, 1, 0, 1500)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	before := masterReplOffset(t, rdb)
	res, err := m.EvalAndConfirm(ctx, realSetScript, []string{key}, "1800000000123")
	require.NoError(t, err)
	assert.Equal(t, int64(1800000000123), res.Reply)
	assert.Equal(t, MintDurabilityConfirmed, res.Durability, "durable_err=%v", res.DurableErr)
	assert.Equal(t, 1, res.Attempts)
	assert.Greater(t, masterReplOffset(t, rdb), before, "铸造的写必须传播,WAITAOF 才有偏移可等")
	v, err := rdb.Get(ctx, key).Result()
	require.NoError(t, err)
	assert.Equal(t, "1800000000123", v)
}

// (b) 单实例要 1 个副本确认(1 1):WAITAOF 等满超时、回 [1, 0],结论 NotDurable;耗时受 WAITAOF 的超时约束,不会挂住。
// 写本身已经执行,调用方据此按推送失败回滚。
func TestRealRedisMintNotDurableWithoutReplica(t *testing.T) {
	addr := realRedisAddrOrSkip(t)
	rdb := newRealRedisInspector(t, addr)
	key := realRedisKey(t, rdb)
	const timeoutMs = 300
	m := newRealMinter(t, addr, 1, 1, timeoutMs)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	started := time.Now()
	res, err := m.EvalAndConfirm(ctx, realSetScript, []string{key}, "7")
	elapsed := time.Since(started)
	require.NoError(t, err)
	assert.Equal(t, int64(7), res.Reply)
	assert.Equal(t, MintDurabilityNotDurable, res.Durability, "durable_err=%v", res.DurableErr)
	require.Error(t, res.DurableErr)
	assert.GreaterOrEqual(t, elapsed, timeoutMs*time.Millisecond/2, "副本份数满足不了,WAITAOF 应等到超时")
	assert.Less(t, elapsed, timeoutMs*time.Millisecond+time.Second, "等待受 WAITAOF 的超时约束")
	v, err := rdb.Get(ctx, key).Result()
	require.NoError(t, err)
	assert.Equal(t, "7", v, "写已执行,只是没确认落盘")
}

// (c) 启动探针:1 0 通过;单实例配 1 1 必须失败(拓扑里没有副本,运行期每一次铸造都会判 not_durable)。
func TestRealRedisProbe(t *testing.T) {
	addr := realRedisAddrOrSkip(t)
	newRealRedisInspector(t, addr)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	require.NoError(t, newRealMinter(t, addr, 1, 0, 1500).Probe(ctx))
	require.Error(t, newRealMinter(t, addr, 1, 1, 300).Probe(ctx))
}

// (d) 写回原值:别的连接先写(模拟首发已生效、应答丢了),铸造通道上只把原值写回一次,复制偏移照样前进,WAITAOF 确认的是写回
// 那次的偏移 —— 它排在首发之后,所以首发也已落盘。只读的识别分支不推进偏移:连接的 woff 停在原处,WAITAOF 立即回满份数,
// 确认不到首发那次写(这正是重放识别与两段回执确认脚本都要写回原值的原因)。
func TestRealRedisWriteBackAdvancesOffsetForWaitAOF(t *testing.T) {
	addr := realRedisAddrOrSkip(t)
	rdb := newRealRedisInspector(t, addr)
	key := realRedisKey(t, rdb)
	m := newRealMinter(t, addr, 1, 0, 1500)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	require.NoError(t, rdb.Set(ctx, key, "42", 0).Err(), "首发:另一条连接上的写")
	afterFirst := masterReplOffset(t, rdb)

	res, err := m.EvalAndConfirm(ctx, realReplayScript, []string{key}, "42")
	require.NoError(t, err)
	assert.Equal(t, int64(-42), res.Reply, "识别分支命中")
	assert.Equal(t, MintDurabilityConfirmed, res.Durability, "durable_err=%v", res.DurableErr)
	afterWriteBack := masterReplOffset(t, rdb)
	assert.Greater(t, afterWriteBack, afterFirst, "值不变的 SET 也要传播,WAITAOF 才有偏移可等")

	// 上面的 WAITAOF 已确认写回那次落盘,这条连接再发 WAITAOF 不会阻塞,也就不会因 GETACK 推进偏移。
	res, err = m.EvalAndConfirm(ctx, realReadOnlyReplayScript, []string{key}, "42")
	require.NoError(t, err)
	assert.Equal(t, int64(-42), res.Reply)
	assert.Equal(t, afterWriteBack, masterReplOffset(t, rdb), "只读分支不传播:这时的 WAITAOF 确认不了任何东西")
}

// (e) CAS 不过的分支(回 0)一个字节都不写:复制偏移不动。没写过东西的新连接上 WAITAOF 立即「满足」—— 这份确认什么都没证明,
// 铸造回 0 / -1、确认脚本复核不过时调用方都不用它。
func TestRealRedisCasMissWritesNothing(t *testing.T) {
	addr := realRedisAddrOrSkip(t)
	rdb := newRealRedisInspector(t, addr)
	key := realRedisKey(t, rdb)
	m := newRealMinter(t, addr, 1, 0, 1500)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	require.NoError(t, rdb.Set(ctx, key, "5", 0).Err())
	before := masterReplOffset(t, rdb)
	res, err := m.EvalAndConfirm(ctx, realCasScript, []string{key}, "4", "6")
	require.NoError(t, err)
	assert.Equal(t, int64(0), res.Reply)
	assert.Equal(t, MintDurabilityConfirmed, res.Durability, "新连接的 woff 为 0,WAITAOF 立即满足")
	assert.Equal(t, before, masterReplOffset(t, rdb), "CAS 不过一个字节都不写")
	v, err := rdb.Get(ctx, key).Result()
	require.NoError(t, err)
	assert.Equal(t, "5", v)
}

// (f) 池被占满:唯一一条连接正挂在等不满份数的 WAITAOF 上,第二次调用取不到连接。go-redis 只从 Pipelined 返回 ErrPoolTimeout、
// 不给命令设错误或值;通道必须回 ErrOwnerEpochMintNotSent(一个字节都没写),不能交回「Reply = nil、err = nil」。
func TestRealRedisPoolExhaustionIsNotSent(t *testing.T) {
	addr := realRedisAddrOrSkip(t)
	rdb := newRealRedisInspector(t, addr)
	busyKey := realRedisKey(t, rdb)
	idleKey := realRedisKey(t, rdb)
	ack := mintAOFAck{numLocal: 1, numReplicas: 1, timeout: 1500 * time.Millisecond}
	conf := ownerEpochMintClientConfig(playerredis.Config{Addrs: []string{addr}}, ack)
	conf.PoolSizePerNode = 1
	conf.PoolTimeoutMs = 200
	client, err := playerredis.NewPinned(conf)
	require.NoError(t, err)
	m := &RedisOwnerEpochMinter{client: client, conf: conf, ack: ack, nowMs: WallClockMs}
	t.Cleanup(func() { _ = m.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// 单实例要 1 个副本:WAITAOF 挂满 1500ms,这段时间唯一的连接被占着。
		_, _ = m.EvalAndConfirm(ctx, realSetScript, []string{busyKey}, "1")
	}()
	require.Eventually(t, func() bool {
		stats := client.PoolStats()
		return stats.TotalConns == 1 && stats.IdleConns == 0
	}, 2*time.Second, 5*time.Millisecond, "第一条 pipeline 应占住唯一的连接")

	res, err := m.EvalAndConfirm(ctx, realSetScript, []string{idleKey}, "2")
	require.ErrorIs(t, err, ErrOwnerEpochMintNotSent)
	assert.ErrorIs(t, err, goredis.ErrPoolTimeout, "原因留在包装链里")
	assert.Nil(t, res.Reply)
	assert.Equal(t, 0, res.Attempts)
	exists, err := rdb.Exists(ctx, idleKey).Result()
	require.NoError(t, err)
	assert.Zero(t, exists, "取不到连接的那次一个字节都没写")
	wg.Wait()
}
