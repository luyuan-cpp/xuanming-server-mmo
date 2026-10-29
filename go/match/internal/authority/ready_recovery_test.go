package authority

import (
	"context"
	"database/sql"
	"errors"
	"os"
	pb "proto/match"
	inboxpb "proto/player_inbox"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestReadyTiDBExpiredOutcomeRecoveryScansLostReplyAndReady(t *testing.T) {
	for _, state := range []uint32{2, 3, 4} {
		t.Run(string(rune('0'+state)), func(t *testing.T) {
			s := testStore(t)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			f, request := terminalResultFixture(t, s, true)
			if state == 4 {
				require.NoError(t, s.Finish(ctx, f, true), "成功开房的真实公开收尾进入ready")
			} else if state == 3 {
				require.NoError(t, s.Unknown(ctx, f), "回包丢失的真实公开收尾保留UNKNOWN")
			}
			_, err := s.DB.ExecContext(ctx, "UPDATE match_formation SET retry_at_ms=0 WHERE battle_id=?", f.BattleId)
			require.NoError(t, err)
			// 不调用Guild取消或Battle RPC；正式公开巡检必须自行产生唯一SQL ABORT。
			after, battle, err := s.RecoverExpiredOutcomes(ctx, 0, 0)
			require.NoError(t, err)
			require.Equal(t, uint64(0), after)
			require.Equal(t, f.BattleId, battle)
			row, err := readTerminal(ctx, s.DB, f.BattleId)
			require.NoError(t, err)
			require.NotNil(t, row)
			require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, row.Decision)
			for _, player := range []uint64{11, 22} {
				ticket, err := s.Ticket(ctx, player)
				require.NoError(t, err)
				require.Equal(t, uint32(4), ticket.State)
			}
			decision, err := s.CommitBattleOutcome(ctx, request)
			require.NoError(t, err)
			require.Equal(t, row.Decision, decision, "迟到COMPLETE不能越过已提交ABORT")
			var people int
			require.NoError(t, s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM match_battle_settlement_source").Scan(&people))
			require.Zero(t, people)
		})
	}
}

func TestReadyTiDBExpiredOutcomeRecoveryKeepsUnknownAndSkipsBlockedPrefix(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	f, _ := terminalResultFixture(t, s, true)
	_, err := s.DB.ExecContext(ctx, "UPDATE match_formation SET state=4,retry_at_ms=0 WHERE battle_id=?", f.BattleId)
	require.NoError(t, err)
	// 八个未决但损坏的v1原件不能永久挡住第九个可恢复原件。
	for id := uint64(880); id < 888; id++ {
		_, err = s.DB.ExecContext(ctx, "INSERT INTO match_formation("+formationColumns+") SELECT ?,"+strings.TrimPrefix(formationColumns, "battle_id,")+" FROM match_formation WHERE battle_id=?", id, f.BattleId)
		require.NoError(t, err)
	}
	_, err = s.DB.ExecContext(ctx, "UPDATE match_formation SET members=? WHERE battle_id=880", []byte{0xff})
	require.NoError(t, err)
	after, battle, err := s.RecoverExpiredOutcomes(ctx, 0, 0)
	require.Error(t, err, "完整原件损坏必须如实报告，但不能挡住后续页")
	require.Equal(t, uint64(887), battle)
	row, err := readTerminal(ctx, s.DB, f.BattleId)
	require.NoError(t, err)
	require.Nil(t, row, "一次批次最多八条")
	_, _, err = s.RecoverExpiredOutcomes(ctx, after, battle)
	require.NoError(t, err)
	row, err = readTerminal(ctx, s.DB, f.BattleId)
	require.NoError(t, err)
	require.NotNil(t, row)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, row.Decision)
	var oldDecisions int
	require.NoError(t, s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM match_battle_completion WHERE battle_id<901").Scan(&oldDecisions))
	require.Zero(t, oldDecisions)
}

func TestReadyTiDBExpiredOutcomeRecoveryHonorsDeadlineLeaseAndObservedZero(t *testing.T) {
	for _, shape := range []string{"未到期", "工作租约仍有效", "实际采用0", "历史协议0", "已有COMPLETE"} {
		t.Run(shape, func(t *testing.T) {
			s := testStore(t)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			f, request := terminalResultFixture(t, s, shape != "未到期")
			if shape == "已有COMPLETE" {
				_, err := s.CommitBattleOutcome(ctx, request)
				require.NoError(t, err)
			}
			lease := uint64(0)
			if shape == "工作租约仍有效" {
				lease = f.RetryAtMs
			}
			_, err := s.DB.ExecContext(ctx, "UPDATE match_formation SET retry_at_ms=? WHERE battle_id=?", lease, f.BattleId)
			require.NoError(t, err)
			if shape == "实际采用0" {
				require.NoError(t, s.RecordBattleAdopted(ctx, f, f.TargetInstanceId, 0))
			} else if shape == "历史协议0" {
				_, err = s.DB.ExecContext(ctx, "UPDATE match_formation SET outcome_protocol_version=0 WHERE battle_id=?", f.BattleId)
				require.NoError(t, err)
			}
			before, err := s.Formation(ctx, f.BattleId)
			require.NoError(t, err)
			var beforeTickets []*pb.MatchAuthorityTicketRecord
			for _, player := range []uint64{11, 22} {
				ticket, err := s.Ticket(ctx, player)
				require.NoError(t, err)
				beforeTickets = append(beforeTickets, ticket)
			}
			afterRetry, afterBattle, err := s.RecoverExpiredOutcomes(ctx, 0, 0)
			require.NoError(t, err)
			require.Zero(t, afterRetry, "不具备候选资格或已终局不占恢复页")
			require.Zero(t, afterBattle)
			after, err := s.Formation(ctx, f.BattleId)
			require.NoError(t, err)
			if shape == "已有COMPLETE" {
				before.RetryAtMs = terminalOutcomeNoRetryMs // 只允许退休历史调度元数据。
			}
			require.True(t, proto.Equal(before, after), "巡检不能改写不具备ABORT资格的原件")
			for i, player := range []uint64{11, 22} {
				ticket, err := s.Ticket(ctx, player)
				require.NoError(t, err)
				require.True(t, proto.Equal(beforeTickets[i], ticket), "不具备ABORT资格不能释放或改写原票")
			}
			row, err := readTerminal(ctx, s.DB, f.BattleId)
			require.NoError(t, err)
			if shape == "已有COMPLETE" {
				require.NotNil(t, row)
				require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE, row.Decision)
			} else {
				require.Nil(t, row)
			}
			if shape == "实际采用0" || shape == "历史协议0" {
				decision, err := s.RecoverExpiredOutcome(ctx, before)
				require.NoError(t, err)
				require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, decision)
			}
		})
	}
}

