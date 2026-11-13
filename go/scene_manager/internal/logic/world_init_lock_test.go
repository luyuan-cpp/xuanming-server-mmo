package logic

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 补建锁的 SET NX 首发已经写入、回包丢了,go-zero 句柄重发同一条命令:重发看到的是自己这次的值,回「已存在」
// (docs/design/infra-reconnect-overview.md §4.1 R8)。拿锁的一方要认出这把锁是自己的,照常补建并在结束时放锁。
// 修复前红:当成别人持锁、跳过本轮;这把锁没人放,空占到 TTL(60 秒),期间本 zone 没有补建。
func TestAcquireWorldInitLock_ReplayedSetNXIsRecognisedAsOwnLock(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	const confID uint64 = 1001
	ctx := context.Background()
	mr.ZAdd(nodeLoadKey(testZoneId), 0, "10")
	lockKey := worldInitLockKey(testZoneId)

	// 第一条对锁键的 SET 到达时,先替它把值写进去:随后真正执行的 SET NX 落空,等同于「首发已写入,这一条是重发」。
	// 钩子跑在 miniredis 的服务端协程上、还没拿库锁,这里可以调 mr.*;只触发一次。
	var fired atomic.Bool
	mr.Server().SetPreHook(func(_ *server.Peer, cmd string, args ...string) bool {
		if cmd != "SET" || len(args) < 2 || args[0] != lockKey || !fired.CompareAndSwap(false, true) {
			return false
		}
		if err := mr.Set(lockKey, args[1]); err != nil {
			t.Errorf("simulate the first SET NX having landed: %v", err)
		}
		return false
	})
	t.Cleanup(func() { mr.Server().SetPreHook(nil) })

	initWorldScenesForZone(ctx, sc, testZoneId, []uint64{confID}, false)

	require.True(t, fired.Load(), "前提不成立:没有拦到补建锁的 SET NX")
	channels, err := GetAllWorldChannels(ctx, sc, confID, testZoneId)
	require.NoError(t, err)
	assert.Len(t, channels, 1, "认出自己的锁,照常补建")
	assert.False(t, mr.Exists(lockKey), "补建结束时按令牌放锁")
}

// 锁确实是别人的:核对令牌不符,照旧跳过本轮,别人的锁原样留着。
func TestAcquireWorldInitLock_AnotherHoldersLockStillSkipsTheRound(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	const confID uint64 = 1001
	ctx := context.Background()
	mr.ZAdd(nodeLoadKey(testZoneId), 0, "10")
	lockKey := worldInitLockKey(testZoneId)
	require.NoError(t, mr.Set(lockKey, "another-initializer"))

	initWorldScenesForZone(ctx, sc, testZoneId, []uint64{confID}, false)

	channels, err := GetAllWorldChannels(ctx, sc, confID, testZoneId)
	require.NoError(t, err)
	assert.Empty(t, channels, "别人持锁时本轮不补建")
	holder, err := mr.Get(lockKey)
	require.NoError(t, err)
	assert.Equal(t, "another-initializer", holder, "别人的锁不能动")
}
