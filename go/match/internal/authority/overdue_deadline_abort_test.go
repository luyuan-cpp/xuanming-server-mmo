package authority

// 「超期未作废」的巡检计数(CountOverdueDeadlineAborts,2026-11-11)。真 TiDB,沿用 terminalResultFixture
// (MATCH_IT_TIDB / MATCH_IT_REQUIRE_TIDB)。计数只看 SQL 列(候选形状、截止、终局行),所以直接改 deadline_ms 模拟超期,
// 不经持久 Create 原件。

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	pb "proto/match"
)

// 刚过截止加宽限、还在等证据的局不算;截止之后超过最晚落定时刻(deadlineAbortOverdueMs)才算,并报最早一局超期多久;
// 已有同一原件的终局行就不再算。
func TestOverdueDeadlineAbortsTiDBCountOnlyPastTheLatestSettleTime(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	f, req := terminalResultFixture(t, s, true)
	count, oldest, err := s.CountOverdueDeadlineAborts(ctx)
	require.NoError(t, err)
	require.Zero(t, count, "刚过截止加宽限,还在最晚落定时刻之内")
	require.Zero(t, oldest)

	const overdueBy uint64 = 5000
	stamp, err := s.Now(ctx)
	require.NoError(t, err)
	_, err = s.DB.ExecContext(ctx, "UPDATE match_formation SET deadline_ms=? WHERE battle_id=?", stamp-deadlineAbortOverdueMs-overdueBy, f.BattleId)
	require.NoError(t, err)
	count, oldest, err = s.CountOverdueDeadlineAborts(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, count)
	require.GreaterOrEqual(t, oldest, overdueBy, "超期多久从最晚落定时刻起算")
	require.Less(t, oldest, overdueBy+60*1000, "两次读 SQL 时刻之间不会隔一分钟")

	// 落了终局就不算:先改回原截止让 COMPLETE 能过原件核对,提交之后再改成超期。
	_, err = s.DB.ExecContext(ctx, "UPDATE match_formation SET deadline_ms=? WHERE battle_id=?", f.DeadlineMs, f.BattleId)
	require.NoError(t, err)
	decision, err := s.CommitBattleOutcome(ctx, req)
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE, decision)
	_, err = s.DB.ExecContext(ctx, "UPDATE match_formation SET deadline_ms=? WHERE battle_id=?", stamp-deadlineAbortOverdueMs-overdueBy, f.BattleId)
	require.NoError(t, err)
	count, _, err = s.CountOverdueDeadlineAborts(ctx)
	require.NoError(t, err)
	require.Zero(t, count, "已有终局行")
}
