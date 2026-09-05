package logic

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 队列 key 往返:matchQueueKey 生成 → parseQueueKey 解析,且三类 key 共用 {mq}。
func TestParseQueueKeyRoundTrip(t *testing.T) {
	cases := []struct {
		mode   int32
		config uint32
	}{
		{3, 0}, {3, 1}, {5, 1}, {1, 42}, {0, 0}, {6, 4294967295},
	}
	for _, c := range cases {
		key := matchQueueKey(c.mode, c.config)
		require.Contains(t, key, matchQueueHashTag)
		mode, config, err := parseQueueKey(key)
		require.NoError(t, err, key)
		require.Equal(t, c.mode, mode, key)
		require.Equal(t, c.config, config, key)

		// 旧格式 key(无 tag)同样能解析:迁移路径靠它取 (mode, config)。
		legacy := legacyMatchQueueKey(c.mode, c.config)
		require.NotContains(t, legacy, "{")
		mode, config, err = parseQueueKey(legacy)
		require.NoError(t, err, legacy)
		require.Equal(t, c.mode, mode, legacy)
		require.Equal(t, c.config, config, legacy)
	}
	require.Contains(t, matchQueueIndexKey, matchQueueHashTag)
	require.Contains(t, matcherLockKey(3, 0), matchQueueHashTag)
}

// 前缀演进不影响解析(只认尾部);异物拒绝。
func TestParseQueueKeyRejectsGarbage(t *testing.T) {
	_, _, err := parseQueueKey("match:{other-tag}:queue:3:7")
	require.NoError(t, err, "hash tag 变化不应影响尾部解析")

	for _, bad := range []string{
		"", "match:{mq}:index", "match:{mq}:lock:3:0", "match:{mq}:queue:x:1",
		"match:{mq}:queue:3", "spectate:battles:active", "player:1:location",
	} {
		_, _, err := parseQueueKey(bad)
		require.Error(t, err, bad)
	}
}

// ---- Redis Cluster slot 归属(把注释里的"同 slot,集群下可用"钉成测试)----
//
// miniredis 单节点不做 slot 校验,所有集成测试对 CROSSSLOT 零证明力;任何后续
// 改动(换前缀、在 key 前再拼一段带花括号的 zone/env 段 —— Redis 只取第一对花括号
// 做 hash tag)都能让 miniredis 全绿却在真实集群上让 enqueue / requeue / prune
// 三条多 key Lua 立刻 CROSSSLOT。这里按 Redis 规范独立实现 slot 计算。

// redisHashSlot 按 Redis Cluster 规范算 key 的 slot:取第一对非空 {} 里的内容做
// hash tag,CRC16-XMODEM(poly 0x1021,init 0)mod 16384。
func redisHashSlot(key string) int {
	tagged := key
	if start := strings.IndexByte(key, '{'); start >= 0 {
		if end := strings.IndexByte(key[start+1:], '}'); end > 0 {
			tagged = key[start+1 : start+1+end]
		}
	}
	return int(crc16XModem([]byte(tagged)) & 16383)
}

