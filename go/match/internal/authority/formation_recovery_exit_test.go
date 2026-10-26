package authority

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	battlepb "proto/battle"
	pb "proto/match"
)

// 2026-10-22 修复（social-match-battle#0）的回归：全球成局的票据不能因为目标 Battle 实例重启、被替换或
// 失联而永久卡住；释放只认 SQL 上的正面证据，一次查询超时或目标仍在都不能释放，多副本只释放一次。
// 目标 Battle 消失 / 超时在 SQL 层的表现就是"worker 拿不到应答、只能 Unknown 后失去租约"，
// 本文件不起 Battle 进程，也不依赖真实墙钟：截止与租约到期都用数据库当前时刻之前的固定值注入。

// exitFormationFixture 用正式 Join/Claim 建一局两人全球成局（票据 state 2），并补齐终局恢复核对的原账号。
// requeue=true 对应排队来源（发房前失败回原队），false 对应直接开局来源（失败结束原票）。
func exitFormationFixture(t *testing.T, s *Store, battleID uint64, requeue bool) []*pb.MatchAuthorityTicketRecord {
	t.Helper()
	ctx := context.Background()
	first, second := testJoin(t, s, battleID*10+1), testJoin(t, s, battleID*10+2)
	f := &pb.MatchFormationRecord{BattleId: battleID, OperationId: uuid.NewString(), PhysicalRegion: 1, TargetNodeId: 3,
		TargetInstanceId: "original-instance", TargetEndpoint: "127.0.0.1:1", Mode: first.Mode, ConfigId: first.ConfigId, RequeueOnFailure: requeue}
	require.NoError(t, s.Claim(ctx, f, []*pb.MatchAuthorityTicketRecord{first, second}))
	for _, original := range []*pb.MatchAuthorityTicketRecord{first, second} {
		_, err := s.DB.ExecContext(ctx, "UPDATE match_authority_ticket SET owner_account='original-owner',owner_revision=1 WHERE player_id=? AND ticket=?", original.PlayerId, original.Ticket)
		require.NoError(t, err)
	}
	return []*pb.MatchAuthorityTicketRecord{first, second}
}

// persistV1Intent 以本代租约持久完整 v1 Create 原件，等同 gather 发出 Create 之前的提交点。
// expired=true 时把原件的创建 / 截止时刻移到终局宽限之前，模拟已过权威截止与提交宽限。
func persistV1Intent(t *testing.T, s *Store, work *pb.MatchFormationRecord, expired bool) {
	t.Helper()
	ctx := context.Background()
	created, deadline := work.CreatedMs, work.DeadlineMs
	if expired {
		stamp, err := s.Now(ctx)
		require.NoError(t, err)
		created, deadline = stamp-terminalOutcomeGraceMs-2000, stamp-terminalOutcomeGraceMs-1000
		_, err = s.DB.ExecContext(ctx, "UPDATE match_formation SET created_ms=?,deadline_ms=? WHERE battle_id=?", created, deadline, work.BattleId)
		require.NoError(t, err)
	}
	var players []*battlepb.BattlePlayerSnapshot
	for i, m := range work.Members {
		players = append(players, &battlepb.BattlePlayerSnapshot{PlayerId: m.PlayerId, TeamIndex: uint32(i)})
	}
	request := &battlepb.CreateBattleRequest{BattleId: work.BattleId, PlacementOperationId: work.OperationId, TargetInstanceId: work.TargetInstanceId,
		BattleConfigId: work.ConfigId, MatchMode: work.Mode, OutcomeProtocolVersion: 1, CreatedAtMs: created, DeadlineMs: deadline, Players: players}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
	require.NoError(t, err)
	capability := &battlepb.QueryBattleCreationResponse{State: battlepb.BattleCreationState_BATTLE_CREATION_ABSENT, TargetInstanceId: work.TargetInstanceId, SupportedOutcomeProtocolVersion: 1}
	_, err = s.SaveSupportedIntent(ctx, work, raw, capability)
	require.NoError(t, err)
	persisted, err := s.CreateIntentPersisted(ctx, work)
	require.NoError(t, err)
	require.True(t, persisted)
}

// expireWorkLease 模拟持租 worker 随目标 Battle 一起失联：原租约到期，下一个副本才可接手。
func expireWorkLease(t *testing.T, s *Store, battleID uint64) {
	t.Helper()
	_, err := s.DB.ExecContext(context.Background(), "UPDATE match_formation SET retry_at_ms=1 WHERE battle_id=?", battleID)
	require.NoError(t, err)
}

