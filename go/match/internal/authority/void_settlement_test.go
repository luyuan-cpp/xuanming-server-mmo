package authority

import (
	"context"
	"crypto/sha256"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	battlepb "proto/battle"
	pb "proto/match"
	inboxpb "proto/player_inbox"
)

// 作废结算(battle-instance-loss-early-abort.md §5.1):每个 ABORT 在同一事务里给成员写一份 VOID、无效果的个人来源。

func TestVoidSettlementForCarriesOnlyIdentityAndDisplayFields(t *testing.T) {
	f := &pb.MatchFormationRecord{BattleId: 901, Mode: 3, ConfigId: 7}
	want := &battlepb.BattleSettlementData{
		BattleId:        901,
		PlayerId:        11,
		Outcome:         battlepb.EBattleOutcome_BATTLE_OUTCOME_VOID,
		PlayerTeamIndex: 1,
		FinishedAtMs:    12345,
		MatchMode:       3,
		BattleConfigId:  7,
	}
	require.True(t, proto.Equal(want, voidSettlementFor(f, 11, 1, 12345)), "作废结算只带身份、作废结局与展示字段,效果字段必须全空")
}

// 所在方优先取持久 Create 原件里的分边(各模式都有);原件缺失或解不开时用成局成员上记的(只有 PVP_TEAM 持久化)。
// 原件里不属于这一局成员的玩家不进结果。
func TestMemberSidesPreferTheCreateOriginal(t *testing.T) {
	members := []*pb.MatchFormationMember{{PlayerId: 11, TeamIndex: 0}, {PlayerId: 22, TeamIndex: 0}}
	create, err := proto.Marshal(&battlepb.CreateBattleRequest{Players: []*battlepb.BattlePlayerSnapshot{{PlayerId: 11, TeamIndex: 0}, {PlayerId: 22, TeamIndex: 1}, {PlayerId: 33, TeamIndex: 1}}})
	require.NoError(t, err)
	require.Equal(t, map[uint64]uint32{11: 0, 22: 1}, memberSides(&pb.MatchFormationRecord{Members: members, CreateRequest: create}))

	teamMembers := []*pb.MatchFormationMember{{PlayerId: 11, TeamIndex: 1}, {PlayerId: 22, TeamIndex: 0}}
	require.Equal(t, map[uint64]uint32{11: 1, 22: 0}, memberSides(&pb.MatchFormationRecord{Members: teamMembers}), "没有原件:用成员上记的分边")
	require.Equal(t, map[uint64]uint32{11: 1, 22: 0}, memberSides(&pb.MatchFormationRecord{Members: teamMembers, CreateRequest: []byte{0xff}}), "原件解不开:同上")
}