func crc16XModem(data []byte) uint16 {
	var crc uint16
	for _, b := range data {
		crc ^= uint16(b) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// 自检:标准 XMODEM 校验向量与 redis-cli CLUSTER KEYSLOT 的已知值。
func TestRedisHashSlotReference(t *testing.T) {
	require.Equal(t, uint16(0x31C3), crc16XModem([]byte("123456789")))
	require.Equal(t, 12182, redisHashSlot("foo"))
	require.Equal(t, 5061, redisHashSlot("bar"))
	require.Equal(t, redisHashSlot("foo"), redisHashSlot("{foo}bar"), "hash tag 只取花括号内")
	require.Equal(t, redisHashSlot("foo"), redisHashSlot("x{foo}y{bar}"), "只认第一对花括号")
	require.Equal(t, redisHashSlot("{}foo"), redisHashSlot("{}foo"), "空 tag 按整串算")
	require.NotEqual(t, redisHashSlot("foo"), redisHashSlot("{}foo"))
}

// 队列三类 key 与每条多 key Lua 的 KEYS 列表必须同 slot。
func TestQueueKeysShareOneSlot(t *testing.T) {
	want := redisHashSlot(matchQueueIndexKey)
	require.Equal(t, redisHashSlot(strings.Trim(matchQueueHashTag, "{}")), want, "注册集 slot 必须由 {mq} 决定")

	cases := []struct {
		mode   int32
		config uint32
	}{
		{3, 0}, {3, 1}, {5, 1}, {1, 42}, {0, 0}, {6, 4294967295}, {4, 7}, {2, 3},
	}
	for _, c := range cases {
		queueKey := matchQueueKey(c.mode, c.config)
		lockKey := matcherLockKey(c.mode, c.config)
		rankKey := matchRankKey(c.mode, c.config)
		require.Equal(t, want, redisHashSlot(queueKey), queueKey)
		require.Equal(t, want, redisHashSlot(lockKey), lockKey)
		require.Equal(t, want, redisHashSlot(rankKey), rankKey)
		derived, err := rankKeyForQueue(queueKey)
		require.NoError(t, err)
		require.Equal(t, rankKey, derived, "由队列 key 推出的镜像 key 必须与 matchRankKey 一致")

		// 多 key Lua 的 KEYS 列表(与 queue.go / matcher.go 的调用点一一对应)。
		for name, keys := range map[string][]string{
			"enqueueScript":            {matchQueueIndexKey, queueKey, rankKey},
			"requeueScript":            {matchQueueIndexKey, queueKey, rankKey},
			"pruneQueueScript":         {matchQueueIndexKey, queueKey, rankKey},
			"removeQueueMembersScript": {queueKey, rankKey},
			"queueSnapshotScript":      {queueKey, rankKey},
		} {
			for _, k := range keys {
				require.Equal(t, want, redisHashSlot(k), fmt.Sprintf("%s KEYS=%v 出现跨 slot key %s", name, keys, k))
			}
		}
	}
}

// 按玩家 / 按 id 分布的 key 不得带花括号(误用 tag 会把它们压到同一 slot)。
func TestPerEntityKeysHaveNoHashTag(t *testing.T) {
	for _, key := range []string{
		matchTicketKey(1), matchTicketKey(18446744073709551615),
		matchRatingKey(1), matchRatingKey(18446744073709551615), matchRatingAppliedKey(42),
		challengeKey(9001), challengeTargetKey(6002),
		spectateBattleKey(42), spectateWatchingKey(7), spectateBattlesActiveKey,
		battleLockKey(1), getPlayerLocationKey(1), playerSessionKey(1),
		legacyMatchQueueKey(3, 0),
	} {
		require.NotContains(t, key, "{", key)
		require.NotContains(t, key, "}", key)
	}
	// 两张不同玩家的票据落不同 slot(证明确实按玩家分布,不是被某段 tag 压平)。
	require.NotEqual(t, redisHashSlot(matchTicketKey(1)), redisHashSlot(matchTicketKey(2)))
	require.NotEqual(t, redisHashSlot(matchRatingKey(1)), redisHashSlot(matchRatingKey(2)))

	// 自定义前缀的队列 key(票据里记录的任意形态)推出的镜像 key 保留前缀;
	// 旧格式 key(无 tag)推出的镜像 key 也无 tag —— 所以取消旧 key 必须走单 key LREM。
	derived, err := rankKeyForQueue("match:{other}:queue:3:7")
	require.NoError(t, err)
	require.Equal(t, "match:{other}:rank:3:7", derived)
	require.Equal(t, redisHashSlot("match:{other}:queue:3:7"), redisHashSlot(derived))
	derived, err = rankKeyForQueue(legacyMatchQueueKey(3, 0))
	require.NoError(t, err)
	require.Equal(t, "match:rank:3:0", derived)
	_, err = rankKeyForQueue("match:{mq}:index")
	require.Error(t, err)
}
