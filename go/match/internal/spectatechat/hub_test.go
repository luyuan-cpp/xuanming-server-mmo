package spectatechat

import (
	"testing"

	"shared/clusterredis"

	"github.com/stretchr/testify/require"
)

// 确认观战交流室使用的全部 Redis 键满足 MatchPrivateKeys 守卫（spectate: 前缀 + 同槽 {battle_id} hash tag）。
func TestSpectateChatRedisKeysMatchPrivatePolicy(t *testing.T) {
	const battleID uint64 = 987654321
	keys := []any{
		"eval", "return 0", 5,
		keySeq(battleID),
		keyMsgs(battleID),
		keyCD(battleID),
		keyRate(battleID),
		keyClosed(battleID),
	}
	require.NoError(t, clusterredis.MatchPrivateKeys.CheckCommand(keys))
	require.NoError(t, clusterredis.MatchPrivateKeys.CheckCommand([]any{"lrange", keyMsgs(battleID), 0, -1}))
	require.NoError(t, clusterredis.MatchPrivateKeys.CheckCommand([]any{"hget", keyCD(battleID), "1001"}))
	require.NoError(t, clusterredis.MatchPrivateKeys.CheckCommand([]any{"exists", keyClosed(battleID)}))
}