func TestReadyTiDBCompletedHistoryDoesNotConsumeRecoveryPageOrRewriteOutbox(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	f, request := terminalResultFixture(t, s, true)
	decision, err := s.CommitBattleOutcome(ctx, request)
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE, decision)
	completed, err := s.Formation(ctx, f.BattleId)
	require.NoError(t, err)
	require.Equal(t, terminalOutcomeNoRetryMs, completed.RetryAtMs, "新COMPLETE同事务退出终局恢复调度")
	// 模拟旧版本COMPLETE保留普通调度值；公开入口只退休此字段，不重写任何业务原件。
	_, err = s.DB.ExecContext(ctx, "UPDATE match_formation SET retry_at_ms=0 WHERE battle_id=?", f.BattleId)
	require.NoError(t, err)
	terminalBefore, err := readTerminal(ctx, s.DB, f.BattleId)
	require.NoError(t, err)
	readPeople := func() []*pb.MatchBattleSettlementSourceRecord {
		rows, err := s.DB.QueryContext(ctx, "SELECT battle_id,player_id,settlement_proto,payload_digest,archived_ms,owner_account,owner_revision,issued_at_ms,inbox_seq,inbox_state,dispatch_state,next_retry_ms FROM match_battle_settlement_source WHERE battle_id=? ORDER BY player_id", f.BattleId)
		require.NoError(t, err)
		defer rows.Close()
		var people []*pb.MatchBattleSettlementSourceRecord
		for rows.Next() {
			r := &pb.MatchBattleSettlementSourceRecord{}
			require.NoError(t, rows.Scan(&r.BattleId, &r.PlayerId, &r.SettlementProto, &r.PayloadDigest, &r.ArchivedMs, &r.OwnerAccount, &r.OwnerRevision, &r.IssuedAtMs, &r.InboxSeq, &r.InboxState, &r.DispatchState, &r.NextRetryMs))
			people = append(people, r)
		}
		require.NoError(t, rows.Err())
		return people
	}
	peopleBefore := readPeople()
	require.Len(t, peopleBefore, 2)
	var resultPublishedBefore, resultRetryBefore uint64
	require.NoError(t, s.DB.QueryRowContext(ctx, "SELECT result_published,next_retry_ms FROM match_battle_completion WHERE battle_id=?", f.BattleId).Scan(&resultPublishedBefore, &resultRetryBefore))
	afterRetry, afterBattle, err := s.RecoverExpiredOutcomes(ctx, 0, 0)
	require.NoError(t, err)
	require.Zero(t, afterRetry)
	require.Zero(t, afterBattle, "历史成功局不占用八局恢复候选页")
	after, err := s.Formation(ctx, f.BattleId)
	require.NoError(t, err)
	require.True(t, proto.Equal(completed, after), "只恢复调度退休标记，state/deadline/Create和回执均不改")
	terminalAfter, err := readTerminal(ctx, s.DB, f.BattleId)
	require.NoError(t, err)
	require.True(t, proto.Equal(terminalBefore, terminalAfter))
	peopleAfter := readPeople()
	require.Len(t, peopleAfter, len(peopleBefore))
	for i := range peopleBefore {
		require.True(t, proto.Equal(peopleBefore[i], peopleAfter[i]), "终局调度退休不能清除或重投个人outbox")
	}
	var resultPublishedAfter, resultRetryAfter uint64
	require.NoError(t, s.DB.QueryRowContext(ctx, "SELECT result_published,next_retry_ms FROM match_battle_completion WHERE battle_id=?", f.BattleId).Scan(&resultPublishedAfter, &resultRetryAfter))
	require.Equal(t, resultPublishedBefore, resultPublishedAfter)
	require.Equal(t, resultRetryBefore, resultRetryAfter)
	decision, err = s.CommitBattleOutcome(ctx, request)
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE, decision, "调度退休不能破坏原字节回执幂等")
}

