package authority

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	battlepb "proto/battle"
	kafkapb "proto/contracts/kafka"
	pb "proto/match"
)

func validReplay() *battlepb.BattleReplayEnvelope {
	return &battlepb.BattleReplayEnvelope{FormatVersion: 1, Codec: battlepb.EBattleReplayCodec_BATTLE_REPLAY_CODEC_GZIP, RawBytes: 64, Payload: []byte{0x1f, 0x8b, 0x08, 0x00, 0x01}}
}

func TestBattleReplayDropReason(t *testing.T) {
	require.Equal(t, "", battleReplayDropReason(validReplay()))
	cases := []struct {
		name   string
		mutate func(*battlepb.BattleReplayEnvelope)
		want   string
	}{
		{"未知格式版本", func(e *battlepb.BattleReplayEnvelope) { e.FormatVersion = 2 }, BattleReportDropInvalidEnvelope},
		{"未压缩", func(e *battlepb.BattleReplayEnvelope) { e.Codec = battlepb.EBattleReplayCodec_BATTLE_REPLAY_CODEC_NONE }, BattleReportDropInvalidEnvelope},
		{"缺原始大小", func(e *battlepb.BattleReplayEnvelope) { e.RawBytes = 0 }, BattleReportDropInvalidEnvelope},
		{"空正文", func(e *battlepb.BattleReplayEnvelope) { e.Payload = nil }, BattleReportDropInvalidEnvelope},
		{"原始超限", func(e *battlepb.BattleReplayEnvelope) { e.RawBytes = maxBattleReplayRawBytes + 1 }, BattleReportDropTooLarge},
		{"压缩后超限", func(e *battlepb.BattleReplayEnvelope) { e.Payload = make([]byte, maxBattleReplayPayloadBytes+1) }, BattleReportDropTooLarge},
	}
	for _, c := range cases {
		e := validReplay()
		c.mutate(e)
		require.Equal(t, c.want, battleReplayDropReason(e), c.name)
	}
	// 恰好等于上限仍然合格。
	e := validReplay()
	e.RawBytes, e.Payload = maxBattleReplayRawBytes, make([]byte, maxBattleReplayPayloadBytes)
	require.Equal(t, "", battleReplayDropReason(e))
}

func TestBattleReportSummaryOrdersMembersAndCopiesResult(t *testing.T) {
	original := &battlepb.CreateBattleRequest{Players: []*battlepb.BattlePlayerSnapshot{
		{PlayerId: 30, TeamIndex: 1, PlayerName: "丙", Level: 3, ClassId: 2},
		{PlayerId: 20, TeamIndex: 0, PlayerName: "乙", Level: 2, ClassId: 1},
		{PlayerId: 10, TeamIndex: 1, PlayerName: "甲", Level: 1, ClassId: 3},
	}}
	event := &kafkapb.BattleResultEvent{BattleId: 7, MatchMode: uint32(pb.MatchMode_MATCH_MODE_1V1), BattleConfigId: 5, FinishedAtMs: 1234,
		Outcome: battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_B_WIN, TotalRounds: 9}
	s := battleReportSummary(original, event, true)
	var ids []uint64
	for _, m := range s.Members {
		ids = append(ids, m.PlayerId)
	}
	require.Equal(t, []uint64{20, 10, 30}, ids, "按 (team_index, player_id) 升序")
	require.Equal(t, "甲", s.Members[1].Name)
	require.Equal(t, uint32(3), s.Members[1].ClassId)
	require.Equal(t, pb.MatchMode_MATCH_MODE_1V1, s.Mode)
	require.Equal(t, uint64(7), s.BattleId)
	require.Equal(t, uint32(5), s.BattleConfigId)
	require.Equal(t, uint64(1234), s.FinishedAtMs)
	require.Equal(t, battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_B_WIN, s.Outcome)
	require.Equal(t, uint32(9), s.TotalRounds)
	require.True(t, s.HasReplay)
}

