package authority

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	battlepb "proto/battle"
	pb "proto/match"
	inboxpb "proto/player_inbox"
)

// 2026-10-23 修复（审计 r2-battle#2、r2-guild#1）的回归：开局成功（formation=4、票 state 3）之后 battle 停机作废或崩溃，
// 这一局再也不会有 CommitBattleOutcome。它的唯一出口是权威截止之后、在原 P→formation 锁内与 COMPLETE 争同一主键的
// SQL ABORT：formation 4→5、全员原票置 4（同一事务），整局作废、没有任何个人结算来源，不会出现「部分成员已结算」。
// 截止前任何巡检或取消都不释放（一次超时 / 实例失联不是终局证明）；多副本同时恢复只写一份 ABORT；迟到的 COMPLETE
// 只能读到 ABORT。活动局另经帮会的签名取消取得同一份 ABORT 回执，guild 据此释放 launch（guild 侧回归见
// go/guild/internal/data/trial_launch_abort_tidb_test.go）。
// 复用 formation_recovery_exit_test.go 的夹具；不起 Battle、不依赖真实墙钟，截止用数据库当前时刻之前的固定值注入。

// expireStartedFormation 同时改写已持久 v1 原件与 SQL 时刻，模拟这一局已过权威截止与终局提交宽限。
// formation 行与 Create 原件里的两个时刻必须一致（supportedOutcomeOriginal 逐项核对），所以连原件一起重写。
func expireStartedFormation(t *testing.T, s *Store, battleID uint64) {
	t.Helper()
	ctx := context.Background()
	f, err := s.Formation(ctx, battleID)
	require.NoError(t, err)
	request := &battlepb.CreateBattleRequest{}
	require.NoError(t, proto.Unmarshal(f.CreateRequest, request))
	stamp, err := s.Now(ctx)
	require.NoError(t, err)
	request.CreatedAtMs, request.DeadlineMs = stamp-terminalOutcomeGraceMs-2000, stamp-terminalOutcomeGraceMs-1000
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
	require.NoError(t, err)
	_, err = s.DB.ExecContext(ctx, "UPDATE match_formation SET created_ms=?,deadline_ms=?,create_request=? WHERE battle_id=?", request.CreatedAtMs, request.DeadlineMs, raw, battleID)
	require.NoError(t, err)
}

// startFormation 走 gather 的成功收尾：持久 v1 原件 → 记下 Battle 回执的实际采用版本 → Finish(true)。
func startFormation(t *testing.T, s *Store, battleID uint64) {
	t.Helper()
	ctx := context.Background()
	work, err := s.AcquireWork(ctx, battleID)
	require.NoError(t, err)
	persistV1Intent(t, s, work, false)
	require.NoError(t, s.RecordBattleAdopted(ctx, work, work.TargetInstanceId, 1))
	require.NoError(t, s.Finish(ctx, work, true))
	f, err := s.Formation(ctx, battleID)
	require.NoError(t, err)
	require.EqualValues(t, 4, f.State)
}

// lateCompleteAfterAbort 模拟 battle 迟到的终局提交（在途重试、或旧进程恢复后重放）：只能读到既有 ABORT，
// 不写任何个人结算来源。
func lateCompleteAfterAbort(t *testing.T, s *Store, f *pb.MatchFormationRecord) {
	t.Helper()
	ctx := context.Background()
	original := &battlepb.CreateBattleRequest{}
	require.NoError(t, proto.Unmarshal(f.CreateRequest, original))
	event := completeResult()
	require.Len(t, original.Players, len(event.Settlements))
	require.Len(t, event.Teams, len(original.Players))
	event.BattleId = f.BattleId
	event.MatchMode = original.MatchMode
	event.BattleConfigId = original.BattleConfigId
	event.ActivityContext = original.ActivityContext
	event.FinishedAtMs = original.DeadlineMs
	for i, player := range original.Players {
		require.NotNil(t, player)
		event.Teams[i].TeamIndex = player.TeamIndex
		event.Teams[i].PlayerIds = []uint64{player.PlayerId}
		event.Settlements[i].BattleId = f.BattleId
		event.Settlements[i].PlayerId = player.PlayerId
		event.Settlements[i].PlayerTeamIndex = player.TeamIndex
	}
	raw, err := proto.Marshal(event)
	require.NoError(t, err)
	require.NoError(t, validateTerminalResult(f, event, raw))
	decision, err := s.CommitBattleOutcome(ctx, &pb.CommitBattleOutcomeRequest{BattleId: f.BattleId, PlacementOperationId: f.OperationId, TargetInstanceId: f.TargetInstanceId, RawResultPayload: raw})
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, decision)
	var sources int
	require.NoError(t, s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM match_battle_settlement_source WHERE battle_id=?", f.BattleId).Scan(&sources))
	require.Zero(t, sources, "作废的局没有任何个人结算来源")
}

