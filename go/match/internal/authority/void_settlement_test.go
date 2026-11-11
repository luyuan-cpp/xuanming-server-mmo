package authority

import (
	"context"
	"crypto/sha256"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	battlepb "proto/battle"
	pb "proto/match"
)

// 作废结算(battle-instance-loss-early-abort.md §5.1):每个 ABORT 在同一事务里给成员写一份 VOID、无效果的个人来源。

func TestVoidSettlementForCarriesOnlyIdentityAndDisplayFields(t *testing.T) {
	f := &pb.MatchFormationRecord{BattleId: 901, Mode: 3, ConfigId: 7}
	m := &pb.MatchFormationMember{PlayerId: 11, Ticket: "ticket-11", TeamIndex: 1}
	want := &battlepb.BattleSettlementData{
		BattleId:        901,
		PlayerId:        11,
		Outcome:         battlepb.EBattleOutcome_BATTLE_OUTCOME_VOID,
		PlayerTeamIndex: 1,
		FinishedAtMs:    12345,
		MatchMode:       3,
		BattleConfigId:  7,
	}
	require.True(t, proto.Equal(want, voidSettlementFor(f, m, 12345)), "作废结算只带身份、作废结局与展示字段,效果字段必须全空")
}

// requireVoidSettlementSources 断言 f 的每名成员各有一行作废结算来源:正文是 voidSettlementFor、摘要对得上、
// 归属取原票、签发时刻与归档时刻相同、还没入箱(inbox_seq=0、dispatch_state=1)。
func requireVoidSettlementSources(t *testing.T, s *Store, f *pb.MatchFormationRecord, wantAccount string, wantRevision uint64) {
	t.Helper()
	ctx := context.Background()
	var count int
	require.NoError(t, s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM match_battle_settlement_source WHERE battle_id=?", f.BattleId).Scan(&count))
	require.Equal(t, len(f.Members), count, "ABORT 给每名成员写一份作废结算")
	for _, m := range f.Members {
		var body, digest []byte
		var account string
		var revision, archived, issued, seq uint64
		var dispatch uint32
		require.NoError(t, s.DB.QueryRowContext(ctx, "SELECT settlement_proto,payload_digest,owner_account,owner_revision,archived_ms,issued_at_ms,inbox_seq,dispatch_state FROM match_battle_settlement_source WHERE battle_id=? AND player_id=?", f.BattleId, m.PlayerId).
			Scan(&body, &digest, &account, &revision, &archived, &issued, &seq, &dispatch))
		decoded := &battlepb.BattleSettlementData{}
		require.NoError(t, proto.Unmarshal(body, decoded))
		require.True(t, proto.Equal(voidSettlementFor(f, m, archived), decoded), "正文必须是作废结算:VOID、无效果")
		sum := sha256.Sum256(body)
		require.Equal(t, sum[:], digest)
		require.Equal(t, wantAccount, account)
		require.Equal(t, wantRevision, revision)
		require.Equal(t, archived, issued, "签发时刻就是 ABORT 落库时刻")
		require.Zero(t, seq, "还没入箱")
		require.EqualValues(t, 1, dispatch, "待发件箱首投")
	}
}

func TestAbortTiDBWritesVoidSettlementPerMember(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	fixture, _ := terminalResultFixture(t, s, true)
	f, err := s.Formation(ctx, fixture.BattleId)
	require.NoError(t, err)
	tx, err := s.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	stamp, err := now(ctx, tx)
	require.NoError(t, err)
	require.NoError(t, commitAbortTx(ctx, tx, f, stamp, pb.MatchBattleCompletionSourceKind_MATCH_BATTLE_COMPLETION_SOURCE_MATCH_INSTANCE_GONE_ABORT))
	require.NoError(t, tx.Commit())

	requireVoidSettlementSources(t, s, f, "original-owner", 1)
	var released int
	require.NoError(t, s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM match_authority_ticket WHERE formation_id=? AND state=4", f.BattleId).Scan(&released))
	require.Equal(t, len(f.Members), released, "原票照旧在 ABORT 里直接放回,不等作废 APPLIED")
}

func TestAbortTiDBSkipsVoidSettlementWithoutOriginalTicket(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	fixture, _ := terminalResultFixture(t, s, true)
	f, err := s.Formation(ctx, fixture.BattleId)
	require.NoError(t, err)
	// 坏数据:22 的原票不再指向这一局。他没有归属可投,只有 11 拿到作废结算;ABORT 本身照常提交。
	_, err = s.DB.ExecContext(ctx, "UPDATE match_authority_ticket SET formation_id=0 WHERE player_id=22")
	require.NoError(t, err)
	tx, err := s.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	stamp, err := now(ctx, tx)
	require.NoError(t, err)
	require.NoError(t, commitAbortTx(ctx, tx, f, stamp, pb.MatchBattleCompletionSourceKind_MATCH_BATTLE_COMPLETION_SOURCE_MATCH_INSTANCE_GONE_ABORT))
	require.NoError(t, tx.Commit())

	var players []uint64
	rows, err := s.DB.QueryContext(ctx, "SELECT player_id FROM match_battle_settlement_source WHERE battle_id=? ORDER BY player_id", f.BattleId)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var id uint64
		require.NoError(t, rows.Scan(&id))
		players = append(players, id)
	}
	require.NoError(t, rows.Err())
	require.Equal(t, []uint64{11}, players)
	row, err := readTerminal(ctx, s.DB, f.BattleId)
	require.NoError(t, err)
	require.NotNil(t, row)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, row.Decision)
}
