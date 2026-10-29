//go:build integration

package routing

// 集成测试:合服相关的映射语义打**真 Redis**(默认 etc/data_service.yaml 的
// MappingRedis.Host,本地 127.0.0.1:6379)。
//
// 跑法:  go test -tags=integration ./internal/routing/...
// 覆盖:  DATA_SERVICE_IT_REDIS_HOST / DATA_SERVICE_IT_YAML
//
// 为什么单测(miniredis)不够:RegisterPlayerZone 的 SETNX 走的是一段 Lua
// (setPlayerZoneIfAbsentScript),而 miniredis 的 Lua 是 gopher-lua 的仿真实现,
// `redis.call('SET', k, v, 'NX')` 写入失败时的返回值(false / nil)与真 Redis
// 未必一致 —— 恰恰是那个分支决定了"绝不覆盖"这条铁律成不成立。
//
// 库的选择:**不能选**。go-zero 的 redis.RedisConf 没有 DB 字段(只有 Host/Type/
// User/Pass/Tls/NonBlock/PingTimeout),所以 data_service 的 mapping Redis 恒在 DB 0。
// TestMappingRedisEffectiveDBIsZero 把这条钉死,因为 tools/merge_zone 必须连同一个库。
// 用例因此直接在 DB 0 上跑,但只在 itKeyPatterns 里的前缀完全为空时才跑(否则 Skip),
// 并且只删自己建的键 —— 绝不 FLUSHDB。本地 compose 的 Redis 就是服务在用的 mapping Redis,
// 跑过服务就有键:复验时用一次性的 `redis:7.2 --appendonly yes`(规格 h 节第 4 步)。
//
// 映射持久化第 1a 批(docs/design/player-route-mapping-durability-proposal.md)加了四个用例:
// 发布 pipeline 是否同一条连接(Q3 的实测证据)、WAITAOF 的真实回复、发布脚本在真 Redis 上与 miniredis 同一组向量的结果,
// 以及 db_first 端到端(还要 DATA_SERVICE_IT_MYSQL_HOST,缺任一环境变量就 Skip)。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"data_service/internal/config"
	"data_service/internal/store"
	"data_service/internal/store/storetest"

	"shared/placement"

	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

const (
	// envRedisHost 覆盖 yaml 的 MappingRedis.Host。
	envRedisHost = "DATA_SERVICE_IT_REDIS_HOST"
	// envYaml 覆盖 data_service.yaml 路径。
	envYaml = "DATA_SERVICE_IT_YAML"

	itRedisTimeout = 3 * time.Second
)

// itKeyPatterns 是用例会碰的所有键模式:跑前要求它们为空,跑后按模式清理。
var itKeyPatterns = []string{"player:zone:*", "merge:in_progress:*", "player:placement:*", "player:maprev:*"}

// newIntegrationRouter 接一个指向真 Redis 的 Router(DB 恒 0,见文件顶部),RouteStore 段取默认(redis 模式)。
func newIntegrationRouter(t *testing.T) (*Router, *goredis.Client) {
	t.Helper()
	return newIntegrationRouterWith(t, config.RouteStoreConfig{})
}

// newIntegrationRouterWith 同上,RouteStore 段与 Router 选项由调用方给(db_first 用例用)。
func newIntegrationRouterWith(t *testing.T, routeCfg config.RouteStoreConfig, opts ...RouterOption) (*Router, *goredis.Client) {
	t.Helper()

	host := resolveRedisHost(t)
	raw := goredis.NewClient(&goredis.Options{Addr: host, DB: 0})
	ctx, cancel := context.WithTimeout(context.Background(), itRedisTimeout)
	defer cancel()
	if err := raw.Ping(ctx).Err(); err != nil {
		raw.Close()
		t.Skipf("Redis %s 不可达,跳过集成测试: %v", host, err)
	}
	for _, pattern := range itKeyPatterns {
		keys, _, err := raw.Scan(ctx, 0, pattern, 50).Result()
		if err != nil {
			raw.Close()
			t.Skipf("Redis %s SCAN %s 失败: %v", host, pattern, err)
		}
		if len(keys) > 0 {
			raw.Close()
			t.Skipf("Redis %s DB0 已有 %s 键(%v ...):本用例要独占这些前缀,先清干净或换一个空实例",
				host, pattern, keys)
		}
	}
	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), itRedisTimeout)
		defer cleanCancel()
		// 只删本用例前缀下的键,绝不 FLUSHDB —— DB0 上还有别的开发数据。
		for _, pattern := range itKeyPatterns {
			iter := raw.Scan(cleanCtx, 0, pattern, 200).Iterator()
			var keys []string
			for iter.Next(cleanCtx) {
				keys = append(keys, iter.Val())
			}
			if len(keys) > 0 {
				if err := raw.Del(cleanCtx, keys...).Err(); err != nil {
					t.Errorf("清理 %s 失败(请手工删): %v", pattern, err)
				}
			}
		}
		raw.Close()
	})

	r := NewRouter(config.Config{
		MappingRedis:     redis.RedisConf{Host: host, Type: "node"},
		DevRedis:         config.DevRedisConfig{Host: host, DB: 0},
		PlayerLockTTLSec: 3,
		RouteStore:       routeCfg,
	}, opts...)
	t.Cleanup(r.Close)
	return r, raw
}

