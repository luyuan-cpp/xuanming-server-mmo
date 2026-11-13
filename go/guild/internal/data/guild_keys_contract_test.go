package data

// guild 控制集群键的契约(docs/design/redis-cluster-all-domains-20261110.md §3.2、§4.8)。
//
// 控制集群上 Redis Cluster,一段 Lua 的全部 KEYS 必须同 slot,否则回 CROSSSLOT;控制集群句柄的守卫
// (clusterredis.ControlClusterKeys)还要求多键命令的每个键都带同一个登记过的族 tag。开发环境连的是单实例,
// 跨 slot 不会报错,所以由这里提前拦住:KEYS 都由生产代码的函数给出,脚本加键时这里跟着变。

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"shared/clusterredis"
	"shared/redisslot"
)

// 金样:键名逐字钉住。改形要同批改运维文档与 tools/data_consistency_check 的手写键名(它是独立 go 模块,不 import redisslot)。
func TestGuildControlKeysGolden(t *testing.T) {
	cases := map[string]string{
		guildKey(42):                     "guild:v2:{g:42}",
		guildCacheGenerationKey(42):      "guild:v2:cache_generation:{g:42}",
		playerGuildKey(7):                "player_guild:v2:{pg:7}",
		playerGuildCacheGenerationKey(7): "player_guild:v2:cache_generation:{pg:7}",
		zoneRankKey(3):                   "guild_rank:zone:{gr:3}",
		zoneRankVersionKey(3):            "guild_rank:version:{gr:3}",
		zoneRankTombstoneKey(3):          "guild_rank:tombstone:{gr:3}",
		guildRankLockKey(3):              "guild_rank:maintenance_lock:{gr:3}",
	}
	for got, want := range cases {
		assert.Equal(t, want, got)
	}
}

// 每段多键脚本的 KEYS 同一个 tag、同一个 slot,并且过得了控制集群句柄的守卫。
func TestGuildMultiKeyScriptsShareOneControlTag(t *testing.T) {
	const (
		guildID  = uint64(1234567890123)
		playerID = uint64(9876543210987)
		zone     = uint32(445)
	)
	scripts := map[string][]string{
		"帮会缓存填充 / 失效":     {guildCacheGenerationKey(guildID), guildKey(guildID)},
		"玩家所在帮会缓存填充 / 失效": {playerGuildCacheGenerationKey(playerID), playerGuildKey(playerID)},
		"写榜 / 立墓碑":        rankZoneKeys(zone),
		"摘榜 / 按版本摘 / 读榜头": rankEntryKeys(zone),
	}
	for name, keys := range scripts {
		require.GreaterOrEqual(t, len(keys), 2, name)
		firstTag, tagged := redisslot.HashTag(keys[0])
		require.True(t, tagged, "%s: %s 必须带 tag", name, keys[0])
		assert.True(t, redisslot.IsControlTag(firstTag), "%s: %s 的 tag 必须是登记过的控制集群族", name, keys[0])
		for _, key := range keys[1:] {
			tag, _ := redisslot.HashTag(key)
			assert.Equal(t, firstTag, tag, "%s: %s 与 %s 不同 tag", name, keys[0], key)
			assert.Equal(t, redisslot.HashSlot(keys[0]), redisslot.HashSlot(key), "%s: %s 不在同一 slot", name, key)
		}
		assert.NoError(t, clusterredis.ControlClusterKeys.CheckKeys(keys), "%s 过不了控制集群守卫", name)
	}
}

// 维护锁与同区榜键同一个 tag:以后锁与榜在同一段脚本里核对时不用再改形。
func TestGuildRankLockSharesTheZoneTag(t *testing.T) {
	assert.Equal(t, redisslot.HashSlot(zoneRankKey(9)), redisslot.HashSlot(guildRankLockKey(9)))
	assert.NoError(t, clusterredis.ControlClusterKeys.CheckKeys([]string{guildRankLockKey(9), zoneRankKey(9)}))
}

// 控制集群键不能长成玩家域的形状:玩家域客户端拿到会拒绝(反过来也一样),键发错句柄在开发期就暴露。
func TestGuildControlKeysAreRejectedByThePlayerDomain(t *testing.T) {
	for _, key := range []string{guildKey(1), playerGuildKey(1), zoneRankKey(1), guildRankLockKey(1)} {
		assert.ErrorIs(t, clusterredis.PlayerDomainKeys.CheckKeys([]string{key}), clusterredis.ErrWrongDomainTag, key)
	}
}
