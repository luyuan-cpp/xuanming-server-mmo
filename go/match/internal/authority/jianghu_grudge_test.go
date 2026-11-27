package authority

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	battlepb "proto/battle"
	kafkapb "proto/contracts/kafka"
	pb "proto/match"
)

func grudgeDuelEvent(battleID, finishedMs uint64) *kafkapb.BattleResultEvent {
	return &kafkapb.BattleResultEvent{
		BattleId:     battleID,
		MatchMode:    uint32(pb.MatchMode_MATCH_MODE_JIANGHU_DUEL),
		Outcome:      battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN,
		TotalRounds:  5,
		FinishedAtMs: finishedMs,
		JianghuDuelContext: &battlepb.JianghuDuelBattleContext{
			WritTier:              1,
			ChallengerCaptainId:   1001,
			ChallengerCaptainName: "QingFeng",
			TargetCaptainId:       2001,
			TargetCaptainName:     "MoYuan",
			TeamSize:              1,
		},
	}
}

// 恩怨录的去重键不能是会被裁掉、还带处理时刻 owner_revision 的战报行(数据覆写审计 r2 第 4 轮 r2-jh#3):
// 一局入账之后同一对宿敌又打了 20 场以上,那一局的战报行被裁掉;它的结果再被重投时,不论这次查到的 owner_revision 是多少
// (原值、过户后的新值、查不到时的 0),胜负与连胜都不能再算一次。
func TestGrudgeLedger_RedeliveryAfterTrimOrTransferStaysDuplicate(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	revs := map[uint64]uint64{1001: 1, 2001: 1}

	const first uint64 = 990001
	_, applied, err := s.ApplyJianghuDuelBattleResult(ctx, grudgeDuelEvent(first, 1_000), revs)
	require.NoError(t, err)
	require.True(t, applied)
	_, applied, err = s.ApplyJianghuDuelBattleResult(ctx, grudgeDuelEvent(first, 1_000), revs)
	require.NoError(t, err)
	require.False(t, applied, "redelivery while the log row is still kept")

	for i := uint64(1); i <= MaxJianghuGrudgeLogsPerRival+1; i++ {
		_, applied, err = s.ApplyJianghuDuelBattleResult(ctx, grudgeDuelEvent(first+i, 1_000+i*1_000), revs)
		require.NoError(t, err)
		require.True(t, applied)
	}
	var kept int
	require.NoError(t, s.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM match_grudge_log WHERE player_id=1001 AND owner_revision=1 AND peer_player_id=2001 AND battle_id=?", first).Scan(&kept))
	require.Zero(t, kept, "test premise: the first battle's log row has been trimmed")

	for name, redeliveryRevs := range map[string]map[uint64]uint64{
		"same owner revision":         revs,
		"owner revision after a sale": {1001: 2, 2001: 1},
		"profile lookup failed (0)":   {},
	} {
		_, applied, err = s.ApplyJianghuDuelBattleResult(ctx, grudgeDuelEvent(first, 1_000), redeliveryRevs)
		require.NoError(t, err, name)
		require.False(t, applied, "%s: redelivery after the log row was trimmed must stay a duplicate", name)
	}

	var wins, winStreak uint32
	require.NoError(t, s.DB.QueryRowContext(ctx,
		"SELECT wins,win_streak FROM match_grudge_relation WHERE player_id=1001 AND owner_revision=1 AND peer_player_id=2001").Scan(&wins, &winStreak))
	require.Equal(t, uint32(MaxJianghuGrudgeLogsPerRival+2), wins, "every battle counted exactly once")
	require.Equal(t, uint32(MaxJianghuGrudgeLogsPerRival+2), winStreak)
	var otherBuckets int
	require.NoError(t, s.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM match_grudge_relation WHERE player_id=1001 AND owner_revision<>1").Scan(&otherBuckets))
	require.Zero(t, otherBuckets, "a rejected redelivery opens no relation under another owner revision")
}