func TestReadyTiDBExpiredOutcomeRecoveryIsIdempotentAcrossIndependentPools(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	f, _ := terminalResultFixture(t, s, true)
	_, err := s.DB.ExecContext(ctx, "UPDATE match_formation SET state=4,retry_at_ms=0 WHERE battle_id=?", f.BattleId)
	require.NoError(t, err)
	f, err = s.Formation(ctx, f.BattleId)
	require.NoError(t, err)
	var database string
	require.NoError(t, s.DB.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&database))
	cfg := mysql.NewConfig()
	cfg.Net, cfg.Addr, cfg.User, cfg.DBName = "tcp", os.Getenv("MATCH_IT_TIDB"), "root", database
	cfg.Timeout, cfg.ReadTimeout, cfg.WriteTimeout = 3*time.Second, 3*time.Second, 3*time.Second
	cfg.Params = map[string]string{"tidb_txn_mode": "'pessimistic'", "transaction_isolation": "'READ-COMMITTED'", "tidb_read_staleness": "0", "tidb_low_resolution_tso": "OFF", "tidb_read_consistency": "'strict'", "tidb_replica_read": "'leader'", "tidb_snapshot": "''"}
	peer := *s
	peer.DB, err = sql.Open("mysql", cfg.FormatDSN())
	require.NoError(t, err)
	defer peer.DB.Close()
	a, err := s.DB.Conn(ctx)
	require.NoError(t, err)
	b, err := peer.DB.Conn(ctx)
	require.NoError(t, err)
	var firstID, secondID uint64
	require.NoError(t, a.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&firstID))
	require.NoError(t, b.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&secondID))
	require.NotEqual(t, firstID, secondID)
	require.NoError(t, a.Close())
	require.NoError(t, b.Close())
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, store := range []*Store{s, &peer} {
		go func(store *Store) {
			<-start
			decision, err := store.RecoverExpiredOutcome(ctx, f)
			if err == nil && decision != pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT {
				err = errors.New("独立副本未重放同一ABORT")
			}
			results <- err
		}(store)
	}
	close(start)
	require.NoError(t, <-results)
	require.NoError(t, <-results)
	var decisions int
	require.NoError(t, s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM match_battle_completion WHERE battle_id=? AND decision=2", f.BattleId).Scan(&decisions))
	require.Equal(t, 1, decisions)
}