// enableReplayRecording 把夹具成局的建房原件改成「本局录回放」,并补上名字和等级(列表条目从这里取)。
func enableReplayRecording(t *testing.T, s *Store, f *pb.MatchFormationRecord) {
	t.Helper()
	original := &battlepb.CreateBattleRequest{}
	require.NoError(t, proto.Unmarshal(f.CreateRequest, original))
	original.RecordReplay = true
	for _, p := range original.Players {
		p.PlayerName = fmt.Sprintf("玩家%d", p.PlayerId)
		p.Level = 10
	}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(original)
	require.NoError(t, err)
	_, err = s.DB.ExecContext(context.Background(), "UPDATE match_formation SET create_request=? WHERE battle_id=?", raw, f.BattleId)
	require.NoError(t, err)
}

func countRows(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, s.DB.QueryRowContext(context.Background(), query, args...).Scan(&n))
	return n
}

func TestBattleReportTiDBCommitStoresReportWithOutcome(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	f, r := terminalResultFixture(t, s, false)
	enableReplayRecording(t, s, f)
	r.Replay = validReplay()
	d, err := s.CommitBattleOutcome(ctx, r)
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE, d)
	for _, player := range []uint64{11, 22} {
		reports, err := s.BattleReports(ctx, player)
		require.NoError(t, err)
		require.Len(t, reports, 1)
		require.Equal(t, f.BattleId, reports[0].BattleId)
		require.True(t, reports[0].HasReplay)
		require.Len(t, reports[0].Members, 2)
		require.Equal(t, "玩家11", reports[0].Members[0].Name)
		envelope, err := s.BattleReport(ctx, player, f.BattleId)
		require.NoError(t, err)
		require.True(t, proto.Equal(r.Replay, envelope))
	}
	_, err = s.BattleReport(ctx, 33, f.BattleId)
	require.ErrorIs(t, err, ErrBattleReportNotFound, "不是参战者")
	require.Equal(t, 2, countRows(t, s, "SELECT ref_count FROM match_battle_report WHERE battle_id=?", f.BattleId))

	// 同字节重放:回 COMPLETE,不重复写。
	d, err = s.CommitBattleOutcome(ctx, r)
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE, d)
	require.Equal(t, 2, countRows(t, s, "SELECT COUNT(*) FROM match_player_battle_report WHERE battle_id=?", f.BattleId))
	require.Equal(t, 1, countRows(t, s, "SELECT COUNT(*) FROM match_battle_report WHERE battle_id=?", f.BattleId))

	// 换一份回放重放:回执照旧 COMPLETE(只记 mismatch),库里那份不变。
	changed := proto.Clone(r).(*pb.CommitBattleOutcomeRequest)
	changed.Replay.Payload = append(changed.Replay.Payload, 0x02)
	d, err = s.CommitBattleOutcome(ctx, changed)
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE, d)
	envelope, err := s.BattleReport(ctx, 11, f.BattleId)
	require.NoError(t, err)
	require.True(t, proto.Equal(r.Replay, envelope))
}

func TestBattleReportTiDBNotRequestedReplayIsDroppedOutcomeCommits(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	f, r := terminalResultFixture(t, s, false) // 原件 record_replay = false
	r.Replay = validReplay()
	d, err := s.CommitBattleOutcome(ctx, r)
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE, d)
	require.Zero(t, countRows(t, s, "SELECT COUNT(*) FROM match_battle_report WHERE battle_id=?", f.BattleId))
	require.Zero(t, countRows(t, s, "SELECT COUNT(*) FROM match_player_battle_report WHERE battle_id=?", f.BattleId))
}

func TestBattleReportTiDBInvalidReplayKeepsListEntryWithoutBody(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	f, r := terminalResultFixture(t, s, false)
	enableReplayRecording(t, s, f)
	r.Replay = validReplay()
	r.Replay.Codec = battlepb.EBattleReplayCodec_BATTLE_REPLAY_CODEC_NONE
	d, err := s.CommitBattleOutcome(ctx, r)
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE, d)
	reports, err := s.BattleReports(ctx, 11)
	require.NoError(t, err)
	require.Len(t, reports, 1)
	require.False(t, reports[0].HasReplay)
	require.Zero(t, countRows(t, s, "SELECT COUNT(*) FROM match_battle_report WHERE battle_id=?", f.BattleId))
	_, err = s.BattleReport(ctx, 11, f.BattleId)
	require.ErrorIs(t, err, ErrBattleReportNotFound)

	// 两名参战者都有列表行,只是没有正文。
	require.Equal(t, 2, countRows(t, s, "SELECT COUNT(*) FROM match_player_battle_report WHERE battle_id=?", f.BattleId))
}

