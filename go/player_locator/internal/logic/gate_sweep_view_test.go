package logic

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "proto/player_locator"
	"shared/liveview"
)

// 只在本文件用的另外两个 gate 实例号:骤降要至少 liveview.MinKnown 个登记的 gate。
const (
	viewGateD = "inst-gate-d"
	viewGateE = "inst-gate-e"
)

func viewPairs(gates ...string) []gateSlot {
	pairs := make([]gateSlot, 0, len(gates))
	for _, gate := range gates {
		pairs = append(pairs, gateSlot{gate: gate})
	}
	return pairs
}

func liveSet(gates ...string) map[string]struct{} {
	live := make(map[string]struct{}, len(gates))
	for _, gate := range gates {
		live[gate] = struct{}{}
	}
	return live
}

// newViewTestSweeper 只装 checkLiveView 用得到的部分:闸门与时钟。
func newViewTestSweeper(clock *testClock) *gateSweeper {
	return &gateSweeper{
		liveView: liveview.NewGate(liveViewHold, liveview.RecoverBelowThreshold),
		deps:     reconcilerDeps{now: clock.Now},
	}
}

// etcd 回档、重建或连错时 Range 成功回空:不能据此判全部 gate 死亡,压多久都不放行。
func TestCheckLiveView_EmptySetIsAlwaysHeld(t *testing.T) {
	clock := newTestClock()
	s := newViewTestSweeper(clock)
	for round := 0; round < 3; round++ {
		if err := s.checkLiveView(viewPairs("a"), liveSet()); !errors.Is(err, errLiveViewEmpty) {
			t.Fatalf("round %d: err = %v, want errLiveViewEmpty", round, err)
		}
		clock.Advance(liveViewHold)
	}
	if err := s.checkLiveView(nil, liveSet()); err != nil {
		t.Fatalf("没有登记 gate 时空集是正常的: %v", err)
	}
}

// 超过一半同时消失先压住;缺席数连续 liveViewHold 不变才放行:真有这么多 gate 死了,不能永远不释放会话。
func TestCheckLiveView_DropIsHeldUntilStableThenReleased(t *testing.T) {
	clock := newTestClock()
	s := newViewTestSweeper(clock)
	pairs := viewPairs("a", "b", "c", "d")
	live := liveSet("a")
	if err := s.checkLiveView(pairs, live); !errors.Is(err, errLiveViewDrop) {
		t.Fatalf("骤降: err = %v, want errLiveViewDrop", err)
	}
	clock.Advance(liveViewHold - time.Second)
	if err := s.checkLiveView(pairs, live); !errors.Is(err, errLiveViewDrop) {
		t.Fatalf("差一秒满: err = %v, want errLiveViewDrop", err)
	}
	clock.Advance(time.Second)
	if err := s.checkLiveView(pairs, live); err != nil {
		t.Fatalf("稳定满 liveViewHold 应放行: %v", err)
	}
	clock.Advance(testReconcileInterval)
	if err := s.checkLiveView(pairs, live); err != nil {
		t.Fatalf("放行之后接着收尾: %v", err)
	}
	if err := s.checkLiveView(pairs, liveSet("a", "b", "c", "d")); err != nil {
		t.Fatalf("恢复之后应放行: %v", err)
	}
	if err := s.checkLiveView(pairs, live); !errors.Is(err, errLiveViewDrop) {
		t.Fatalf("恢复之后再次骤降要重新压住: %v", err)
	}
}

// 压住期间有 gate 回来、但仍过半(etcd 恢复中):计时从头算。
func TestCheckLiveView_ChangeWhileDroppedRestartsTheClock(t *testing.T) {
	clock := newTestClock()
	s := newViewTestSweeper(clock)
	pairs := viewPairs("a", "b", "c", "d", "e")
	require.ErrorIs(t, s.checkLiveView(pairs, liveSet("a")), errLiveViewDrop)
	clock.Advance(20 * time.Minute)
	require.ErrorIs(t, s.checkLiveView(pairs, liveSet("a", "b")), errLiveViewDrop, "还缺三个,仍过半")
	clock.Advance(liveViewHold - time.Second)
	require.ErrorIs(t, s.checkLiveView(pairs, liveSet("a", "b")), errLiveViewDrop, "从回来那一刻起还不满")
	clock.Advance(time.Second)
	require.NoError(t, s.checkLiveView(pairs, liveSet("a", "b")))
}

