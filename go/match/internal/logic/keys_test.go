package logic

// 观战键的分布约定:本场的键共用 hash tag(见 spectate_tier_test.go),按实体分布的键不带花括号;
// 玩家域契约键带玩家 id 的 hash tag(docs/design/redis-player-store-cluster.md §5.2)。
// 这里按 Redis 规范独立实现 slot 计算,不依赖 Redis。

import (
	"strings"
	"testing"

	"match/internal/playercontract"

	"github.com/stretchr/testify/require"
)

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

// 按 id 分布的 key 不得带花括号(误用 tag 会把它们压到同一 slot)。
func TestPerEntityKeysHaveNoHashTag(t *testing.T) {
	for _, key := range []string{
		spectateBattlesActiveKey, spectateBroadcastActiveKey, spectateFeaturedActiveKey,
	} {
		require.NotContains(t, key, "{", key)
		require.NotContains(t, key, "}", key)
	}
	// 玩家域契约键相反:同一玩家的键必须同 slot。
	for _, key := range []string{playercontract.BattleLockKey(1), playercontract.LocationKey(1), playercontract.SessionKey(1)} {
		require.Equal(t, redisHashSlot("1"), redisHashSlot(key), key)
	}
	// 不同玩家的键落不同 slot(证明确实按 id 分布,不是被某段 tag 压平)。
	require.NotEqual(t, redisHashSlot(playercontract.LocationKey(1)), redisHashSlot(playercontract.LocationKey(2)))
}