// TestMappingRedisEffectiveDBIsZero 钉死一条**跨仓契约**:mapping Redis 恒在 DB 0。
//
// go-zero 的 redis.RedisConf 没有 DB 字段,yaml 里写 `DB: 15` 会被 unmarshaler
// 静默忽略。这条不是学术问题:tools/merge_zone 的 -mapping-redis-db 默认 0、
// 帮助文本却写着 "often not 0",运维照着旧 yaml 填 15,合服标记与 remap 就会
// 全部打进一个 data_service 从来不看的库 —— 闸门失效、remap 一个玩家都改不到,
// 却报告"成功"。这个用例就是让那种漂移在 CI 里当场红掉。
func TestMappingRedisEffectiveDBIsZero(t *testing.T) {
	host := resolveRedisHost(t)
	probe := goredis.NewClient(&goredis.Options{Addr: host, DB: 0})
	ctx, cancel := context.WithTimeout(context.Background(), itRedisTimeout)
	defer cancel()
	if err := probe.Ping(ctx).Err(); err != nil {
		probe.Close()
		t.Skipf("Redis %s 不可达: %v", host, err)
	}
	defer probe.Close()

	// 故意像旧 yaml 那样"想要"另一个库:RedisConf 根本没有承载它的字段。
	r := NewRouter(config.Config{
		MappingRedis:     redis.RedisConf{Host: host, Type: "node"},
		Regions:          []config.RegionConfig{{Id: 1, Zones: []uint32{1}, Redis: config.RedisClusterConfig{Addrs: []string{host}}}},
		PlayerLockTTLSec: 3,
	})
	defer r.Close()

	const key = "zz:it:mapping_db_probe"
	require.NoError(t, r.mappingRedis.SetexCtx(ctx, key, "1", 30))
	defer probe.Del(ctx, key)

	n, err := probe.Exists(ctx, key).Result()
	require.NoError(t, err)
	assert.EqualValues(t, 1, n,
		"mapping Redis 必须落在 DB 0;若这条失败说明 go-zero 支持了 DB 选择,"+
			"届时 tools/merge_zone 的 -mapping-redis-db 与本服务的配置必须同时更新")
}

// TestRegisterPlayerZone_RealRedisNeverOverwrites 是 SETNX 那段 Lua 的真 Redis 验证。
func TestRegisterPlayerZone_RealRedisNeverOverwrites(t *testing.T) {
	r, raw := newIntegrationRouter(t)
	ctx := context.Background()

	require.NoError(t, r.RegisterPlayerZone(ctx, 90001, 3, 0))
	val, err := raw.Get(ctx, "player:zone:90001").Result()
	require.NoError(t, err)
	assert.Equal(t, "3", val)

	// 同 zone 幂等(CreatePlayer 允许重试)
	require.NoError(t, r.RegisterPlayerZone(ctx, 90001, 3, 0))

	// 异 zone 拒绝,且零变更
	err = r.RegisterPlayerZone(ctx, 90001, 9, 0)
	require.Error(t, err)
	var conflict *HomeZoneConflictError
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, uint32(3), conflict.Existing)
	val, err = raw.Get(ctx, "player:zone:90001").Result()
	require.NoError(t, err)
	assert.Equal(t, "3", val, "真 Redis 上也必须一个字节都没改")
}

// TestMergeFence_RealRedisBlocksRegister 端到端跑一遍合服闸门对登记的封锁:
// 立标记 → 建号被拒(既有映射不动,别的 zone 照常)→ 清标记 → 恢复放行。
// 原 TestMergeFence_RealRedisBlocksRegisterAndGatesRemap 里 remap 的几步随 RemapHomeZoneForMerge 一起删除
// (2026-10-12,proposal §4.3 的 W2);改映射只走 tools/merge_zone。
func TestMergeFence_RealRedisBlocksRegister(t *testing.T) {
	r, raw := newIntegrationRouter(t)
	ctx := context.Background()

	require.NoError(t, r.RegisterPlayerZone(ctx, 90010, 31, 0))
	require.NoError(t, r.RegisterPlayerZone(ctx, 90011, 31, 0))
	require.NoError(t, r.RegisterPlayerZone(ctx, 90012, 32, 0))

	// 合服工具立标记:值是 JSON,TTL 长于整次合服。
	require.NoError(t, raw.Set(ctx, MergeFenceKey(31),
		`{"started_at":1757000000,"source":31,"target":32}`, 2*time.Hour).Err())

	fenced, err := r.IsMergeInProgress(ctx, 31)
	require.NoError(t, err)
	assert.True(t, fenced)

	// 封锁期间不接受新映射 —— 否则它会跑在合服清单之外,合服完仍指向源 zone。
	require.ErrorIs(t, r.RegisterPlayerZone(ctx, 90013, 31, 0), ErrZoneMergeInProgress)
	_, err = r.GetPlayerHomeZone(ctx, 90013)
	require.ErrorIs(t, err, ErrHomeZoneNotMapped, "被闸门拒绝的登记不许留下半条映射")
	// 闸门按 zone,不是全局的。
	require.NoError(t, r.RegisterPlayerZone(ctx, 90014, 32, 0))
	for pid, want := range map[uint64]uint32{90010: 31, 90011: 31, 90012: 32, 90014: 32} {
		zone, err := r.GetPlayerHomeZone(ctx, pid)
		require.NoError(t, err)
		assert.Equal(t, want, zone, "player %d", pid)
	}

	// 工具跑完删标记(TTL 只是兜底);之后源 zone 恢复放行。
	require.NoError(t, raw.Del(ctx, MergeFenceKey(31)).Err())
	require.NoError(t, r.RegisterPlayerZone(ctx, 90013, 31, 0))
}