func requireStartedAndOccupied(t *testing.T, s *Store, battleID uint64, tickets []*pb.MatchAuthorityTicketRecord, why string) {
	t.Helper()
	ctx := context.Background()
	terminal, err := readTerminal(ctx, s.DB, battleID)
	require.NoError(t, err)
	require.Nil(t, terminal, why)
	f, err := s.Formation(ctx, battleID)
	require.NoError(t, err)
	require.EqualValues(t, 4, f.State, why)
	for _, original := range tickets {
		require.EqualValues(t, 3, ticketRow(t, s, original.PlayerId).State, why)
	}
}

func requireAbortedOnceAndReleased(t *testing.T, s *Store, battleID uint64, tickets []*pb.MatchAuthorityTicketRecord) *pb.MatchFormationRecord {
	t.Helper()
	ctx := context.Background()
	var rows int
	require.NoError(t, s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM match_battle_completion WHERE battle_id=?", battleID).Scan(&rows))
	require.Equal(t, 1, rows, "多副本只写一份终局")
	terminal, err := readTerminal(ctx, s.DB, battleID)
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, terminal.Decision)
	var formatVersion, sourceKind uint32
	require.NoError(t, s.DB.QueryRowContext(ctx, "SELECT format_version,source_kind FROM match_battle_completion WHERE battle_id=?", battleID).Scan(&formatVersion, &sourceKind))
	require.Zero(t, formatVersion, "截止 ABORT 没有 Battle 原字节")
	require.EqualValues(t, pb.MatchBattleCompletionSourceKind_MATCH_BATTLE_COMPLETION_SOURCE_MATCH_DEADLINE_ABORT, sourceKind, "不冒充 BATTLE_RPC 来源")
	f, err := s.Formation(ctx, battleID)
	require.NoError(t, err)
	require.EqualValues(t, 5, f.State, "formation 4→5 与 ABORT 同一事务")
	for _, original := range tickets {
		require.EqualValues(t, 4, ticketRow(t, s, original.PlayerId).State, "原票与 ABORT 同一事务结束")
	}
	return f
}

func TestStartedFormationTiDBNoResultLeavesOnlyThroughSQLAbortAfterDeadline(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const battleID = 1701
	tickets := exitFormationFixture(t, s, battleID, false)
	startFormation(t, s, battleID)
	// battle 停机作废 / 崩溃：原 worker 的租约随后到期，这一局再也不会有 CommitBattleOutcome。
	expireWorkLease(t, s, battleID)

	// 截止前：巡检多少轮都不释放（Create 已发出、房间可能还在打）。
	for range 2 {
		_, _, err := s.RecoverExpiredOutcomes(ctx, 0, 0)
		require.NoError(t, err)
		requireStartedAndOccupied(t, s, battleID, tickets, "截止前不能释放")
	}

	// 权威截止已过：两个副本同时巡检，只有一方提交 ABORT。
	expireStartedFormation(t, s, battleID)
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
	f := requireAbortedOnceAndReleased(t, s, battleID, tickets)
	lateCompleteAfterAbort(t, s, f)

	// 已终结的成局不再进候选，再巡检一轮什么都不变。
	_, _, err := s.RecoverExpiredOutcomes(ctx, 0, 0)
	require.NoError(t, err)
	again := requireAbortedOnceAndReleased(t, s, battleID, tickets)
	require.True(t, proto.Equal(f, again))
}

