package sessionfence

// Leave 可以重放(F4,docs/design/client-request-ordering.md §6.2):LeaveGame 在 Leave 报错时回错误、客户端重发,
// 而报错时脚本可能已经执行、只是回包丢了(读超时 / 断连;登录域客户端自己不重发,重放来自客户端重发 LeaveGame)。重放必须返回同一个解除的玩家,
// 否则会话头不带这个玩家时(BindSession 还没到 gate),它就没人收尾、永久停在 ONLINE。
//
// 修复前:脚本只读 player,第一次执行已经把它删掉,重放返回 0。

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLeaveReplayReturnsTheSameUnboundPlayer(t *testing.T) {
	ctx := context.Background()
	rdb, _ := newTestRedis(t)

	bound, err := Leave(ctx, rdb, testKey, testTTL)
	require.NoError(t, err)
	require.Zero(t, bound, "a connection that never bound a player has nothing to settle")

	_, gen, err := CheckEnterGame(ctx, rdb, testKey, 1001, testTTL)
	require.NoError(t, err)
	outcome, err := BindPlayer(ctx, rdb, testKey, 1001, gen)
	require.NoError(t, err)
	require.Equal(t, OutcomeOK, outcome)

	bound, err = Leave(ctx, rdb, testKey, testTTL)
	require.NoError(t, err)
	require.Equal(t, uint64(1001), bound)
	bound, err = Leave(ctx, rdb, testKey, testTTL)
	require.NoError(t, err)
	require.Equal(t, uint64(1001), bound, "replaying the leave must return the player the first run unbound")

	// 重放多递增的那一次 gen 无害:入口记下的旧 gen 照样判「期间已离开」,绑定已解除。
	outcome, err = Recheck(ctx, rdb, testKey, gen)
	require.NoError(t, err)
	require.Equal(t, OutcomeLeft, outcome)
	closedBound, err := Close(ctx, rdb, testKey, testTTL)
	require.NoError(t, err)
	require.Zero(t, closedBound, "Close only reads the live binding, not the last unbound player")
}

func TestLeaveAfterRebindReturnsTheNewPlayer(t *testing.T) {
	ctx := context.Background()
	rdb, _ := newTestRedis(t)
	_, gen, err := CheckEnterGame(ctx, rdb, testKey, 1001, testTTL)
	require.NoError(t, err)
	_, err = BindPlayer(ctx, rdb, testKey, 1001, gen)
	require.NoError(t, err)
	bound, err := Leave(ctx, rdb, testKey, testTTL)
	require.NoError(t, err)
	require.Equal(t, uint64(1001), bound)

	// 回选角之后换角:新角色绑定,之后的离开返回新角色,上一次解除的旧角色不再出现。
	outcome, gen2, err := CheckEnterGame(ctx, rdb, testKey, 1002, testTTL)
	require.NoError(t, err)
	require.Equal(t, OutcomeOK, outcome)
	outcome, err = BindPlayer(ctx, rdb, testKey, 1002, gen2)
	require.NoError(t, err)
	require.Equal(t, OutcomeOK, outcome)
	bound, err = Leave(ctx, rdb, testKey, testTTL)
	require.NoError(t, err)
	require.Equal(t, uint64(1002), bound)
	bound, err = Leave(ctx, rdb, testKey, testTTL)
	require.NoError(t, err)
	require.Equal(t, uint64(1002), bound)
}