// TestPlacementScripts_RealRedis 是两段落点相关 Lua 的真 Redis 验证:miniredis 的 gopher-lua
// 仿真里「不带 storage_id 时 KEYS[3] 为 nil」「pcall 回整数」「SET NX 失败回 false」「EXISTS 回整数 1」
// 几处都可能与真 Redis 不同,而它们恰好决定「home 已存在时落点不被写」「围栏存在时 merging=true」
// 与「提交点复核围栏时两键都不写」这几条语义。
func TestPlacementScripts_RealRedis(t *testing.T) {
	r, raw := newIntegrationRouter(t)
	ctx := context.Background()

	// 带 storage_id:两键一起写下。
	require.NoError(t, r.RegisterPlayerZone(ctx, 90101, 41, placement.DefaultGlobalStorageID))
	val, err := raw.Get(ctx, placement.Key(90101)).Result()
	require.NoError(t, err)
	assert.Equal(t, placement.StableValue(placement.DefaultGlobalStorageID, 1), val)

	// 不带 storage_id:只写 home。
	require.NoError(t, r.RegisterPlayerZone(ctx, 90102, 41, 0))
	n, err := raw.Exists(ctx, placement.Key(90102)).Result()
	require.NoError(t, err)
	assert.EqualValues(t, 0, n)

	// home 已存在:同值重试与冲突都不写落点。
	require.NoError(t, r.RegisterPlayerZone(ctx, 90102, 41, placement.DefaultGlobalStorageID))
	var conflict *HomeZoneConflictError
	require.ErrorAs(t, r.RegisterPlayerZone(ctx, 90102, 42, placement.DefaultGlobalStorageID), &conflict)
	n, err = raw.Exists(ctx, placement.Key(90102)).Result()
	require.NoError(t, err)
	assert.EqualValues(t, 0, n, "真 Redis 上 home 已存在时也不许写落点")

	// 围栏:home 与 merging 同一段脚本读出。
	zone, merging, err := r.GetPlayerHomeZoneAndMergeFence(ctx, 90101)
	require.NoError(t, err)
	assert.Equal(t, uint32(41), zone)
	assert.False(t, merging)
	require.NoError(t, raw.Set(ctx, MergeFenceKey(41), `{"started_at":1757000000}`, time.Minute).Err())
	zone, merging, err = r.GetPlayerHomeZoneAndMergeFence(ctx, 90101)
	require.NoError(t, err)
	assert.Equal(t, uint32(41), zone)
	assert.True(t, merging)

	// 提交点复核:绕过预检直接打写入脚本(模拟「预检之后才立围栏」),围栏内两键都不写。
	require.ErrorIs(t, r.registerPlayerZoneFenced(ctx, 90103, 41, placement.DefaultGlobalStorageID), ErrZoneMergeInProgress)
	n, err = raw.Exists(ctx, mappingKey(90103), placement.Key(90103)).Result()
	require.NoError(t, err)
	assert.EqualValues(t, 0, n, "真 Redis 上围栏内提交点也不许写 player:zone / player:placement")

	_, _, err = r.GetPlayerHomeZoneAndMergeFence(ctx, 90199)
	require.ErrorIs(t, err, ErrHomeZoneNotMapped)
}

// TestGetPlayerHomeZone_RealRedisMissingIsTyped:真 Redis 上"键不存在"同样只归到
// ErrHomeZoneNotMapped,不会与连接错误混在一起。
func TestGetPlayerHomeZone_RealRedisMissingIsTyped(t *testing.T) {
	r, _ := newIntegrationRouter(t)

	_, err := r.GetPlayerHomeZone(context.Background(), 90099)
	require.ErrorIs(t, err, ErrHomeZoneNotMapped)
	assert.Contains(t, err.Error(), "no home zone mapping")
}

// ── 映射持久化第 1a 批:发布 pipeline 与 db_first 端到端 ─────────────────

// TestPublishRoute_RealRedisSameConnection 是规格 Q3 的实测证据:经**实际发布用的客户端**(r.publishRedis 的 Pipelined)发
// [CLIENT ID, EVAL, WAITAOF, CLIENT ID],两次 CLIENT ID 必须相等 —— WAITAOF 只确认**本连接**之前的写,
// 发布脚本与 WAITAOF 不在同一条连接上,落盘确认就是假的。
// 2026-10-21(A2-1):发布已改用独立的 go-redis 客户端(publish_redis.go),不再经 go-zero 的 PipelinedCtx;本用例只验同一连接,
// 所以 Router 取 db_first(redis 模式不建发布客户端)。用例名与 PASS 口径不变。
// WAITAOF 这里发 0 0(不要求落盘),只看连接身份;真实的落盘回复由 TestPublishRoute_RealRedisWaitAOF 验。
func TestPublishRoute_RealRedisSameConnection(t *testing.T) {
	r, _ := newIntegrationRouterWith(t, config.RouteStoreConfig{Mode: config.RouteStoreModeDBFirst})
	ctx := context.Background()
	require.NotNil(t, r.publishRedis, "db_first must build the publish client")

	var firstID, evalCmd, waitCmd, secondID *goredis.Cmd
	_, pipeErr := r.publishRedis.Pipelined(ctx, func(p goredis.Pipeliner) error {
		firstID = p.Do(ctx, "CLIENT", "ID")
		evalCmd = p.Eval(ctx, placement.PublishRouteScript, placement.PublishRouteKeys(90301, 51), "1", "51", "")
		waitCmd = p.Do(ctx, "WAITAOF", 0, 0, 100)
		secondID = p.Do(ctx, "CLIENT", "ID")
		return nil
	})
	if pipeErr != nil {
		t.Logf("pipeline returned %v (per-command results below are what matter)", pipeErr)
	}
	require.NoError(t, evalCmd.Err())
	if err := waitCmd.Err(); err != nil {
		t.Logf("WAITAOF 0 0 100: %v (Redis < 7.2? the connection identity check below still applies)", err)
	}
	first, err := firstID.Int64()
	require.NoError(t, err)
	second, err := secondID.Int64()
	require.NoError(t, err)
	assert.Equal(t, first, second, "the publish client's Pipelined must run the whole pipeline on one connection")
}

// TestPublishRoute_RealRedisWaitAOF:WAITAOF 1 0 的真实回复是 [1, 0](本地 fsync 已确认、无副本),
// 且整条发布路径(publishRoute,默认配置 WAITAOF 1 0)在真 Redis 上成功。
// appendonly 关着(CONFIG GET appendonly = no)或 Redis < 7.2(unknown command)时 Skip —— 复验必须用
// `redis:7.2 --appendonly yes` 跑出 PASS(规格 h 节第 4 步),Skip 要如实报成"未验证"。
// 2026-10-21(A2-1):Router 取 db_first(publishRoute 只用发布客户端,redis 模式不建它,直接调会 fail-closed),
// 前半段的手工 pipeline 也走 r.publishRedis。
func TestPublishRoute_RealRedisWaitAOF(t *testing.T) {
	r, _ := newIntegrationRouterWith(t, config.RouteStoreConfig{Mode: config.RouteStoreModeDBFirst})
	ctx := context.Background()
	require.NotNil(t, r.publishRedis, "db_first must build the publish client")
	on, err := redisAppendOnly(ctx, resolveRedisHost(t))
	if err != nil {
		t.Skipf("CONFIG GET appendonly failed, cannot tell whether WAITAOF can work: %v", err)
	}
	if !on {
		t.Skip("appendonly is off on this Redis; rerun against `redis:7.2 --appendonly yes`")
	}

	var evalCmd, waitCmd *goredis.Cmd
	_, _ = r.publishRedis.Pipelined(ctx, func(p goredis.Pipeliner) error {
		evalCmd = p.Eval(ctx, placement.PublishRouteScript, placement.PublishRouteKeys(90311, 52), "1", "52", "")
		waitCmd = p.Do(ctx, "WAITAOF", 1, 0, 1500)
		return nil
	})
	require.NoError(t, evalCmd.Err())
	acks, err := waitCmd.Slice()
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "unknown command") {
		t.Skipf("Redis < 7.2 has no WAITAOF: %v", err)
	}
	require.NoError(t, err)
	assert.Equal(t, []any{int64(1), int64(0)}, acks)
	require.NoError(t, checkWaitAOFReply(acks, 1, 0))

	require.True(t, r.publish.waitAOF, "the default RouteStore config must wait for local AOF")
	require.NoError(t, r.publishRoute(ctx, store.RouteRow{PlayerID: 90312, HomeZone: 52, Rev: 1}))
	assertRedisRouteKeys(t, ctx, r, 90312, [3]string{"52", "", "1"})
}

