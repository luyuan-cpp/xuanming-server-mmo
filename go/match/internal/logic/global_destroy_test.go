package logic

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"match/internal/authority"
	pb "proto/match"
)

// 2026-10-22 评审补（social-match-battle#0 评审 must_fix 第 2 条）：globalDestroy 的「无原件不等目标 Battle」短路
// 此前没有会转红的回归。Store 层用例（authority/formation_recovery_exit_test.go）只证明了 CreateIntentPersisted 与
// Finish(false) 的既有前提，把 globalDestroy 改回旧代码它们照样全绿。这里用替身权威直接调生产函数，目标端点指向
// 不监听的本机端口 127.0.0.1:1：不起 TiDB、不起 Battle、不依赖墙钟。

// destroyAuthorityFake 只回答「本代租约下有没有 Create 原件」；任何提交调用都记下来，用例断言为零。
type destroyAuthorityFake struct {
	intentPersisted bool
	intentErr       error
	intentChecks    int
	commits         []string
}

func (f *destroyAuthorityFake) CreateIntentPersisted(context.Context, *pb.MatchFormationRecord) (bool, error) {
	f.intentChecks++
	return f.intentPersisted, f.intentErr
}

func (f *destroyAuthorityFake) RecordBattleAdopted(context.Context, *pb.MatchFormationRecord, string, uint32) error {
	f.commits = append(f.commits, "RecordBattleAdopted")
	return errors.New("fake: unexpected adopted receipt")
}

func (f *destroyAuthorityFake) Finish(context.Context, *pb.MatchFormationRecord, bool) error {
	f.commits = append(f.commits, "Finish")
	return errors.New("fake: unexpected finish")
}

// 目标实例已经消失：端点不再监听（同端点即使起了新进程，UUID 也对不上，Battle 会空回包）。
func vanishedTargetFormation() *pb.MatchFormationRecord {
	return &pb.MatchFormationRecord{BattleId: 2101, OperationId: "original-op", State: 3, TargetInstanceId: "original-instance",
		TargetEndpoint: "127.0.0.1:1", OutcomeProtocolVersion: 1, RetryAtMs: 42}
}

// 无原件：不拨目标 Battle，直接判「原 operation 不会建房」。
// 改回旧代码（删掉 !intentPersisted 短路、要求目标回 ABORTED）时，globalDestroy 会去拨 127.0.0.1:1，DestroyBattle 失败后
// 返回 false，下面第一条 require.True 转红 —— 生产上就是「目标实例消失或换 UUID 时 state 2 票据永久卡住」。
func TestGlobalDestroyWithoutCreateIntentDoesNotWaitForTargetBattle(t *testing.T) {
	store := &destroyAuthorityFake{}
	require.True(t, globalDestroy(store, vanishedTargetFormation()))
	require.Equal(t, 1, store.intentChecks)
	require.Empty(t, store.commits, "判定本身不提交；释放只在 globalAbortPrepared → Finish(false)，那里在行锁内复核租约")
}

// 有原件：Create 可能已在途，仍必须等目标 Battle 回 ABORTED；目标不可达就不能判「不会建房」（fail-closed）。
// 拨号失败最迟在 globalDestroy 自己的 3 秒超时内返回。
func TestGlobalDestroyWithCreateIntentStillRequiresTargetAbort(t *testing.T) {
	store := &destroyAuthorityFake{intentPersisted: true}
	require.False(t, globalDestroy(store, vanishedTargetFormation()))
	require.Equal(t, 1, store.intentChecks)
	require.Empty(t, store.commits)
}

// 租约已被别的副本接手，或读取失败：什么都不能判（判 true 就会让失租的旧 worker 去解冻、回队）。
func TestGlobalDestroyWithoutLeaseDecidesNothing(t *testing.T) {
	for _, err := range []error{authority.ErrWorkLease, context.DeadlineExceeded} {
		store := &destroyAuthorityFake{intentErr: err}
		require.False(t, globalDestroy(store, vanishedTargetFormation()), err)
		require.Equal(t, 1, store.intentChecks)
		require.Empty(t, store.commits)
	}
}