// writeReportBattle 直接在一个事务里调 writeBattleReportTx,模拟一局已通过终局核对的 PVP 对局。
func writeReportBattle(t *testing.T, s *Store, battleID, stamp uint64, withReplay bool, players ...uint64) {
	t.Helper()
	ctx := context.Background()
	original := &battlepb.CreateBattleRequest{BattleId: battleID, RecordReplay: true}
	event := &kafkapb.BattleResultEvent{BattleId: battleID, MatchMode: uint32(pb.MatchMode_MATCH_MODE_1V1), FinishedAtMs: stamp, Outcome: battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN}
	for i, player := range players {
		original.Players = append(original.Players, &battlepb.BattlePlayerSnapshot{PlayerId: player, TeamIndex: uint32(i % 2)})
		event.Settlements = append(event.Settlements, &battlepb.BattleSettlementData{BattleId: battleID, PlayerId: player, PlayerTeamIndex: uint32(i % 2)})
	}
	create, err := proto.Marshal(original)
	require.NoError(t, err)
	var replay *battlepb.BattleReplayEnvelope
	if withReplay {
		replay = validReplay()
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = writeBattleReportTx(ctx, tx, &pb.MatchFormationRecord{BattleId: battleID, CreateRequest: create}, event, replay, stamp)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
}

func TestBattleReportTiDBKeepsNewestPerPlayerAndReleasesBodies(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const base = uint64(1_000_000)
	for i := uint64(1); i <= BattleReportsKeptPerPlayer; i++ {
		writeReportBattle(t, s, 1000+i, base+i, true, 11, 22)
	}
	require.Equal(t, BattleReportsKeptPerPlayer, countRows(t, s, "SELECT COUNT(*) FROM match_player_battle_report WHERE player_id=11"))

	// 第 101 局只有 11 和 33:11 最旧的 1001 被挤出;22 还列着它,正文保留,ref_count 2 → 1。
	writeReportBattle(t, s, 2001, base+101, true, 11, 33)
	reports, err := s.BattleReports(ctx, 11)
	require.NoError(t, err)
	require.Len(t, reports, BattleReportsKeptPerPlayer)
	require.Equal(t, uint64(2001), reports[0].BattleId, "新的在前")
	require.Equal(t, uint64(1002), reports[len(reports)-1].BattleId)
	_, err = s.BattleReport(ctx, 11, 1001)
	require.ErrorIs(t, err, ErrBattleReportNotFound, "挤出之后本人看不到")
	_, err = s.BattleReport(ctx, 22, 1001)
	require.NoError(t, err, "另一名参战者还列着它")
	require.Equal(t, 1, countRows(t, s, "SELECT ref_count FROM match_battle_report WHERE battle_id=1001"))

	// 22 再打一局:22 最旧的 1001 也被挤出,没人再列着它,正文删掉。
	writeReportBattle(t, s, 2002, base+102, true, 22, 44)
	require.Zero(t, countRows(t, s, "SELECT COUNT(*) FROM match_battle_report WHERE battle_id=1001"))
	_, err = s.BattleReport(ctx, 22, 1001)
	require.ErrorIs(t, err, ErrBattleReportNotFound)
	require.Equal(t, BattleReportsKeptPerPlayer, countRows(t, s, "SELECT COUNT(*) FROM match_player_battle_report WHERE player_id=22"))

	// 没存正文的局被挤出时,释放只影响 0 行,不报错。
	writeReportBattle(t, s, 3001, base+103, false, 55, 66)
	for i := uint64(1); i <= BattleReportsKeptPerPlayer; i++ {
		writeReportBattle(t, s, 4000+i, base+200+i, true, 55, 66)
	}
	require.Zero(t, countRows(t, s, "SELECT COUNT(*) FROM match_player_battle_report WHERE battle_id=3001"))
}