// publishScriptVector 是发布脚本的一条真 Redis 对照向量(与 route_publish_test.go 里 8 个 miniredis 用例同一组场景):
// preset 按 [zone, placement, maprev] 预置("" = 不写);ttl 给预置的 maprev 加 TTL(观察 touch);
// fenceKey 决定传不传第 4 把键;fenced 决定那把围栏键是否存在;want / wantErr 二选一;
// wantAfter 是执行后的三键;wantTouch 是否期望 maprev 被原值写回(TTL 被清掉)。
type publishScriptVector struct {
	name      string
	preset    [3]string
	ttl       bool
	fenceKey  bool
	fenced    bool
	rev       string
	home      string
	plc       string
	want      placement.PublishOutcome
	wantErr   string
	wantAfter [3]string
	wantTouch bool
}

// publishScriptVectors 逐条对应 route_publish_test.go 的 TestPublishRouteScript_* 1–8(注释里标了出处)。
// miniredis 的 Lua 是 gopher-lua 仿真:GET 缺席回 false、string.byte、错误回复的形状都可能与真 Redis 不同,
// 而这些恰好决定 set-if-newer、遗留值守卫与围栏复核成不成立。改脚本时两边一起改。
var publishScriptVectors = []publishScriptVector{
	// AppliesWhenAbsent
	{name: "absent applies", fenceKey: true, rev: "1", home: "5", plc: "1000000:1",
		want: placement.PublishApplied, wantAfter: [3]string{"5", "1000000:1", "1"}},
	// LengthBeforeLexicographic
	{name: "cached 9 send 10 applies", preset: [3]string{"5", "", "9"}, rev: "10", home: "5",
		want: placement.PublishApplied, wantAfter: [3]string{"5", "", "10"}},
	{name: "cached 10 send 9 superseded", preset: [3]string{"5", "", "10"}, rev: "9", home: "5",
		want: placement.PublishSuperseded, wantAfter: [3]string{"5", "", "10"}},
	// EqualSameIsConfirmed
	{name: "equal rev same value confirmed with touch", preset: [3]string{"5", "7:3", "4"}, ttl: true, fenceKey: true,
		rev: "4", home: "5", plc: "7:3", want: placement.PublishConfirmed, wantAfter: [3]string{"5", "7:3", "4"}, wantTouch: true},
	// EqualDifferentIsConflict
	{name: "equal rev different placement conflict", preset: [3]string{"5", "1000000:1", "5"}, rev: "5", home: "5", plc: "1000000:2",
		want: placement.PublishConflict, wantAfter: [3]string{"5", "1000000:1", "5"}},
	{name: "equal rev different home conflict", preset: [3]string{"5", "1000000:1", "5"}, rev: "5", home: "6", plc: "1000000:1",
		want: placement.PublishConflict, wantAfter: [3]string{"5", "1000000:1", "5"}},
	// EmptyArgDeletes
	{name: "empty placement deletes", preset: [3]string{"5", "7:1", "1"}, rev: "2", home: "5",
		want: placement.PublishApplied, wantAfter: [3]string{"5", "", "2"}},
	{name: "tombstone deletes both", preset: [3]string{"5", "", "2"}, rev: "3",
		want: placement.PublishApplied, wantAfter: [3]string{"", "", "3"}},
	// RejectsBadRevAndMalformedMapRev
	{name: "bad rev leading zero", preset: [3]string{"5", "7:1", "3"}, rev: "01", home: "5", plc: "7:1",
		wantErr: "bad rev argument", wantAfter: [3]string{"5", "7:1", "3"}},
	{name: "bad rev 21 digits", preset: [3]string{"5", "7:1", "3"}, rev: "123456789012345678901", home: "5", plc: "7:1",
		wantErr: "bad rev argument", wantAfter: [3]string{"5", "7:1", "3"}},
	{name: "malformed maprev", preset: [3]string{"5", "7:1", "abc"}, rev: "4", home: "5", plc: "7:1",
		wantErr: "malformed maprev", wantAfter: [3]string{"5", "7:1", "abc"}},
	// LegacyValuesWithoutMapRev
	{name: "legacy zone differs", preset: [3]string{"4", "", ""}, rev: "1", home: "5",
		want: placement.PublishLegacyConflict, wantAfter: [3]string{"4", "", ""}},
	{name: "legacy placement present sent empty", preset: [3]string{"", "7:1", ""}, rev: "1", home: "5",
		want: placement.PublishLegacyConflict, wantAfter: [3]string{"", "7:1", ""}},
	{name: "legacy placement differs", preset: [3]string{"5", "7:1", ""}, rev: "1", home: "5", plc: "7:2",
		want: placement.PublishLegacyConflict, wantAfter: [3]string{"5", "7:1", ""}},
	{name: "legacy values same", preset: [3]string{"5", "7:1", ""}, rev: "1", home: "5", plc: "7:1",
		want: placement.PublishApplied, wantAfter: [3]string{"5", "7:1", "1"}},
	{name: "only legacy placement same", preset: [3]string{"", "7:1", ""}, rev: "1", home: "5", plc: "7:1",
		want: placement.PublishApplied, wantAfter: [3]string{"5", "7:1", "1"}},
	// FenceKeyBlocksEverything
	{name: "fence blocks absent player", fenceKey: true, fenced: true, rev: "1", home: "5",
		want: placement.PublishFenced, wantAfter: [3]string{}},
	{name: "fence blocks without touch", preset: [3]string{"5", "", "3"}, ttl: true, fenceKey: true, fenced: true, rev: "3", home: "5",
		want: placement.PublishFenced, wantAfter: [3]string{"5", "", "3"}},
	{name: "fence ignored with three keys", fenced: true, rev: "1", home: "5",
		want: placement.PublishApplied, wantAfter: [3]string{"5", "", "1"}},
}