func TestStartedActivityFormationTiDBCancelSignsAbortOnlyAfterDeadline(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const battleID = 1801
	a, b := testJoin(t, s, 18011), testJoin(t, s, 18012)
	// Join 只用于构造原票快照；直接开局前必须先释放排队占用。
	require.NoError(t, s.Cancel(ctx, a.PlayerId, a.Ticket))
	require.NoError(t, s.Cancel(ctx, b.PlayerId, b.Ticket))
	tickets := []*pb.MatchAuthorityTicketRecord{a, b}
	formation := &pb.MatchFormationRecord{BattleId: battleID, OperationId: uuid.NewString(), PhysicalRegion: 1, TargetNodeId: 3,
		TargetInstanceId: "original-instance", TargetEndpoint: "127.0.0.1:1", Mode: a.Mode, ConfigId: a.ConfigId}
	key := &pb.MatchDirectIntentRecord{Kind: 2, OwnerId: 44, RequestLow: 381}
	body := []byte("same-entire-original-start")
	_, err := s.ReserveDirect(ctx, key, body, formation, tickets)
	require.NoError(t, err)
	startFormation(t, s, battleID)

	// 帮会恢复器每轮重放原 Start 的取消。截止前：只报告真实 formation=4，不签终局，原票不动 —— guild 不能据此释放。
	for range 2 {
		receipt, err := s.CancelActivity(ctx, key, body)
		require.NoError(t, err)
		require.False(t, receipt.RequestCancelled)
		require.EqualValues(t, battleID, receipt.BattleId)
		require.EqualValues(t, 4, receipt.FormationState)
		require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, receipt.TerminalDecision)
		requireStartedAndOccupied(t, s, battleID, tickets, "截止前的取消不能释放")
	}

	// 权威截止已过：两个 guild 副本同时取消，同一主键上只提交一份 ABORT，两份回执都证明 ABORT。
	expireStartedFormation(t, s, battleID)
	replica := *s
	start := make(chan struct{})
	type reply struct {
		receipt *pb.CancelActivityBattleResponse
		err     error
	}
	replies := make(chan reply, 2)
	for _, store := range []*Store{s, &replica} {
		go func(store *Store) {
			<-start
			receipt, err := store.CancelActivity(ctx, key, body)
			replies <- reply{receipt, err}
		}(store)
	}
	close(start)
	for range 2 {
		r := <-replies
		require.NoError(t, r.err)
		require.EqualValues(t, battleID, r.receipt.BattleId)
		require.EqualValues(t, 5, r.receipt.FormationState)
		require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, r.receipt.TerminalDecision)
	}
	f := requireAbortedOnceAndReleased(t, s, battleID, tickets)
	lateCompleteAfterAbort(t, s, f)

	// 释放之后的重放回同一份证明，不再改任何行。
	receipt, err := s.CancelActivity(ctx, key, body)
	require.NoError(t, err)
	require.EqualValues(t, 5, receipt.FormationState)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, receipt.TerminalDecision)
	again := requireAbortedOnceAndReleased(t, s, battleID, tickets)
	require.True(t, proto.Equal(f, again))
}

// lockedInbox 让 completion_test.go 的 terminalInboxFixture 能被两个副本并发调用(它的 seen 不加锁)。
type lockedInbox struct {
	inboxpb.PlayerInboxClient
	mu    sync.Mutex
	inner *terminalInboxFixture
}

func (l *lockedInbox) EnqueueBattleSettlement(ctx context.Context, r *inboxpb.BattleSettlementInboxRequest, opts ...grpc.CallOption) (*inboxpb.BattleSettlementInboxResponse, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inner.EnqueueBattleSettlement(ctx, r, opts...)
}

func (l *lockedInbox) LookupBattleSettlement(ctx context.Context, r *inboxpb.BattleSettlementInboxRequest, opts ...grpc.CallOption) (*inboxpb.BattleSettlementInboxResponse, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inner.LookupBattleSettlement(ctx, r, opts...)
}

func (l *lockedInbox) seenPerPlayer() map[uint64]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[uint64]int{}
	for _, player := range l.inner.seen {
		out[player]++
	}
	return out
}

