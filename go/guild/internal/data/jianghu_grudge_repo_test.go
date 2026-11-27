package data

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	battlepb "proto/battle"
	kafkapb "proto/contracts/kafka"
	matchpb "proto/match"
)

func guildDuelEvent(battleID, finishedMs uint64) *kafkapb.BattleResultEvent {
	return &kafkapb.BattleResultEvent{
		BattleId:     battleID,
		MatchMode:    uint32(matchpb.MatchMode_MATCH_MODE_JIANGHU_DUEL),
		Outcome:      battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN,
		TotalRounds:  5,
		FinishedAtMs: finishedMs,
		JianghuDuelContext: &battlepb.JianghuDuelBattleContext{
			WritTier:              1,
			ChallengerCaptainId:   1001,
			ChallengerCaptainName: "QingFeng",
			TargetCaptainId:       2001,
			TargetCaptainName:     "MoYuan",
			SideAGuildId:          7101,
			SideAGuildName:        "QingYun",
			SideBGuildId:          7102,
			SideBGuildName:        "CangLan",
			TeamSize:              3,
		},
	}
}

// 恩怨墙的去重键不能是会被裁掉的战绩行(数据覆写审计 r2 第 4 轮 r2-jh#3):一局入账之后两帮又打了 20 场以上,
// 那一局的战绩行被裁掉;它的结果再被重投(位点回拨、换消费组重读)时,汇总不能再加一次。
func TestRecordJianghuDuelResultStaysIdempotentAfterRecordTrimmed(t *testing.T) {
	dsn := os.Getenv("GUILD_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("GUILD_TEST_MYSQL_DSN 未设置，跳过帮派恩怨墙入账闸门测试")
	}
	db, err := sql.Open("mysql", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	require.NoError(t, db.PingContext(ctx))
	resetGuildSchemaViaMigrate(t, ctx, db)
	repo := NewGuildRepo(nil, db, time.Minute)

	const first uint64 = 880001
	applied, err := repo.RecordJianghuDuelResult(ctx, guildDuelEvent(first, 1_000))
	require.NoError(t, err)
	require.True(t, applied)
	applied, err = repo.RecordJianghuDuelResult(ctx, guildDuelEvent(first, 1_000))
	require.NoError(t, err)
	require.False(t, applied, "redelivery while the record row is still on the wall")

	for i := uint64(1); i <= MaxGuildJianghuWallRecords+1; i++ {
		applied, err = repo.RecordJianghuDuelResult(ctx, guildDuelEvent(first+i, 1_000+i*1_000))
		require.NoError(t, err)
		require.True(t, applied)
	}
	var onWall int
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM guild_jianghu_grudge_record WHERE guild_id=? AND battle_id=?", 7101, first).Scan(&onWall))
	require.Zero(t, onWall, "test premise: the first battle has been trimmed off the wall")

	applied, err = repo.RecordJianghuDuelResult(ctx, guildDuelEvent(first, 1_000))
	require.NoError(t, err)
	require.False(t, applied, "redelivery after the record row was trimmed must stay a duplicate")

	var wins, losses uint32
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT wins FROM guild_jianghu_rival_summary WHERE guild_id=? AND rival_guild_id=?", 7101, 7102).Scan(&wins))
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT losses FROM guild_jianghu_rival_summary WHERE guild_id=? AND rival_guild_id=?", 7102, 7101).Scan(&losses))
	require.Equal(t, uint32(MaxGuildJianghuWallRecords+2), wins, "every battle counted exactly once")
	require.Equal(t, uint32(MaxGuildJianghuWallRecords+2), losses)
	var onWallAgain int
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM guild_jianghu_grudge_record WHERE guild_id=? AND battle_id=?", 7101, first).Scan(&onWallAgain))
	require.Zero(t, onWallAgain, "the rejected redelivery left no stale row behind")
}