// TestPublishRouteScript_RealRedisParity 在真 Redis 上跑 publishScriptVectors。
func TestPublishRouteScript_RealRedisParity(t *testing.T) {
	r, raw := newIntegrationRouter(t)
	ctx := context.Background()

	for i, v := range publishScriptVectors {
		t.Run(v.name, func(t *testing.T) {
			playerID := uint64(90400 + i)
			fenceZone := uint32(70 + i)
			keys := []string{placement.HomeZoneKey(playerID), placement.Key(playerID), placement.MapRevKey(playerID)}
			for j, val := range v.preset {
				if val != "" {
					require.NoError(t, raw.Set(ctx, keys[j], val, 0).Err())
				}
			}
			if v.ttl {
				require.NoError(t, raw.Expire(ctx, keys[2], time.Hour).Err())
			}
			if v.fenced {
				require.NoError(t, raw.Set(ctx, MergeFenceKey(fenceZone), `{"started_at":1757000000}`, time.Minute).Err())
			}
			publishKeys := placement.PublishRouteKeys(playerID, 0)
			if v.fenceKey {
				publishKeys = placement.PublishRouteKeys(playerID, fenceZone)
			}

			res, err := r.mappingRedis.EvalCtx(ctx, placement.PublishRouteScript, publishKeys, v.rev, v.home, v.plc)
			if v.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), v.wantErr)
			} else {
				require.NoError(t, err)
				reply, err := placement.ClassifyPublishReply(res, v.rev, v.home, v.plc)
				require.NoError(t, err, "raw reply %v", res)
				assert.Equal(t, v.want, reply.Outcome)
			}
			assertRedisRouteKeys(t, ctx, r, playerID, v.wantAfter)

			if v.ttl {
				ttl, err := raw.TTL(ctx, keys[2]).Result()
				require.NoError(t, err)
				if v.wantTouch {
					assert.Equal(t, time.Duration(-1), ttl, "the touch rewrites maprev with a plain SET, which clears the TTL")
				} else {
					assert.Positive(t, ttl, "maprev must not be touched")
				}
			}
		})
	}
}

