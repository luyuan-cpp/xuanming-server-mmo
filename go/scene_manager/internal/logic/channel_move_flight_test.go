package logic

// joinChannelMove(channel_move_flight.go)本身的契约:同一频道同一时刻只跑一次、等待方按自己的 ctx 放弃、
// 发起方 panic 时等待方拿到错误、结束之后不留记录。与 Redis / Agones 无关,全用通道排定先后,不靠时序。
// 端到端的合并(EnterScene 自愈与懒改派同时进同一个频道)见 world_channel_reserve_before_move_test.go
// TestReserveBeforeMove_ConcurrentRequestPathMovesShareOneMove。

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// channelMoveWaitersForTest 回 sceneID 那一次在跑的改派正被几个请求等着;没有在跑的回 0。
func channelMoveWaitersForTest(sceneID uint64) int {
	channelMoveFlightsMu.Lock()
	defer channelMoveFlightsMu.Unlock()
	if flight, running := channelMoveFlights[sceneID]; running {
		return flight.waiters
	}
	return 0
}

// channelMoveRunningForTest 报告 sceneID 此刻有没有在跑的改派。
func channelMoveRunningForTest(sceneID uint64) bool {
	channelMoveFlightsMu.Lock()
	defer channelMoveFlightsMu.Unlock()
	_, running := channelMoveFlights[sceneID]
	return running
}

// waitClosedForTest 等 ch 关闭,5 秒等不到就判失败,不让用例挂死。
func waitClosedForTest(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// 等待方按自己的 ctx 放弃等待,不跑自己的改派,也不影响在跑的那一次;那一次结束后不留记录,下一次照常跑。
func TestJoinChannelMove_WaiterGivesUpOnItsOwnContext(t *testing.T) {
	const sceneID uint64 = 88661
	started := make(chan struct{})
	release := make(chan struct{})
	leaderDone := make(chan struct{})
	var leaderNode string
	var leaderErr error
	go func() {
		defer close(leaderDone)
		leaderNode, leaderErr = joinChannelMove(context.Background(), sceneID, func() (string, error) {
			close(started)
			<-release
			return "node-a", nil
		})
	}()
	waitClosedForTest(t, started, "the first move to start")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	node, err := joinChannelMove(ctx, sceneID, func() (string, error) {
		t.Error("a request must not run its own move while one is in flight for the same channel")
		return "", nil
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, node)
	require.True(t, channelMoveRunningForTest(sceneID), "等待方放弃不影响在跑的那一次")

	// 别的频道不合并:同一时刻照常跑自己的。
	other, err := joinChannelMove(context.Background(), sceneID+1, func() (string, error) { return "node-c", nil })
	require.NoError(t, err)
	require.Equal(t, "node-c", other)

	close(release)
	waitClosedForTest(t, leaderDone, "the first move to finish")
	require.NoError(t, leaderErr)
	require.Equal(t, "node-a", leaderNode)
	require.False(t, channelMoveRunningForTest(sceneID), "结束之后不留记录")

	node, err = joinChannelMove(context.Background(), sceneID, func() (string, error) { return "node-b", nil })
	require.NoError(t, err)
	require.Equal(t, "node-b", node, "上一次结束后,下一次照常跑自己的")
}

// 发起方的改派 panic:panic 照样抛给发起方,等待方拿到 errChannelMoveAborted,不会把空落点当成成功;记录照常删掉。
func TestJoinChannelMove_PanicHandsWaitersAnError(t *testing.T) {
	const sceneID uint64 = 88662
	started := make(chan struct{})
	release := make(chan struct{})
	leaderRecovered := make(chan any, 1)
	go func() {
		defer func() { leaderRecovered <- recover() }()
		_, _ = joinChannelMove(context.Background(), sceneID, func() (string, error) {
			close(started)
			<-release
			panic("move blew up")
		})
	}()
	waitClosedForTest(t, started, "the first move to start")

	waiterErr := make(chan error, 1)
	go func() {
		_, err := joinChannelMove(context.Background(), sceneID, func() (string, error) {
			t.Error("a request must not run its own move while one is in flight for the same channel")
			return "", nil
		})
		waiterErr <- err
	}()
	require.Eventually(t, func() bool { return channelMoveWaitersForTest(sceneID) == 1 },
		5*time.Second, time.Millisecond, "前提:第二个请求在等第一次的结果")

	close(release)
	select {
	case recovered := <-leaderRecovered:
		require.Equal(t, "move blew up", recovered, "panic 照样抛给发起方")
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the panicking move")
	}
	select {
	case err := <-waiterErr:
		require.ErrorIs(t, err, errChannelMoveAborted)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the waiter")
	}
	require.False(t, channelMoveRunningForTest(sceneID), "panic 之后同样不留记录")
}
