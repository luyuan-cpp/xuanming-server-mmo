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
// 用例因此直接在 DB 0 上跑,但只在 player:zone:* / merge:in_progress:* 两个前缀
// 完全为空时才跑(否则 Skip),并且只删自己建的键 —— 绝不 FLUSHDB。

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"data_service/internal/config"

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
var itKeyPatterns = []string{"player:zone:*", "merge:in_progress:*"}

// newIntegrationRouter 接一个指向真 Redis 的 Router(DB 恒 0,见文件顶部)。
func newIntegrationRouter(t *testing.T) (*Router, *goredis.Client) {
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
			t.Skipf("Redis %s DB0 已有 %s 键(%v ...):本用例要独占这两个前缀,先清干净或换一个空实例",
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
	})
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

	require.NoError(t, r.RegisterPlayerZone(ctx, 90001, 3))
	val, err := raw.Get(ctx, "player:zone:90001").Result()
	require.NoError(t, err)
	assert.Equal(t, "3", val)

	// 同 zone 幂等(CreatePlayer 允许重试)
	require.NoError(t, r.RegisterPlayerZone(ctx, 90001, 3))

	// 异 zone 拒绝,且零变更
	err = r.RegisterPlayerZone(ctx, 90001, 9)
	require.Error(t, err)
	var conflict *HomeZoneConflictError
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, uint32(3), conflict.Existing)
	val, err = raw.Get(ctx, "player:zone:90001").Result()
	require.NoError(t, err)
	assert.Equal(t, "3", val, "真 Redis 上也必须一个字节都没改")
}

// TestMergeFence_RealRedisBlocksRegisterAndGatesRemap 端到端跑一遍合服闸门的完整时序:
// 立标记 → 建号被拒 → dry-run → apply → 清标记 → 恢复放行。
// 合服工具那一侧要照着这个顺序实现。
func TestMergeFence_RealRedisBlocksRegisterAndGatesRemap(t *testing.T) {
	r, raw := newIntegrationRouter(t)
	ctx := context.Background()

	require.NoError(t, r.RegisterPlayerZone(ctx, 90010, 31))
	require.NoError(t, r.RegisterPlayerZone(ctx, 90011, 31))
	require.NoError(t, r.RegisterPlayerZone(ctx, 90012, 32))

	// 没有标记时 remap 一律拒绝(线上误调用的防线)。
	_, _, err := r.RemapHomeZoneForMerge(ctx, 31, 32, true)
	require.ErrorIs(t, err, ErrMergeFenceMissing)

	// 合服工具立标记:值是 JSON,TTL 长于整次合服。
	require.NoError(t, raw.Set(ctx, MergeFenceKey(31),
		`{"started_at":1757000000,"source":31,"target":32}`, 2*time.Hour).Err())

	fenced, err := r.IsMergeInProgress(ctx, 31)
	require.NoError(t, err)
	assert.True(t, fenced)

	// 封锁期间不接受新映射 —— 否则它会跑在 SCAN 游标后面,合服完仍指向源 zone。
	require.ErrorIs(t, r.RegisterPlayerZone(ctx, 90013, 31), ErrZoneMergeInProgress)

	matched, updated, err := r.RemapHomeZoneForMerge(ctx, 31, 32, true)
	require.NoError(t, err)
	assert.Equal(t, 2, matched)
	assert.Equal(t, 0, updated)

	matched, updated, err = r.RemapHomeZoneForMerge(ctx, 31, 32, false)
	require.NoError(t, err)
	assert.Equal(t, 2, matched)
	assert.Equal(t, 2, updated)
	for _, pid := range []uint64{90010, 90011, 90012} {
		zone, err := r.GetPlayerHomeZone(ctx, pid)
		require.NoError(t, err)
		assert.Equal(t, uint32(32), zone, "player %d", pid)
	}

	// 工具跑完删标记(TTL 只是兜底);之后源 zone 恢复放行。
	require.NoError(t, raw.Del(ctx, MergeFenceKey(31)).Err())
	require.NoError(t, r.RegisterPlayerZone(ctx, 90013, 31))
}

// TestGetPlayerHomeZone_RealRedisMissingIsTyped:真 Redis 上"键不存在"同样只归到
// ErrHomeZoneNotMapped,不会与连接错误混在一起。
func TestGetPlayerHomeZone_RealRedisMissingIsTyped(t *testing.T) {
	r, _ := newIntegrationRouter(t)

	_, err := r.GetPlayerHomeZone(context.Background(), 90099)
	require.ErrorIs(t, err, ErrHomeZoneNotMapped)
	assert.Contains(t, err.Error(), "no home zone mapping")
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