// ticketRow 只读 SQL 原行；Store.Ticket 会顺手终结过期排队票，不适合观察状态机结果。
func ticketRow(t *testing.T, s *Store, pid uint64) *pb.MatchAuthorityTicketRecord {
	t.Helper()
	ticket, err := scanTicket(s.DB.QueryRowContext(context.Background(), "SELECT "+ticketColumns+" FROM match_authority_ticket WHERE player_id=?", pid))
	require.NoError(t, err)
	return ticket
}

// 本用例只证明 Store 层前提：失租旧 worker 再也写不出原件，"无原件"对接手者永久成立，Finish(false) 能按原合同释放。
// Finish(false) 无原件时修复前就能成功，所以它不是 globalDestroy 短路的红灯；那条回归在
// logic/global_destroy_test.go（2026-10-22 评审补）。
func TestFormationExitTiDBUnlaunchedFormationReleasesWithoutTargetBattle(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const battleID = 1101
	tickets := exitFormationFixture(t, s, battleID, true)
	crashed, err := s.AcquireWork(ctx, battleID)
	require.NoError(t, err)
	require.NoError(t, s.Prepared(ctx, crashed, tickets[0].PlayerId, "original-scene"))
	// 原 worker 已冻结一人，探测目标 Battle 能力时目标实例消失，worker 随后失联：没有 Create 原件，租约到期。
	expireWorkLease(t, s, battleID)
	recovered, err := s.AcquireWork(ctx, battleID)
	require.NoError(t, err)
	require.EqualValues(t, 3, recovered.State)
	require.Len(t, recovered.Prepared, 1, "接手者凭持久端点解冻，不重新定位玩家")

	// 失租的旧 worker 再也拿不到发 Create 的资格，"无原件"才是永久成立的证据。
	_, err = s.CreateIntentPersisted(ctx, crashed)
	require.ErrorIs(t, err, ErrWorkLease)
	request := &battlepb.CreateBattleRequest{BattleId: battleID, PlacementOperationId: crashed.OperationId, TargetInstanceId: crashed.TargetInstanceId, OutcomeProtocolVersion: 1, DeadlineMs: crashed.DeadlineMs}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
	require.NoError(t, err)
	_, err = s.SaveSupportedIntent(ctx, crashed, raw, &battlepb.QueryBattleCreationResponse{State: battlepb.BattleCreationState_BATTLE_CREATION_ABSENT, TargetInstanceId: crashed.TargetInstanceId, SupportedOutcomeProtocolVersion: 1})
	require.ErrorIs(t, err, ErrWorkLease)

	persisted, err := s.CreateIntentPersisted(ctx, recovered)
	require.NoError(t, err)
	require.False(t, persisted, "本代租约下无 Create 原件，不需要已消失的目标 Battle 回 ABORTED")
	require.NoError(t, s.Finish(ctx, recovered, false))
	formation, err := s.Formation(ctx, battleID)
	require.NoError(t, err)
	require.EqualValues(t, 5, formation.State)
	for _, original := range tickets {
		current := ticketRow(t, s, original.PlayerId)
		require.EqualValues(t, 1, current.State, "排队来源按发房前失败回原队")
		require.Equal(t, original.Ticket, current.Ticket)
		require.Equal(t, original.EnqueuedMs, current.EnqueuedMs, "保留原等待时长")
		require.Zero(t, current.FormationId)
	}
}

func TestFormationExitTiDBQueryTimeoutBeforeDeadlineKeepsTickets(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const battleID = 1201
	tickets := exitFormationFixture(t, s, battleID, true)
	work, err := s.AcquireWork(ctx, battleID)
	require.NoError(t, err)
	persistV1Intent(t, s, work, false)
	// 原件已持久后查询目标 Battle 超时：worker 只能记 UNKNOWN，Create 可能已在途。
	require.NoError(t, s.Unknown(ctx, work))
	expireWorkLease(t, s, battleID)
	_, _, err = s.RecoverExpiredOutcomes(ctx, 0, 0)
	require.NoError(t, err)
	terminal, err := readTerminal(ctx, s.DB, battleID)
	require.NoError(t, err)
	require.Nil(t, terminal, "截止前的一次超时不是终局证据")

	retry, err := s.AcquireWork(ctx, battleID)
	require.NoError(t, err, "截止前仍由下一个 worker 继续查询原实例")
	persisted, err := s.CreateIntentPersisted(ctx, retry)
	require.NoError(t, err)
	require.True(t, persisted, "有原件时必须走目标 Battle 应答路径，超时即不释放")
	require.Error(t, s.Finish(ctx, retry, false), "有 Create 原件的失败收尾只能等唯一 SQL 终局")
	for _, original := range tickets {
		current := ticketRow(t, s, original.PlayerId)
		require.EqualValues(t, 2, current.State)
		require.EqualValues(t, battleID, current.FormationId)
	}
}