// TestDBFirstRegister_RealStores:真库(storetest 一次性库)+ 真 Redis 的 db_first 端到端。
// 两个环境变量缺任一就 Skip:这个用例要一次性的 Redis 与能建库的 MySQL 账号,不能碰开发库。
func TestDBFirstRegister_RealStores(t *testing.T) {
	if os.Getenv(envRedisHost) == "" || os.Getenv(storetest.EnvHost) == "" {
		t.Skipf("needs both %s (a throwaway Redis) and %s (MySQL with CREATE DATABASE)", envRedisHost, storetest.EnvHost)
	}
	ctx := context.Background()
	db := storetest.NewMigratedDB(t)
	rs, err := store.NewRouteStore(db.Cfg, store.RouteStoreOptions{
		FenceShards: placement.DefaultRouteFenceShards,
		Writer:      store.NewRouteWriterID("routing_it", "test"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rs.Close() })

	rc := config.RouteStoreConfig{Mode: config.RouteStoreModeDBFirst}
	if on, err := redisAppendOnly(ctx, resolveRedisHost(t)); err != nil || !on {
		// WAITAOF 的真实路径由 TestPublishRoute_RealRedisWaitAOF 单独验;这里只验库 → 缓存的编排。
		t.Logf("appendonly off or unknown (err=%v): WAITAOF disabled for this run", err)
		rc.WaitAOFNumLocal, rc.WaitAOFNumReplicas = intPtr(0), intPtr(0)
	}
	r, raw := newIntegrationRouterWith(t, rc, WithRouteStore(rs))
	globalPlacement := placement.StableValue(placement.DefaultGlobalStorageID, 1)

	// 1. 新玩家:库先提交(rev=1),再发布三键。
	require.NoError(t, r.RegisterPlayerZone(ctx, 90501, 61, placement.DefaultGlobalStorageID))
	assertRedisRouteKeys(t, ctx, r, 90501, [3]string{"61", globalPlacement, "1"})
	assertRouteRow(t, rs, 90501, 61, placement.DefaultGlobalStorageID, 1)

	// 2. 同 home 重试:幂等,rev 不变,缓存不变。
	require.NoError(t, r.RegisterPlayerZone(ctx, 90501, 61, placement.DefaultGlobalStorageID))
	assertRouteRow(t, rs, 90501, 61, placement.DefaultGlobalStorageID, 1)
	assertRedisRouteKeys(t, ctx, r, 90501, [3]string{"61", globalPlacement, "1"})

	// 3. 异 home:Redis 预检就拒绝。
	var conflict *HomeZoneConflictError
	require.ErrorAs(t, r.RegisterPlayerZone(ctx, 90501, 62, 0), &conflict)
	assert.Equal(t, uint32(61), conflict.Existing)

	// 4. 只在库里有行(缓存里没有):预检放行,库内加锁读判冲突,缓存零写入。
	_, err = rs.ImportRoutes(ctx, []store.RouteImportRow{{PlayerID: 90502, HomeZone: 63}}, false)
	require.NoError(t, err)
	require.ErrorAs(t, r.RegisterPlayerZone(ctx, 90502, 64, 0), &conflict)
	assert.Equal(t, uint32(63), conflict.Existing)
	assertRedisRouteKeys(t, ctx, r, 90502, [3]string{})

	// 5. 缓存认得、库里没行 → ErrRouteStore + ErrRouteUnknownToStore,库里也不新插。
	require.NoError(t, raw.Set(ctx, placement.HomeZoneKey(90503), "61", 0).Err())
	err = r.RegisterPlayerZone(ctx, 90503, 61, 0)
	require.ErrorIs(t, err, ErrRouteStore)
	require.ErrorIs(t, err, ErrRouteUnknownToStore)
	assertNoRouteRow(t, rs, 90503)

	// 6. 预检命中合服围栏 → 不进库。
	require.NoError(t, raw.Set(ctx, MergeFenceKey(61), `{"started_at":1757000000}`, time.Minute).Err())
	require.ErrorIs(t, r.RegisterPlayerZone(ctx, 90504, 61, 0), ErrZoneMergeInProgress)
	assertNoRouteRow(t, rs, 90504)
}

// redisAppendOnly 用一个临时客户端读 CONFIG GET appendonly。
func redisAppendOnly(ctx context.Context, host string) (bool, error) {
	probe := goredis.NewClient(&goredis.Options{Addr: host, DB: 0})
	defer probe.Close()
	cctx, cancel := context.WithTimeout(ctx, itRedisTimeout)
	defer cancel()
	vals, err := probe.ConfigGet(cctx, "appendonly").Result()
	if err != nil {
		return false, err
	}
	return vals["appendonly"] == "yes", nil
}

// assertRedisRouteKeys 断言一名玩家的 [player:zone, player:placement, player:maprev](缺席为 "")。
func assertRedisRouteKeys(t *testing.T, ctx context.Context, r *Router, playerID uint64, want [3]string) {
	t.Helper()
	vals, err := r.mappingRedis.MgetCtx(ctx, placement.HomeZoneKey(playerID), placement.Key(playerID), placement.MapRevKey(playerID))
	require.NoError(t, err)
	require.Len(t, vals, 3)
	assert.Equal(t, want, [3]string{vals[0], vals[1], vals[2]}, "player %d", playerID)
}

// assertRouteRow 断言 player_route 里这一行的 home / 落点 / rev。
func assertRouteRow(t *testing.T, rs *store.RouteStore, playerID uint64, home, storageID uint32, rev uint64) {
	t.Helper()
	rows, err := rs.GetRoutes(context.Background(), []uint64{playerID})
	require.NoError(t, err)
	row, ok := rows[playerID]
	require.True(t, ok, "player_route has no row for %d", playerID)
	assert.Equal(t, home, row.HomeZone)
	assert.Equal(t, storageID, row.StorageID)
	assert.Equal(t, rev, row.Rev)
}

// assertNoRouteRow 断言 player_route 里没有这一行。
func assertNoRouteRow(t *testing.T, rs *store.RouteStore, playerID uint64) {
	t.Helper()
	rows, err := rs.GetRoutes(context.Background(), []uint64{playerID})
	require.NoError(t, err)
	_, ok := rows[playerID]
	assert.False(t, ok, "player_route must have no row for %d", playerID)
}

// resolveRedisHost 读 yaml 的 MappingRedis.Host,可被环境变量覆盖。
func resolveRedisHost(t *testing.T) string {
	t.Helper()
	if v := os.Getenv(envRedisHost); v != "" {
		return v
	}
	var sec struct {
		MappingRedis redis.RedisConf `json:",optional"`
	}
	path := findDataServiceYaml(t)
	if err := conf.Load(path, &sec); err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	if sec.MappingRedis.Host == "" {
		t.Fatalf("%s 里 MappingRedis.Host 为空;设 %s", path, envRedisHost)
	}
	return sec.MappingRedis.Host
}

func findDataServiceYaml(t *testing.T) string {
	t.Helper()
	if v := os.Getenv(envYaml); v != "" {
		return v
	}
	dir, err := os.Getwd()
	require.NoError(t, err)
	for i := 0; i < 6; i++ {
		candidate := filepath.Join(dir, "etc", "data_service.yaml")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("在 %s 之上找不到 etc/data_service.yaml;设 %s", dir, envYaml)
	return ""
}

// ── 第 3 批读方门禁:读脚本与可信 / 回源在真 Redis 上(docs/handoff/launch-readiness-20261013/spec_D1_batch1b2_2_3.md §5.2)──
//
// miniredis 不支持 INFO 的 server / replication 段,读脚本的执行语义(INFO 能否在脚本里调、string.match 取到的字段、
// Lua false 回成 nil)只有真 Redis 能证。这几条用例会写 mapping:route:ready(带本实例身份的「可信」标记),所以**只在显式给了
// DATA_SERVICE_IT_REDIS_HOST(一次性的 Redis)时跑**,不碰 yaml 里那个开发用的 mapping Redis;跑前要求 mapping:route:* 为空,
// 跑后删掉。回源 SQL 的用例另要 DATA_SERVICE_IT_MYSQL_HOST(能建库的账号,storetest 一次性库)。
//
// 加 DATA_SERVICE_IT_REQUIRE=1:依赖缺失 / 不可达从 SKIP 变成 FAIL(PASS / FAIL / SKIP 必须可区分,规格 §6.5 同口径)。
// 本批新增的用例都以 requireRouteReadIT 开头;既有用例的 Skip 口径不变。

// envRouteReadITRequire 设为 1 时,本批新增的集成用例被跳过即判失败。
const envRouteReadITRequire = "DATA_SERVICE_IT_REQUIRE"

// requireRouteReadIT 必须是用例的第一句:它登记的清理最后一个执行,发现用例被跳过(依赖缺失)且设了
// DATA_SERVICE_IT_REQUIRE=1 就判失败。用清理而不是再探测一遍依赖:跳过的判定只在各自的 setup 一处。
func requireRouteReadIT(t *testing.T) {
	t.Helper()
	if os.Getenv(envRouteReadITRequire) != "1" {
		return
	}
	t.Cleanup(func() {
		if t.Skipped() {
			t.Errorf("%s=1: this integration test was skipped (a dependency is missing or unusable, see the SKIP reason above); treated as FAIL",
				envRouteReadITRequire)
		}
	})
}

// routeReadITControlPattern 是读方门禁用例会碰的控制键(就绪标记;令牌 / 水位键同前缀,一并要求为空)。
const routeReadITControlPattern = "mapping:route:*"

// newReadGateIntegrationRouter 接一个 ReadRequiresReady=true 的 db_first Router(真 Redis,WAITAOF 0/0:本组用例不发布)。
// reader 非 nil 时注入回源读者。要求显式的一次性 Redis 与空的 mapping:route:*,结束时删掉本用例写的控制键。
func newReadGateIntegrationRouter(t *testing.T, reader RouteReader) (*Router, *goredis.Client) {
	t.Helper()
	if os.Getenv(envRedisHost) == "" {
		t.Skipf("needs %s (a throwaway Redis >= 7.2): these cases write a trusted mapping:route:ready marker", envRedisHost)
	}
	rc := config.RouteStoreConfig{
		Mode:               config.RouteStoreModeDBFirst,
		WaitAOFNumLocal:    intPtr(0),
		WaitAOFNumReplicas: intPtr(0),
		ReadRequiresReady:  true,
	}
	var opts []RouterOption
	if reader != nil {
		opts = append(opts, WithRouteReader(reader))
	}
	r, raw := newIntegrationRouterWith(t, rc, opts...)
	ctx, cancel := context.WithTimeout(context.Background(), itRedisTimeout)
	defer cancel()
	keys, _, err := raw.Scan(ctx, 0, routeReadITControlPattern, 50).Result()
	require.NoError(t, err)
	if len(keys) > 0 {
		t.Skipf("Redis DB0 already holds %v: these cases need %s to be empty (use a throwaway instance)", keys, routeReadITControlPattern)
	}
	// 晚于 newIntegrationRouterWith 登记 → 先于它执行(raw 还没关)。
	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), itRedisTimeout)
		defer cleanCancel()
		if err := raw.Del(cleanCtx, placement.RouteReadyKey, placement.RouteWarmKey, placement.RouteWatermarkKey).Err(); err != nil {
			t.Errorf("清理 mapping:route:* 失败(请手工删): %v", err)
		}
	})
	return r, raw
}