func TestReadyTiDBRuntimeAbortCannotReleasePersistedCreateBeforeSQLDecision(t *testing.T) {
	for _, kind := range []string{"协议1", "旧协议0", "明确采用0"} {
		t.Run(kind, func(t *testing.T) {
			s := testStore(t)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			f, _ := terminalResultFixture(t, s, true)
			if kind == "旧协议0" {
				_, err := s.DB.ExecContext(ctx, "UPDATE match_formation SET outcome_protocol_version=0 WHERE battle_id=?", f.BattleId)
				require.NoError(t, err)
			} else if kind == "明确采用0" {
				require.NoError(t, s.RecordBattleAdopted(ctx, f, f.TargetInstanceId, 0))
			}
			f, err := s.Formation(ctx, f.BattleId)
			require.NoError(t, err)
			require.ErrorContains(t, s.Finish(ctx, f, false), "唯一SQL终局", "runtime ABORTED不能替代持久终局")
			after, err := s.Formation(ctx, f.BattleId)
			require.NoError(t, err)
			require.True(t, proto.Equal(f, after))
			for _, player := range []uint64{11, 22} {
				ticket, err := s.Ticket(ctx, player)
				require.NoError(t, err)
				require.Equal(t, uint32(2), ticket.State)
				require.Equal(t, f.BattleId, ticket.FormationId)
			}
			_, err = s.DB.ExecContext(ctx, "UPDATE match_formation SET retry_at_ms=0 WHERE battle_id=?", f.BattleId)
			require.NoError(t, err)
			_, _, err = s.RecoverExpiredOutcomes(ctx, 0, 0)
			require.NoError(t, err)
			row, err := readTerminal(ctx, s.DB, f.BattleId)
			require.NoError(t, err)
			if kind == "协议1" {
				require.NotNil(t, row)
				require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, row.Decision)
			} else {
				require.Nil(t, row)
			}
		})
	}
}

func TestReadyTiDBHistoricalStateFiveNeverRepairsFromNewRoundTicket(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	f, _ := terminalResultFixture(t, s, true)
	_, err := s.DB.ExecContext(ctx, "UPDATE match_formation SET state=5,retry_at_ms=0 WHERE battle_id=?", f.BattleId)
	require.NoError(t, err)
	_, err = s.DB.ExecContext(ctx, "UPDATE match_authority_ticket SET ticket='new-round',formation_id=902,state=3 WHERE player_id=11")
	require.NoError(t, err)
	f, err = s.Formation(ctx, f.BattleId)
	require.NoError(t, err)
	decision, err := s.RecoverExpiredOutcome(ctx, f)
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, decision)
	ticket, err := s.Ticket(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, "new-round", ticket.Ticket)
	require.Equal(t, uint64(902), ticket.FormationId)
	require.Equal(t, uint32(3), ticket.State)
	row, err := readTerminal(ctx, s.DB, f.BattleId)
	require.NoError(t, err)
	require.Nil(t, row)
}