func (l *lockedInbox) callCounts() (int, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inner.enqueues, l.inner.lookups
}

// COMPLETE 已落库、battle 随即崩溃（或停机补提交刚拿到 COMPLETE 就退出）：结果与全员个人结算只剩 match 发件箱投递，
// 不依赖 battle 进程。两个 match 副本同时巡检，同一行在一个认领租约内只有一方投递（next_retry_ms CAS）：结果只发一次，
// 每名成员首轮只 Enqueue，后续到期轮只 Lookup。认领者投递后崩溃、没来得及标记时，租约到期由另一副本重投同一份原字节
// —— 至少一次，评分 / 帮会按 battle_id、Inbox 按 (player, producer, battle_id) 去重。
func TestTerminalTiDBCommittedOutcomeIsDeliveredByOneReplicaPerLease(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	_, request := terminalResultFixture(t, s, false)
	decision, err := s.CommitBattleOutcome(ctx, request)
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE, decision)

	inbox := &lockedInbox{inner: &terminalInboxFixture{state: inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_PENDING}}
	resolve := func(context.Context) (inboxpb.PlayerInboxClient, error) { return inbox, nil }
	// 只记录、不在副本 goroutine 里断言(require 必须在测试 goroutine 调用)。
	var mu sync.Mutex
	var published [][]byte
	var publishedBattles []uint64
	publish := func(_ context.Context, battle uint64, raw []byte) error {
		mu.Lock()
		defer mu.Unlock()
		publishedBattles = append(publishedBattles, battle)
		published = append(published, append([]byte(nil), raw...))
		return nil
	}
	replica := *s
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, store := range []*Store{s, &replica} {
		go func(store *Store) {
			<-start
			results <- store.PublishBattleOutcomesOnce(ctx, resolve, terminalFixtureSecret, publish)
		}(store)
	}
	close(start)
	for range 2 {
		require.NoError(t, <-results)
	}
	require.Len(t, published, 1, "同一认领租约内结果只发一次")
	require.Equal(t, []uint64{request.BattleId}, publishedBattles)
	require.Equal(t, request.RawResultPayload, published[0])
	require.Equal(t, map[uint64]int{11: 1, 22: 1}, inbox.seenPerPlayer(), "首轮每名成员只 Enqueue 一次")
	enqueues, lookups := inbox.callCounts()
	require.Equal(t, 2, enqueues)
	require.Zero(t, lookups)

	// 不等待真实墙钟：只让本局 PENDING 来源到期，下一轮应只查入箱结果。
	retry, err := s.DB.ExecContext(ctx, "UPDATE match_battle_settlement_source SET next_retry_ms=1 WHERE battle_id=? AND dispatch_state=1 AND inbox_seq>0", request.BattleId)
	require.NoError(t, err)
	changed, err := retry.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 2, changed)
	require.NoError(t, replica.PublishBattleOutcomesOnce(ctx, resolve, terminalFixtureSecret, publish))
	require.Len(t, published, 1, "个人 Lookup 不重发结果")
	require.Equal(t, map[uint64]int{11: 2, 22: 2}, inbox.seenPerPlayer(), "每名成员一次 Enqueue + 一次 Lookup")
	enqueues, lookups = inbox.callCounts()
	require.Equal(t, 2, enqueues, "已入箱来源不重复 Enqueue")
	require.Equal(t, 2, lookups)

	// 认领者发出之后、标记 result_published 之前崩溃：行停在未发布、租约到期。另一副本重投同一份原字节，不重算。
	_, err = s.DB.ExecContext(ctx, "UPDATE match_battle_completion SET result_published=0,next_retry_ms=1 WHERE battle_id=?", request.BattleId)
	require.NoError(t, err)
	require.NoError(t, replica.PublishBattleOutcomesOnce(ctx, resolve, terminalFixtureSecret, publish))
	require.Len(t, published, 2)
	require.Equal(t, request.RawResultPayload, published[1])
	var pending int
	require.NoError(t, s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM match_battle_completion WHERE battle_id=? AND result_published=0", request.BattleId).Scan(&pending))
	require.Zero(t, pending)
}