func TestFormationExitTiDBPresentTargetKeepsReadyTickets(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const battleID = 1301
	tickets := exitFormationFixture(t, s, battleID, false)
	work, err := s.AcquireWork(ctx, battleID)
	require.NoError(t, err)
	persistV1Intent(t, s, work, false)
	// 目标 Battle 仍在并回 PRESENT：按成功恢复进入 ready，战斗未出终局前任何巡检都不释放。
	require.NoError(t, s.RecordBattleAdopted(ctx, work, work.TargetInstanceId, 1))
	require.NoError(t, s.Finish(ctx, work, true))
	_, _, err = s.RecoverExpiredOutcomes(ctx, 0, 0)
	require.NoError(t, err)
	terminal, err := readTerminal(ctx, s.DB, battleID)
	require.NoError(t, err)
	require.Nil(t, terminal)
	_, err = s.AcquireWork(ctx, battleID)
	require.ErrorIs(t, err, ErrWorkLease, "已建成的成局不再领工作租约")
	for _, original := range tickets {
		current := ticketRow(t, s, original.PlayerId)
		require.EqualValues(t, 3, current.State)
		released, err := s.ReleaseCompletedReady(ctx, current)
		require.NoError(t, err)
		require.False(t, released, "没有本人到账收据不能释放 ready 票")
		require.EqualValues(t, 3, ticketRow(t, s, original.PlayerId).State)
	}
}

func TestFormationExitTiDBExpiredIntentLeavesOnlyThroughSQLAbort(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const battleID = 1401
	tickets := exitFormationFixture(t, s, battleID, false)
	work, err := s.AcquireWork(ctx, battleID)
	require.NoError(t, err)
	persistV1Intent(t, s, work, true)
	require.NoError(t, s.Unknown(ctx, work))
	// 原租约仍有效时到期恢复不抢占。
	_, _, err = s.RecoverExpiredOutcomes(ctx, 0, 0)
	require.NoError(t, err)
	terminal, err := readTerminal(ctx, s.DB, battleID)
	require.NoError(t, err)
	require.Nil(t, terminal)

	expireWorkLease(t, s, battleID)
	// 过截止后不再发租约、也不进候选：否则每次接手都把 retry_at 推后 120 秒，到期出口被一直推迟。
	_, err = s.AcquireWork(ctx, battleID)
	require.ErrorIs(t, err, ErrAwaitingExpiredOutcome)
	pending, err := s.Pending(ctx)
	require.NoError(t, err)
	for _, f := range pending {
		require.NotEqual(t, uint64(battleID), f.BattleId)
	}
	_, _, err = s.RecoverExpiredOutcomes(ctx, 0, 0)
	require.NoError(t, err)
	terminal, err = readTerminal(ctx, s.DB, battleID)
	require.NoError(t, err)
	require.NotNil(t, terminal)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, terminal.Decision)
	formation, err := s.Formation(ctx, battleID)
	require.NoError(t, err)
	require.EqualValues(t, 5, formation.State)
	for _, original := range tickets {
		require.EqualValues(t, 4, ticketRow(t, s, original.PlayerId).State, "唯一 SQL ABORT 之后原票才结束")
	}
}