func TestReadyTiDBScanDoesNotStarveAfterUnknownPrefix(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for pid := uint64(1); pid <= 19; pid++ {
		ticket := testJoin(t, s, pid)
		_, err := s.DB.ExecContext(ctx, "UPDATE match_authority_ticket SET state=3,formation_id=?,enqueued_ms=100 WHERE player_id=? AND ticket=?", 900+pid, pid, ticket.Ticket)
		require.NoError(t, err)
	}
	var seen []uint64
	var afterMs, afterPlayer uint64
	for page := 0; page < 4; page++ {
		rows, err := s.ReadyAfter(ctx, afterMs, afterPlayer)
		require.NoError(t, err)
		require.LessOrEqual(t, len(rows), 8)
		for _, row := range rows {
			// 不释放任何前项，模拟其权威房间一直UNKNOWN。
			seen = append(seen, row.PlayerId)
			afterMs, afterPlayer = row.EnqueuedMs, row.PlayerId
		}
	}
	require.Len(t, seen, 19)
	require.Equal(t, uint64(19), seen[len(seen)-1])
}

func TestReadyTiDBCompletionEvidenceAndOriginalTicketCAS(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	_, request := terminalResultFixture(t, s, false)
	ticket, err := s.Ticket(ctx, 11)
	require.NoError(t, err)
	ticket.State = 3
	released, err := s.ReleaseCompletedReady(ctx, ticket)
	require.NoError(t, err)
	require.False(t, released, "未收到真实结算原件时不能按时间释放")
	_, err = s.CommitBattleOutcome(ctx, request)
	require.NoError(t, err)
	released, err = s.ReleaseCompletedReady(ctx, ticket)
	require.NoError(t, err)
	require.False(t, released, "仅COMPLETE与来源归档不等于个人到账")
	client := &terminalInboxFixture{state: inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_PENDING}
	resolve := func(context.Context) (inboxpb.PlayerInboxClient, error) { return client, nil }
	publish := func(context.Context, uint64, []byte) error { return nil }
	require.NoError(t, s.PublishBattleOutcomesOnce(ctx, resolve, terminalFixtureSecret, publish))
	released, err = s.ReleaseCompletedReady(ctx, ticket)
	require.NoError(t, err)
	require.False(t, released, "入箱PENDING仍占用ready")
	client.state = inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_APPLIED
	_, err = s.DB.ExecContext(ctx, "UPDATE match_battle_settlement_source SET next_retry_ms=0")
	require.NoError(t, err)
	require.NoError(t, s.PublishBattleOutcomesOnce(ctx, resolve, terminalFixtureSecret, publish))
	released, err = s.ReleaseCompletedReady(ctx, ticket)
	require.NoError(t, err)
	require.True(t, released)
	// 同一个玩家下一轮已持有另一原票，旧扫描不能释放它。
	_, err = s.DB.ExecContext(ctx, "UPDATE match_authority_ticket SET state=3,ticket='next-round',formation_id=902 WHERE player_id=11")
	require.NoError(t, err)
	released, err = s.ReleaseCompletedReady(ctx, ticket)
	require.NoError(t, err)
	require.False(t, released)
	var state uint32
	require.NoError(t, s.DB.QueryRowContext(ctx, "SELECT state FROM match_authority_ticket WHERE player_id=11").Scan(&state))
	require.Equal(t, uint32(3), state)
}