// realRedisIdentity 用普通命令(不经脚本)读本实例身份,与读脚本里 string.match 取到的对照。
func realRedisIdentity(t *testing.T, raw *goredis.Client) placement.RedisInstanceIdentity {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), itRedisTimeout)
	defer cancel()
	srv, err := raw.Info(ctx, "server").Result()
	require.NoError(t, err)
	repl, err := raw.Info(ctx, "replication").Result()
	require.NoError(t, err)
	id, err := placement.ParseRedisInstanceIdentity(srv, repl)
	require.NoError(t, err)
	require.Equal(t, "master", id.Role, "these cases need a master (a replica would never be trusted)")
	return id
}

// TestRouteReadyRead_RealRedisScriptsCallInfo:两段读脚本在真 Redis 上能调 INFO(规格 §2 M1 的「未核实」项),
// 取到的 run_id / role / master_replid 与普通 INFO 命令一致;标记缺席回 nil、映射键缺席回 nil(Lua false → nil),
// R2 脚本的 home 与围栏按约定回 string。FAIL 时按规格 §2 M1 的退路处理(pipeline 先读数据后读身份),不改用例。
func TestRouteReadyRead_RealRedisScriptsCallInfo(t *testing.T) {
	requireRouteReadIT(t)
	r, raw := newReadGateIntegrationRouter(t, nil)
	ctx := context.Background()
	want := realRedisIdentity(t, raw)

	res, err := r.mappingRedis.EvalCtx(ctx, placement.RouteReadyReadScript, placement.RouteReadyReadKeys(mappingKey(90801)))
	require.NoError(t, err, "INFO must be callable inside a script on this Redis")
	read, err := placement.ParseRouteReadyRead(res, 1)
	require.NoError(t, err, "raw reply %#v", res)
	assert.False(t, read.MarkerPresent)
	assert.Equal(t, want, read.Identity)
	assert.Equal(t, []any{nil}, read.Values)

	// 映射键不是 string 类型时 MGET 回 nil(与今天各读方的 MGET 一致),标记不是 string 时按缺席。
	require.NoError(t, raw.LPush(ctx, mappingKey(90802), "wrong-type").Err())
	require.NoError(t, raw.LPush(ctx, placement.RouteReadyKey, "wrong-type").Err())
	res, err = r.mappingRedis.EvalCtx(ctx, placement.RouteReadyReadScript, placement.RouteReadyReadKeys(mappingKey(90802)))
	require.NoError(t, err)
	read, err = placement.ParseRouteReadyRead(res, 1)
	require.NoError(t, err, "raw reply %#v", res)
	assert.False(t, read.MarkerPresent, "a non-string marker reads as absent")
	assert.Equal(t, []any{nil}, read.Values)
	require.NoError(t, raw.Del(ctx, placement.RouteReadyKey).Err())

	require.NoError(t, raw.Set(ctx, mappingKey(90803), "73", 0).Err())
	require.NoError(t, raw.Set(ctx, MergeFenceKey(73), `{"started_at":1757000000}`, time.Minute).Err())
	for _, tc := range []struct {
		player uint64
		values []any
	}{
		{90803, []any{"73", "1"}},
		{90804, []any{nil, nil}},
	} {
		res, err := r.mappingRedis.EvalCtx(ctx, getHomeZoneFenceReadyScript, placement.RouteReadyReadKeys(mappingKey(tc.player)), mergeFenceKeyPrefix)
		require.NoError(t, err)
		read, err := placement.ParseRouteReadyRead(res, 2)
		require.NoError(t, err, "raw reply %#v", res)
		assert.Equal(t, want, read.Identity)
		assert.Equal(t, tc.values, read.Values, "player %d", tc.player)
	}

	verdict, err := ProbeRouteReadyRead(ctx, redis.RedisConf{Host: resolveRedisHost(t), Type: "node"})
	require.NoError(t, err, "the startup probe must pass on a real Redis")
	assert.Equal(t, placement.RouteReadyAbsent, verdict)
}