func TestFormationExitTiDBConcurrentReplicasReleaseOnce(t *testing.T) {
	type outcome struct {
		released bool
		err      error
	}
	t.Run("无原件", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		const battleID = 1501
		tickets := exitFormationFixture(t, s, battleID, true)
		crashed, err := s.AcquireWork(ctx, battleID)
		require.NoError(t, err)
		expireWorkLease(t, s, battleID)
		replica := *s
		start := make(chan struct{})
		results := make(chan outcome, 2)
		// 两个副本各自按 runGlobalFormation 的顺序接手：领租约 → 判原件 → 发房前失败收尾。
		// 守住的是 AcquireWork 互斥这一既有前提在新出口下仍成立（只释放一次），不是修复本身的红灯。
		for _, store := range []*Store{s, &replica} {
			go func(store *Store) {
				<-start
				work, err := store.AcquireWork(ctx, battleID)
				if err != nil {
					results <- outcome{err: err}
					return
				}
				persisted, err := store.CreateIntentPersisted(ctx, work)
				if err != nil || persisted {
					results <- outcome{err: err}
					return
				}
				results <- outcome{released: true, err: store.Finish(ctx, work, false)}
			}(store)
		}
		close(start)
		releases := 0
		for range 2 {
			r := <-results
			if r.released {
				require.NoError(t, r.err)
				releases++
				continue
			}
			require.ErrorIs(t, r.err, ErrWorkLease, "落败副本只能拿到租约冲突")
		}
		require.Equal(t, 1, releases)
		require.ErrorIs(t, s.Finish(ctx, crashed, false), ErrWorkLease, "迟到的原 worker 不能二次收尾")
		for _, original := range tickets {
			current := ticketRow(t, s, original.PlayerId)
			require.EqualValues(t, 1, current.State)
			require.Equal(t, original.Ticket, current.Ticket)
			require.Zero(t, current.FormationId)
		}
	})
	t.Run("过截止原件", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		const battleID = 1502
		tickets := exitFormationFixture(t, s, battleID, false)
		work, err := s.AcquireWork(ctx, battleID)
		require.NoError(t, err)
		persistV1Intent(t, s, work, true)
		require.NoError(t, s.Unknown(ctx, work))
		expireWorkLease(t, s, battleID)
		replica := *s
		start := make(chan struct{})
		results := make(chan error, 2)
		for _, store := range []*Store{s, &replica} {
			go func(store *Store) {
				<-start
				_, _, err := store.RecoverExpiredOutcomes(ctx, 0, 0)
				results <- err
			}(store)
		}
		close(start)
		for range 2 {
			require.NoError(t, <-results)
		}
		var rows int
		require.NoError(t, s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM match_battle_completion WHERE battle_id=?", battleID).Scan(&rows))
		require.Equal(t, 1, rows)
		terminal, err := readTerminal(ctx, s.DB, battleID)
		require.NoError(t, err)
		require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, terminal.Decision)
		for _, original := range tickets {
			require.EqualValues(t, 4, ticketRow(t, s, original.PlayerId).State)
		}
	})
}

// 2026-10-22 评审补：过截止、有 v1 原件、但 RecoverExpiredOutcomes 候选形状不合格的成局，被 AcquireWork 门禁挡住后
// 再没有任何出口和信号。巡检（logic/global_expired_audit.go）靠 CountStrandedExpiredFormations 把它们数出来；
// 形状合格的（由到期恢复收尾）与还没过截止的都不算。
func TestFormationExitTiDBStrandedExpiredFormationsAreCounted(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const recoverable, stranded, notExpired = 1601, 1602, 1603
	for _, id := range []uint64{recoverable, stranded, notExpired} {
		exitFormationFixture(t, s, id, false)
		work, err := s.AcquireWork(ctx, id)
		require.NoError(t, err)
		persistV1Intent(t, s, work, id != notExpired)
	}
	// 能力证据与目标实例对不上（例如有人手改过 target_instance_id）：到期恢复的候选形状不再成立。
	for _, id := range []uint64{stranded, notExpired} {
		_, err := s.DB.ExecContext(ctx, "UPDATE match_formation SET preflight_instance_id='other-instance' WHERE battle_id=?", id)
		require.NoError(t, err)
	}
	count, err := s.CountStrandedExpiredFormations(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, count, "只有过截止且形状不合格的那一局")

	// 两局租约都到期：形状合格的一局由到期恢复收尾；不合格的一局领不到租约、也不会被到期恢复选中，仍然计入。
	expireWorkLease(t, s, recoverable)
	expireWorkLease(t, s, stranded)
	_, err = s.AcquireWork(ctx, stranded)
	require.ErrorIs(t, err, ErrAwaitingExpiredOutcome)
	_, _, err = s.RecoverExpiredOutcomes(ctx, 0, 0)
	require.NoError(t, err)
	terminal, err := readTerminal(ctx, s.DB, stranded)
	require.NoError(t, err)
	require.Nil(t, terminal, "形状不合格的成局不会被到期恢复写终局")
	closed, err := s.Formation(ctx, recoverable)
	require.NoError(t, err)
	require.EqualValues(t, 5, closed.State)
	count, err = s.CountStrandedExpiredFormations(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, count)
}