// requireVoidSettlementSources 断言 f 的每名成员各有一行作废结算来源:正文是 voidSettlementFor、摘要对得上、
// 归属取原票、签发时刻与归档时刻相同、还没入箱(inbox_seq=0、dispatch_state=1)。
func requireVoidSettlementSources(t *testing.T, s *Store, f *pb.MatchFormationRecord, wantAccount string, wantRevision uint64) {
	t.Helper()
	ctx := context.Background()
	var count int
	require.NoError(t, s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM match_battle_settlement_source WHERE battle_id=?", f.BattleId).Scan(&count))
	require.Equal(t, len(f.Members), count, "ABORT 给每名成员写一份作废结算")
	sides := memberSides(f)
	for _, m := range f.Members {
		var body, digest []byte
		var account string
		var revision, archived, issued, seq uint64
		var dispatch uint32
		require.NoError(t, s.DB.QueryRowContext(ctx, "SELECT settlement_proto,payload_digest,owner_account,owner_revision,archived_ms,issued_at_ms,inbox_seq,dispatch_state FROM match_battle_settlement_source WHERE battle_id=? AND player_id=?", f.BattleId, m.PlayerId).
			Scan(&body, &digest, &account, &revision, &archived, &issued, &seq, &dispatch))
		decoded := &battlepb.BattleSettlementData{}
		require.NoError(t, proto.Unmarshal(body, decoded))
		require.True(t, proto.Equal(voidSettlementFor(f, m.PlayerId, sides[m.PlayerId], archived), decoded), "正文必须是作废结算:VOID、无效果")
		sum := sha256.Sum256(body)
		require.Equal(t, sum[:], digest)
		require.Equal(t, wantAccount, account)
		require.Equal(t, wantRevision, revision)
		require.Equal(t, archived, issued, "签发时刻就是 ABORT 落库时刻")
		require.Zero(t, seq, "还没入箱")
		require.EqualValues(t, 1, dispatch, "待发件箱首投")
	}
}

// requireOnlyVoidSources 断言这一局的个人结算来源恰好是每名成员一份作废结算(VOID、不带任何效果),没有别的来源。
// 各个 ABORT 入口的用例用它:作废不产生任何个人效果,但每名成员都要收到一份作废结算。归属与摘要由
// requireVoidSettlementSources 在专门的用例里核。
func requireOnlyVoidSources(t *testing.T, s *Store, battleID uint64, members int) {
	t.Helper()
	rows, err := s.DB.QueryContext(context.Background(), "SELECT player_id,settlement_proto FROM match_battle_settlement_source WHERE battle_id=?", battleID)
	require.NoError(t, err)
	defer rows.Close()
	seen := map[uint64]bool{}
	for rows.Next() {
		var player uint64
		var body []byte
		require.NoError(t, rows.Scan(&player, &body))
		decoded := &battlepb.BattleSettlementData{}
		require.NoError(t, proto.Unmarshal(body, decoded))
		want := &battlepb.BattleSettlementData{BattleId: battleID, PlayerId: player, Outcome: battlepb.EBattleOutcome_BATTLE_OUTCOME_VOID,
			PlayerTeamIndex: decoded.PlayerTeamIndex, FinishedAtMs: decoded.FinishedAtMs, MatchMode: decoded.MatchMode, BattleConfigId: decoded.BattleConfigId}
		require.True(t, proto.Equal(want, decoded), "player=%d 的来源必须是作废结算:VOID、不带任何效果", player)
		seen[player] = true
	}
	require.NoError(t, rows.Err())
	require.Len(t, seen, members, "作废的局给每名成员各写一份作废结算,没有别的个人来源")
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
	require.NoError(t, commitAbortTx(ctx, tx, f, stamp, pb.MatchBattleCompletionSourceKind_MATCH_BATTLE_COMPLETION_SOURCE_MATCH_DEADLINE_ABORT))
	require.NoError(t, tx.Commit())

	requireVoidSettlementSources(t, s, f, "original-owner", 1)
	require.Equal(t, map[uint64]uint32{11: 0, 22: 1}, memberSides(f), "夹具的分边在 Create 原件里,成员上没记")
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
	require.NoError(t, commitAbortTx(ctx, tx, f, stamp, pb.MatchBattleCompletionSourceKind_MATCH_BATTLE_COMPLETION_SOURCE_MATCH_DEADLINE_ABORT))
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

// 作废结算走现有个人发件箱:首投入箱、等 APPLIED、销账,与 COMPLETE 的个人原件同一条路(发件箱只按行状态取件,不看终局种类)。
func TestAbortTiDBVoidSettlementFlowsThroughThePersonalOutbox(t *testing.T) {
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
	require.NoError(t, commitAbortTx(ctx, tx, f, stamp, pb.MatchBattleCompletionSourceKind_MATCH_BATTLE_COMPLETION_SOURCE_MATCH_DEADLINE_ABORT))
	require.NoError(t, tx.Commit())
	backlog, err := s.ReadOutcomeOutboxBacklog(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 2, backlog.First.Rows, "两名成员各一行作废结算待首投")
	require.Zero(t, backlog.Result.Rows, "ABORT 没有整局结果要发 Kafka")

	inbox := &terminalInboxFixture{state: inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_APPLIED}
	resolve := func(context.Context) (inboxpb.PlayerInboxClient, error) { return inbox, nil }
	published := 0
	publish := func(context.Context, uint64, []byte) error {
		published++
		return nil
	}
	require.NoError(t, s.PublishBattleOutcomes(ctx, resolve, terminalFixtureSecret, publish, DefaultOutcomeOutboxOptions()))
	require.Zero(t, published)
	require.Equal(t, 2, inbox.enqueues)
	require.ElementsMatch(t, []uint64{11, 22}, inbox.seen, "每名成员各入箱一次")
	var applied int
	require.NoError(t, s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM match_battle_settlement_source WHERE battle_id=? AND dispatch_state=2 AND inbox_state=2 AND inbox_seq>0", f.BattleId).Scan(&applied))
	require.Equal(t, 2, applied, "收件箱回 APPLIED 之后销账")
}
