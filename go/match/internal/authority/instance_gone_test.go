package authority

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"match/internal/metrics"
	pb "proto/match"
)

// 2026-11-11:原执行进程确认已不在时的提前作废(instance_gone.go)的锁内回归,真 TiDB。锁外的取证与复核在
// logic/global_instance_loss_test.go(假时钟、替身权威)。

func instanceGoneProofOf(f *pb.MatchFormationRecord) InstanceGoneProof {
	return InstanceGoneProof{OperationId: f.OperationId, TargetInstanceId: f.TargetInstanceId}
}

// 截止之前就能写:同一事务写 ABORT(来源 MATCH_INSTANCE_GONE_ABORT、没有 Battle 原字节)、formation→5、全员原票→4、
// 每名成员一份作废结算,计一次 abort_instance_gone。重放只读到既有 ABORT、不重复计数;迟到的 COMPLETE 只能读到 ABORT。
func TestInstanceGoneTiDBAbortsBeforeTheDeadlineAndVoidsEveryMember(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	fixture, lateComplete := terminalResultFixture(t, s, false) // 截止在 5 分钟之后
	f, pending, err := s.InstanceLossCandidate(ctx, fixture.BattleId)
	require.NoError(t, err)
	require.True(t, pending, "在途、原件可裁决、还没有终局:归提前作废管")
	stamp, err := s.Now(ctx)
	require.NoError(t, err)
	require.Greater(t, f.DeadlineMs, stamp, "前提:还没到权威截止")

	counted := metrics.GlobalFlowValue("terminal", terminalAbortInstanceGone)
	decision, err := s.AbortInstanceGone(ctx, f, instanceGoneProofOf(f))
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, decision)
	require.Equal(t, 1, terminalRows(t, s, f.BattleId))
	var formatVersion, sourceKind uint32
	require.NoError(t, s.DB.QueryRowContext(ctx, "SELECT format_version,source_kind FROM match_battle_completion WHERE battle_id=?", f.BattleId).Scan(&formatVersion, &sourceKind))
	require.Zero(t, formatVersion, "没有 Battle 原字节")
	require.EqualValues(t, pb.MatchBattleCompletionSourceKind_MATCH_BATTLE_COMPLETION_SOURCE_MATCH_INSTANCE_GONE_ABORT, sourceKind, "与截止裁决分开审计")
	closed, err := s.Formation(ctx, f.BattleId)
	require.NoError(t, err)
	require.EqualValues(t, 5, closed.State)
	for _, m := range f.Members {
		require.EqualValues(t, 4, ticketRow(t, s, m.PlayerId).State, "原票与 ABORT 同一事务结束")
	}
	requireVoidSettlementSources(t, s, f, "original-owner", 1)
	require.Equal(t, counted+1, metrics.GlobalFlowValue("terminal", terminalAbortInstanceGone))

	_, pending, err = s.InstanceLossCandidate(ctx, f.BattleId)
	require.NoError(t, err)
	require.False(t, pending, "已有终局:不再归提前作废管")
	decision, err = s.AbortInstanceGone(ctx, f, instanceGoneProofOf(f))
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, decision, "重放读到既有 ABORT")
	require.Equal(t, counted+1, metrics.GlobalFlowValue("terminal", terminalAbortInstanceGone), "回放不计数")
	requireOnlyVoidSources(t, s, f.BattleId, len(f.Members))

	late, err := s.CommitBattleOutcome(ctx, lateComplete)
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, late, "迟到的 COMPLETE 只能读到这份 ABORT")
	require.Equal(t, 1, terminalRows(t, s, f.BattleId))
}

// 证据必须指向这一局写定的原操作与原实例:缺字段或与调用方手里的成局不符直接报错;已经有 COMPLETE 的局回 COMPLETE、什么都不改。
func TestInstanceGoneTiDBNeedsTheOriginalIdentityAndRespectsACommittedComplete(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	fixture, complete := terminalResultFixture(t, s, false)
	f, err := s.Formation(ctx, fixture.BattleId)
	require.NoError(t, err)

	for why, proof := range map[string]InstanceGoneProof{
		"缺原操作":  {TargetInstanceId: f.TargetInstanceId},
		"缺原实例":  {OperationId: f.OperationId},
		"别的实例":  {OperationId: f.OperationId, TargetInstanceId: "other-instance"},
		"别的原操作": {OperationId: "other-operation", TargetInstanceId: f.TargetInstanceId},
	} {
		_, err = s.AbortInstanceGone(ctx, f, proof)
		require.Error(t, err, why)
	}
	require.Zero(t, terminalRows(t, s, f.BattleId), "证据不符:什么都不写")

	decision, err := s.CommitBattleOutcome(ctx, complete)
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE, decision)
	_, pending, err := s.InstanceLossCandidate(ctx, f.BattleId)
	require.NoError(t, err)
	require.False(t, pending)
	decision, err = s.AbortInstanceGone(ctx, f, instanceGoneProofOf(f))
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE, decision, "已有 COMPLETE:回放它,不作废")
	require.Equal(t, 2, settlementSources(t, s, f.BattleId), "COMPLETE 的个人来源原样保留")

	_, pending, err = s.InstanceLossCandidate(ctx, 987654321)
	require.NoError(t, err)
	require.False(t, pending, "没有这一局成局")
}

// 提前作废与 COMPLETE 争同一把锁和同一个终局主键:并发时只落一份终局,双方读到的是同一份决定;
// ABORT 赢时全员是作废结算,COMPLETE 赢时是它的个人原件。
func TestInstanceGoneTiDBAndCompleteHaveOneWinner(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	fixture, complete := terminalResultFixture(t, s, false)
	f, err := s.Formation(ctx, fixture.BattleId)
	require.NoError(t, err)
	replica := *s
	type result struct {
		decision pb.BattleOutcomeDecision
		err      error
	}
	var committed, aborted result
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		committed.decision, committed.err = s.CommitBattleOutcome(ctx, complete)
	}()
	go func() {
		defer wg.Done()
		<-start
		aborted.decision, aborted.err = replica.AbortInstanceGone(ctx, f, instanceGoneProofOf(f))
	}()
	close(start)
	wg.Wait()
	require.NoError(t, committed.err)
	require.NoError(t, aborted.err)
	require.Equal(t, 1, terminalRows(t, s, f.BattleId))
	terminal, err := readTerminal(ctx, s.DB, f.BattleId)
	require.NoError(t, err)
	require.Equal(t, terminal.Decision, committed.decision)
	require.Equal(t, terminal.Decision, aborted.decision)
	switch terminal.Decision {
	case pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE:
		require.Equal(t, 2, settlementSources(t, s, f.BattleId))
	case pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT:
		requireOnlyVoidSources(t, s, f.BattleId, len(f.Members))
	default:
		t.Fatalf("unexpected terminal decision %v", terminal.Decision)
	}
}