// TestReadGate_RealRedisTrustedMarkerUsesCache:标记记下的就是本实例身份 → R2 / R6 / R3 用缓存(含缓存里的合服围栏、
// 缺键 = 权威缺席),一次都不回源;标记换成别的 run_id(模拟重启 / 旧备份)→ 立刻不可信、回源。
func TestReadGate_RealRedisTrustedMarkerUsesCache(t *testing.T) {
	requireRouteReadIT(t)
	reader := &readFakeReader{err: errReadBoom}
	r, raw := newReadGateIntegrationRouter(t, reader)
	ctx := context.Background()
	id := realRedisIdentity(t, raw)
	require.NoError(t, raw.Set(ctx, placement.RouteReadyKey, readyMarkerFor(t, id.RunID, id.MasterReplID), 0).Err())
	require.NoError(t, raw.Set(ctx, mappingKey(90901), "71", 0).Err())

	zone, merging, err := r.GetPlayerHomeZoneAndMergeFence(ctx, 90901)
	require.NoError(t, err)
	assert.Equal(t, uint32(71), zone)
	assert.False(t, merging)
	require.NoError(t, raw.Set(ctx, MergeFenceKey(71), `{"started_at":1757000000}`, time.Minute).Err())
	_, merging, err = r.GetPlayerHomeZoneAndMergeFence(ctx, 90901)
	require.NoError(t, err)
	assert.True(t, merging)
	_, _, err = r.GetPlayerHomeZoneAndMergeFence(ctx, 90999)
	require.ErrorIs(t, err, ErrHomeZoneNotMapped, "a trusted replica's missing key is authoritative absence")

	zone, err = r.GetPlayerHomeZone(ctx, 90901)
	require.NoError(t, err)
	assert.Equal(t, uint32(71), zone)
	batch, err := r.BatchGetPlayerHomeZone(ctx, []uint64{90901, 90999})
	require.NoError(t, err)
	assert.Equal(t, map[uint64]uint32{90901: 71}, batch)
	assert.Empty(t, reader.homeFenceCalls, "trusted reads must not fall back")
	assert.Empty(t, reader.getRoutesCalls, "trusted reads must not fall back")

	require.NoError(t, raw.Set(ctx, placement.RouteReadyKey, readyMarkerFor(t, strings.Repeat("a", 40), id.MasterReplID), 0).Err())
	_, _, err = r.GetPlayerHomeZoneAndMergeFence(ctx, 90901)
	require.ErrorIs(t, err, ErrRouteReadUnavailable, "a marker bound to another instance is not trusted; the (failing) fallback is used")
	assert.NotErrorIs(t, err, ErrHomeZoneNotMapped)
	assert.Equal(t, []uint64{90901}, reader.homeFenceCalls)
}

// TestReadGate_RealRedisMarkerAbsentFallsBackToSQL:真 Redis + 真库(storetest 一次性库)。标记缺席 → R2 / R6 / R3 回源
// player_route(缓存里的旧值不用、也不回填),围栏取 63 号片(持有者写满 64 片);R7 在 db_first 下读库。
func TestReadGate_RealRedisMarkerAbsentFallsBackToSQL(t *testing.T) {
	requireRouteReadIT(t)
	if os.Getenv(envRedisHost) == "" || os.Getenv(storetest.EnvHost) == "" {
		t.Skipf("needs both %s (a throwaway Redis) and %s (MySQL with CREATE DATABASE)", envRedisHost, storetest.EnvHost)
	}
	ctx := context.Background()
	db := storetest.NewMigratedDB(t)
	rs, err := store.NewRouteStore(db.Cfg, store.RouteStoreOptions{
		FenceShards: placement.DefaultRouteFenceShards,
		Writer:      store.NewRouteWriterID("routing_it", "test"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rs.Close() })
	_, err = rs.ImportRoutes(ctx, []store.RouteImportRow{{PlayerID: 91001, HomeZone: 72}, {PlayerID: 91002, HomeZone: 72}}, false)
	require.NoError(t, err)

	r, raw := newReadGateIntegrationRouter(t, rs)
	require.NoError(t, raw.Set(ctx, mappingKey(91001), "5", 0).Err(), "a stale cached value that must not be used")

	zone, merging, err := r.GetPlayerHomeZoneAndMergeFence(ctx, 91001)
	require.NoError(t, err)
	assert.Equal(t, uint32(72), zone)
	assert.False(t, merging)
	zone, err = r.GetPlayerHomeZone(ctx, 91002)
	require.NoError(t, err)
	assert.Equal(t, uint32(72), zone)
	_, _, err = r.GetPlayerHomeZoneAndMergeFence(ctx, 91003)
	require.ErrorIs(t, err, ErrHomeZoneNotMapped, "no row in player_route is authoritative absence")
	batch, err := r.BatchGetPlayerHomeZone(ctx, []uint64{91001, 91002, 91003})
	require.NoError(t, err)
	assert.Equal(t, map[uint64]uint32{91001: 72, 91002: 72}, batch)

	cached, err := raw.Get(ctx, mappingKey(91001)).Result()
	require.NoError(t, err)
	assert.Equal(t, "5", cached, "readers never backfill the cache")
	n, err := raw.Exists(ctx, mappingKey(91002)).Result()
	require.NoError(t, err)
	assert.EqualValues(t, 0, n, "readers never backfill the cache")

	// 库里的围栏:持有者在一个事务里写满全部 64 片(placement.MaxRouteFenceShards 的契约),过期取库时钟 + 10 分钟。
	tx, err := db.Raw.BeginTx(ctx, nil)
	require.NoError(t, err)
	for shard := uint32(0); shard < placement.MaxRouteFenceShards; shard++ {
		_, err := tx.ExecContext(ctx,
			"INSERT INTO zone_merge_fence (zone_id, shard, run_id, purpose, src_zone, dst_zone, expires_at_ms, fence_epoch)"+
				" VALUES (?, ?, '72-73-20261018T000000Z-0a1b2c3d', 'merge', 72, 73, CAST(UNIX_TIMESTAMP(NOW(3)) * 1000 AS UNSIGNED) + 600000, 1)",
			72, shard)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
	zone, merging, err = r.GetPlayerHomeZoneAndMergeFence(ctx, 91001)
	require.NoError(t, err)
	assert.Equal(t, uint32(72), zone)
	assert.True(t, merging, "the fence comes from player_route's snapshot, not from the (untrusted) cache")

	ids, err := r.GetAllPlayerIDsInZone(ctx, 72)
	require.NoError(t, err)
	assert.ElementsMatch(t, []uint64{91001, 91002}, ids)
}
