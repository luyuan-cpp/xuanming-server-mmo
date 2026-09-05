package logic

import (
	"context"
	"testing"
	"time"

	"match/internal/discovery"

	"github.com/stretchr/testify/require"
)

// TestMatcherPausesWhenBattlePoolEmpty:battle 池为空时 matcher 不弹组 ——
// 否则 gather 必以 no_battle_node 秒败、幸存者回队首、下一轮再弹,形成每 500ms 一次的
// 热循环(2026-09-02 本地实测:battle 节点端口撞车未注册,同一对玩家被反复"凑单成功")。
// 期望:排队原样保留(票据仍 queued、队列长度不变),不触发 gather。
func TestMatcherPausesWhenBattlePoolEmpty(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	groups := stubGather(t)
	// 一个从未收到任何节点事件的 watcher:Count()==0。
	svcCtx.BattleNodes = discovery.NewNodeWatcher("battle", "BattleNodeService.rpc/", nil, nil)

	setPlayerLocation(t, mr, 4101, 1)
	setPlayerLocation(t, mr, 4102, 2)
	require.Zero(t, joinQueue1v1(t, svcCtx, 4101).ErrorCode)
	require.Zero(t, joinQueue1v1(t, svcCtx, 4102).ErrorCode)

	runMatcherRound(context.Background(), svcCtx)

	select {
	case call := <-groups:
		t.Fatalf("battle 池为空时不应弹组,却触发了 gather: %+v", call.members)
	case <-time.After(500 * time.Millisecond):
	}

	queueKey := ""
	for _, pid := range []uint64{4101, 4102} {
		ticket, err := loadTicket(svcCtx, pid)
		require.NoError(t, err)
		require.NotNil(t, ticket)
		require.Equal(t, ticketStateQueued, ticket.State, "排队必须原样保留")
		require.NotEmpty(t, ticket.QueueKey)
		queueKey = ticket.QueueKey
	}
	depth, err := svcCtx.MatchRedis.Llen(queueKey)
	require.NoError(t, err)
	require.Equal(t, 2, depth, "队列成员不应被弹出")
}
