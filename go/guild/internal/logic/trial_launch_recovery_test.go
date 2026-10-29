package logic

// 2026-10-23 修复（审计 r2-guild#1）回归：历练 launch 恢复器只凭权威终态释放占用。
// 开局成功（match formation=4）后战斗不出结果的局，恢复器每轮重放原 Start 的签名取消：截止前 match 只回 formation=4，
// RPC 也可能超时；只有 match 在权威截止后原子提交 SQL ABORT（回 formation=5、TerminalDecision=ABORT）才终结 launch。
// 仓储与取消适配器都是同一接口的替身（AGENTS §11.4），不连库、不起 match；时间注入 ActivityDeps.Now，不读墙钟。
// 仓储层的同一规则与真库释放由 data/trial_launch_abort_tidb_test.go 覆盖。

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"guild/internal/data"
	pb "proto/guild"
	matchpb "proto/match"
)

// launchRecoveryStore 在 fakeTrialStore 之上补 trialLaunchStore 的四个方法。
// FinishCancelledTrialLaunch 与生产仓储同一前置条件(data.TrialCancelReceiptIsTerminal)，终结后原件不再出现在待恢复页
// (生产仓储只扫 state 1/2)。只在测试 goroutine 里同步调用，不另加锁。
type launchRecoveryStore struct {
	*fakeTrialStore
	pending  []*pb.GuildTrialLaunchOriginal
	finished []*matchpb.CancelActivityBattleResponse
}

func (s *launchRecoveryStore) TrialLaunchEnabled() bool { return true }

func (s *launchRecoveryStore) PrepareTrialLaunch(context.Context, uint64, data.TrialBattleKey, uint32, []*pb.GuildTrialRoleIdentity, uint64) (*pb.GuildTrialLaunchOriginal, error) {
	return nil, errors.New("launch recovery never prepares a new original")
}

func (s *launchRecoveryStore) PendingTrialLaunches(_ context.Context, after uint64, limit int) ([]*pb.GuildTrialLaunchOriginal, error) {
	var page []*pb.GuildTrialLaunchOriginal
	for _, original := range s.pending {
		if original.RequestId > after && len(page) < limit {
			page = append(page, original)
		}
	}
	return page, nil
}

func (s *launchRecoveryStore) FinishCancelledTrialLaunch(_ context.Context, original *pb.GuildTrialLaunchOriginal, receipt *matchpb.CancelActivityBattleResponse, _ uint64) error {
	if !data.TrialCancelReceiptIsTerminal(receipt) {
		return data.ErrWriteConflict
	}
	s.finished = append(s.finished, proto.Clone(receipt).(*matchpb.CancelActivityBattleResponse))
	s.pending = slices.DeleteFunc(s.pending, func(o *pb.GuildTrialLaunchOriginal) bool { return o.RequestId == original.RequestId })
	return nil
}

var _ trialLaunchStore = (*launchRecoveryStore)(nil)

// scriptedCancellation 按顺序回一串取消结果(最后一个重复)，并记下调用次数。
type scriptedCancellation struct {
	replies []scriptedCancelReply
	calls   int
}

type scriptedCancelReply struct {
	receipt *matchpb.CancelActivityBattleResponse
	err     error
}

func (c *scriptedCancellation) CancelOriginal(_ context.Context, original *matchpb.StartActivityBattleRequest) (*matchpb.CancelActivityBattleResponse, error) {
	if original == nil || original.RequestId == 0 {
		return nil, errors.New("recovery must replay the original start")
	}
	reply := c.replies[min(c.calls, len(c.replies)-1)]
	c.calls++
	if reply.err != nil {
		return nil, reply.err
	}
	return proto.Clone(reply.receipt).(*matchpb.CancelActivityBattleResponse), nil
}

func startedTrialLaunchOriginal() *pb.GuildTrialLaunchOriginal {
	return &pb.GuildTrialLaunchOriginal{RequestId: 381, GuildId: 44, ActivityId: 3, PeriodKey: 20261023, GuildPeriodKey: 20261023,
		InitiatorPlayerId: 11, DungeonId: 1, CreatedMs: 1_700_000_000_000,
		Roster: []*pb.GuildTrialRoleIdentity{{PlayerId: 11, Account: "seller", OwnerRevision: 1}, {PlayerId: 22, Account: "seller", OwnerRevision: 1}}}
}

func TestTrialLaunchRecoveryReleasesStartedBattleOnlyOnSQLAbort(t *testing.T) {
	const battleID = 900
	complete := matchpb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE
	abort := &matchpb.CancelActivityBattleResponse{BattleId: battleID, FormationState: 5, TerminalDecision: matchpb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT}
	cancel := &scriptedCancellation{replies: []scriptedCancelReply{
		// 第 1 轮：一次 RPC 超时，没有任何证明。
		{err: context.DeadlineExceeded},
		// 第 2 轮：开局成功、截止前。
		{receipt: &matchpb.CancelActivityBattleResponse{BattleId: battleID, FormationState: 4}},
		// 第 3 轮：已 COMPLETE，等结果事件。
		{receipt: &matchpb.CancelActivityBattleResponse{BattleId: battleID, FormationState: 4, TerminalDecision: complete}},
		// 第 4 轮：自相矛盾的回执。
		{receipt: &matchpb.CancelActivityBattleResponse{BattleId: battleID, FormationState: 5, TerminalDecision: complete}},
		// 第 5 轮：截止后 match 原子提交 SQL ABORT。
		{receipt: abort},
	}}
	store := &launchRecoveryStore{fakeTrialStore: &fakeTrialStore{}, pending: []*pb.GuildTrialLaunchOriginal{startedTrialLaunchOriginal()}}
	deps := &ActivityDeps{trials: store, Cancellation: cancel, Now: func() time.Time { return time.UnixMilli(1_700_000_100_000) }}
	recovery := &trialLaunchRecovery{d: deps}
	ctx := context.Background()

	// 前四轮：超时、formation=4、COMPLETE、矛盾回执，都不释放；formation=4 的两轮只做幂等登记。
	for round := 1; round <= 4; round++ {
		recovery.after = 0
		recovery.round(ctx)
		require.Equal(t, round, cancel.calls)
		require.Empty(t, store.finished, "第 %d 轮不是权威终态，占用必须保留", round)
		require.Len(t, store.pending, 1)
	}
	require.Len(t, store.registered, 2, "formation=4 的两轮各登记一次(仓储幂等)")
	for _, key := range store.registered {
		require.EqualValues(t, battleID, key.BattleID)
		require.EqualValues(t, 381, key.LaunchRequestID)
	}

	// 第五轮拿到 SQL ABORT：恰好终结一次，回执原样交给仓储同事务落库。
	recovery.after = 0
	recovery.round(ctx)
	require.Len(t, store.finished, 1)
	require.True(t, proto.Equal(abort, store.finished[0]))
	require.Empty(t, store.pending)

	// 之后的轮次扫不到已终结的原件，不再重放取消、不再释放。
	recovery.after = 0
	recovery.round(ctx)
	require.Equal(t, 5, cancel.calls)
	require.Len(t, store.finished, 1)
}