// 恢复到一半以下就放行:剩下的缺席 gate 还要连续两拍缺席才判死,在去抖窗口里重新注册就不会被处理。
// 要缺席清零才放行的话,gate 滚动发布里一次短暂过半就会把清扫压住整整 liveViewHold。
func TestCheckLiveView_RecoveryBelowHalfReleasesToTheDebounce(t *testing.T) {
	clock := newTestClock()
	s := newViewTestSweeper(clock)
	pairs := viewPairs("a", "b", "c", "d", "e")
	require.ErrorIs(t, s.checkLiveView(pairs, liveSet("a")), errLiveViewDrop)
	clock.Advance(testReconcileInterval)
	require.NoError(t, s.checkLiveView(pairs, liveSet("a", "b", "c")), "只剩两个缺席(不过半):放行")
	clock.Advance(testReconcileInterval)
	require.ErrorIs(t, s.checkLiveView(pairs, liveSet("a")), errLiveViewDrop, "再次过半要重新压住")
}

// 小集群挂一台、大集群挂不到一半都是日常,不压。同一 gate 在多个 slot 上只算一次。
func TestCheckLiveView_OrdinaryLossPasses(t *testing.T) {
	s := newViewTestSweeper(newTestClock())
	if err := s.checkLiveView(viewPairs("a", "b"), liveSet("a")); err != nil {
		t.Fatalf("两台挂一台: %v", err)
	}
	if err := s.checkLiveView(viewPairs("a", "b", "c", "d"), liveSet("a", "b")); err != nil {
		t.Fatalf("四台挂两台不超过一半: %v", err)
	}
	if err := s.checkLiveView(viewPairs("a", "a", "a", "a", "b", "c"), liveSet("a", "b")); err != nil {
		t.Fatalf("按 gate 去重后三台挂一台: %v", err)
	}
}

// 这一拍没分到 slot(登记集为空)不是恢复的证据:不能把压住清零,计时也不打断。
func TestCheckLiveView_NoSlotsKeepsTheHold(t *testing.T) {
	clock := newTestClock()
	s := newViewTestSweeper(clock)
	pairs := viewPairs("a", "b", "c", "d")
	require.ErrorIs(t, s.checkLiveView(pairs, liveSet("a")), errLiveViewDrop)
	clock.Advance(10 * time.Minute)
	require.NoError(t, s.checkLiveView(nil, liveSet("a")), "没分到 slot 的拍没有可判的")
	clock.Advance(10 * time.Minute)
	require.ErrorIs(t, s.checkLiveView(pairs, liveSet("a")), errLiveViewDrop, "回到同一视图,压住还在")
	clock.Advance(liveViewHold - 20*time.Minute)
	require.NoError(t, s.checkLiveView(pairs, liveSet("a")), "计时从第一次压住算起,没被空拍打断")
}

// 回归(2026-11-05):压住的拍以前按 etcd_error 计,连续 3 拍就让出段,下一拍没段可扫时闸门被空观察清零,
// 30 拍的上限永远到不了。现在压住不算失败:副本一直持段,缺席数稳定满 liveViewHold 才放行,再经两拍去抖转换。
func TestRun_HeldRoundsKeepTheShardUntilRelease(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	gates := []string{idxGateA, idxGateB, idxGateC, viewGateD, viewGateE}
	for i, gate := range gates {
		require.NoError(t, callSetSession(sc, indexTestSession(uint64(9601+i), 100, gate, 1)))
	}
	view := newFakeClusterView(idxGateA) // 五个登记的 gate 只剩 A 还在 etcd 里
	clock := newTestClock()
	deps := testReconcilerDeps(view, clock)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	holdRounds := int(liveViewHold / testReconcileInterval)
	var s *gateSweeper
	rounds := 0
	deps.newTicker = func(time.Duration) (func(context.Context) error, func()) {
		return func(ctx context.Context) error {
			rounds++
			assert.True(t, s.eligible.Load(), "第 %d 拍:被闸门压住的拍不能让副本退出分片", rounds)
			if rounds <= holdRounds {
				assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_ONLINE, readSession(t, sc, 9602).State,
					"第 %d 拍:放行之前死 gate 上的会话一个都不动", rounds)
			}
			if rounds == holdRounds+3 {
				cancel()
			}
			clock.Advance(testReconcileInterval)
			return ctx.Err()
		}, func() {}
	}
	s = newGateSweeper(sc, registrySlotsOwner{mr}, testReconcileConf(), testReconcileInterval, deps)
	s.run(ctx)

	// 第 holdRounds+1 拍(距第一次压住满 liveViewHold)放行并挂嫌疑,第 holdRounds+2 拍确认死亡并转换。
	assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_ONLINE, readSession(t, sc, 9601).State, "A 一直在 etcd 里")
	for i := 1; i < len(gates); i++ {
		assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_DISCONNECTING, readSession(t, sc, uint64(9601+i)).State,
			"gate %s 放行之后照常收尾", gates[i])
	}
}
