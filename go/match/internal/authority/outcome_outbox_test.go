package authority

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/logx/logtest"
	"google.golang.org/protobuf/proto"
	battlepb "proto/battle"
	pb "proto/match"
	inboxpb "proto/player_inbox"
)

// 2026-10-30(评审 B-4 / B-6):终局发件箱的批量 / 并发 / 计数 / 积压读数,巡检对等待退避的尊重,
// 以及到期恢复对不可恢复成局的计数。前半是不碰 SQL 的纯逻辑,后半是真 TiDB(MATCH_IT_TIDB)。

// outcomeRecorder 记 Observe 收到的 kind/outcome,可被多个 goroutine 同时调用。
type outcomeRecorder struct {
	mu   sync.Mutex
	seen map[string]int
}

func newOutcomeRecorder() *outcomeRecorder { return &outcomeRecorder{seen: map[string]int{}} }

func (r *outcomeRecorder) observe(kind, outcome string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen[kind+"/"+outcome]++
}

func (r *outcomeRecorder) count(kind, outcome string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seen[kind+"/"+outcome]
}

func TestOutcomeOutboxOptionsDefaultsAndBounds(t *testing.T) {
	got, err := OutcomeOutboxOptions{}.Normalized()
	require.NoError(t, err)
	require.Equal(t, []int{8, 4, 4, 1}, []int{got.ResultBatch, got.FirstBatch, got.WaitingBatch, got.Concurrency},
		"零值取默认,默认与 2026-10-30 之前写死的行为一致")
	got, err = OutcomeOutboxOptions{ResultBatch: 16, FirstBatch: 1, WaitingBatch: MaxOutcomeOutboxBatch, Concurrency: MaxOutcomeOutboxConcurrency}.Normalized()
	require.NoError(t, err)
	require.Equal(t, []int{16, 1, MaxOutcomeOutboxBatch, MaxOutcomeOutboxConcurrency}, []int{got.ResultBatch, got.FirstBatch, got.WaitingBatch, got.Concurrency})
	for name, bad := range map[string]OutcomeOutboxOptions{
		"结果批量为负":  {ResultBatch: -1},
		"首投批量超上界": {FirstBatch: MaxOutcomeOutboxBatch + 1},
		"等待批量超上界": {WaitingBatch: MaxOutcomeOutboxBatch + 1},
		"并发为负":    {Concurrency: -1},
		"并发超上界":   {Concurrency: MaxOutcomeOutboxConcurrency + 1},
	} {
		_, err := bad.Normalized()
		require.Error(t, err, name)
	}
	// 越界在任何 SQL 之前拒绝:零值 DB 没有连接能力,走到 SQL 会 panic。
	require.Error(t, (&Store{}).PublishBattleOutcomes(context.Background(),
		func(context.Context) (inboxpb.PlayerInboxClient, error) { return nil, nil }, terminalFixtureSecret,
		func(context.Context, uint64, []byte) error { return nil }, OutcomeOutboxOptions{Concurrency: -1}))
}

// 有界并发:同时在途的行不超过 concurrency,且确实能并发到上限;concurrency=1 时严格按读出顺序逐行处理。
func TestOutboxJobsBoundConcurrency(t *testing.T) {
	for _, limit := range []int{1, 3} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			var inFlight, peak atomic.Int32
			var mu sync.Mutex
			var order []int
			started := make(chan struct{}, 10)
			release := make(chan struct{})
			go func() {
				// 前 limit 行同时在途后放行全部;串行实现到不了 limit(>1)时 5 秒后兜底放行,下面的 peak 断言转红。
				timeout := time.After(5 * time.Second)
				for i := 0; i < limit; i++ {
					select {
					case <-started:
					case <-timeout:
						close(release)
						return
					}
				}
				close(release)
			}()
			jobs := make([]outboxJob, 10)
			for i := range jobs {
				jobs[i] = outboxJob{kind: OutboxKindResult, run: func(context.Context) outboxRowResult {
					now := inFlight.Add(1)
					for {
						old := peak.Load()
						if now <= old || peak.CompareAndSwap(old, now) {
							break
						}
					}
					mu.Lock()
					order = append(order, i)
					mu.Unlock()
					started <- struct{}{}
					<-release
					inFlight.Add(-1)
					return outboxRowResult{outcome: OutboxPublished}
				}}
			}
			rec := newOutcomeRecorder()
			failures, fatal := runOutboxJobs(context.Background(), jobs, limit, time.Now().Add(time.Minute), rec.observe)
			require.NoError(t, fatal)
			require.Empty(t, failures)
			require.EqualValues(t, limit, peak.Load(), "在途上限就是 concurrency")
			require.Equal(t, 10, rec.count(OutboxKindResult, OutboxPublished))
			if limit == 1 {
				require.Equal(t, []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}, order, "串行时后一行在前一行结束之后才认领")
			}
		})
	}
}

// SQL 失败(fatal)之后不再派发新行;非致命的投递失败只记下来,不挡后面的行。
func TestOutboxJobsStopAfterFatalButNotAfterDeliveryFailure(t *testing.T) {
	dbErr := errors.New("fixture SQL down")
	offline := errors.New("fixture inbox offline")
	var ran []int
	job := func(i int, result outboxRowResult) outboxJob {
		return outboxJob{kind: OutboxKindFirst, run: func(context.Context) outboxRowResult { ran = append(ran, i); return result }}
	}
	failures, fatal := runOutboxJobs(context.Background(), []outboxJob{
		job(0, outboxRowResult{outcome: OutboxFailed, err: offline}),
		job(1, outboxRowResult{outcome: OutboxPending}),
		job(2, outboxRowResult{outcome: OutboxStoreError, err: dbErr, fatal: true}),
		job(3, outboxRowResult{outcome: OutboxPending}),
	}, 1, time.Now().Add(time.Minute), nil)
	require.ErrorIs(t, fatal, dbErr)
	require.Equal(t, []error{offline}, failures)
	require.Equal(t, []int{0, 1, 2}, ran, "SQL 失败之后的行不再认领")
}

// 过了认领预算就不再认领:剩下的行记 deferred、一行都不跑;ctx 已结束同样不派发,并报 ctx 错误。
func TestOutboxJobsDeferAfterBudgetAndStopOnContext(t *testing.T) {
	ran := 0
	jobs := []outboxJob{
		{kind: OutboxKindResult, run: func(context.Context) outboxRowResult { ran++; return outboxRowResult{outcome: OutboxPublished} }},
		{kind: OutboxKindWaiting, run: func(context.Context) outboxRowResult { ran++; return outboxRowResult{outcome: OutboxApplied} }},
	}
	rec := newOutcomeRecorder()
	failures, fatal := runOutboxJobs(context.Background(), jobs, 2, time.Now().Add(-time.Second), rec.observe)
	require.NoError(t, fatal)
	require.Empty(t, failures)
	require.Zero(t, ran)
	require.Equal(t, 1, rec.count(OutboxKindResult, OutboxDeferred))
	require.Equal(t, 1, rec.count(OutboxKindWaiting, OutboxDeferred))

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, fatal = runOutboxJobs(cancelled, jobs, 2, time.Now().Add(time.Minute), nil)
	require.ErrorIs(t, fatal, context.Canceled)
	require.Zero(t, ran)
}

// 某行 panic:不再派发后面的行,等在途的行结束后在调用方 goroutine 里重新 panic(调用方的 recover 照旧生效)。
func TestOutboxJobsRepanicInCaller(t *testing.T) {
	ranAfter := false
	jobs := []outboxJob{
		{kind: OutboxKindResult, run: func(context.Context) outboxRowResult { panic("fixture boom") }},
		{kind: OutboxKindResult, run: func(context.Context) outboxRowResult { ranAfter = true; return outboxRowResult{} }},
	}
	require.PanicsWithValue(t, "fixture boom", func() {
		_, _ = runOutboxJobs(context.Background(), jobs, 1, time.Now().Add(time.Minute), nil)
	})
	require.False(t, ranAfter)
}

// 形状合格(候选 SQL 会选中)却过不了持久 Create 原件核对的成局:内部回 ErrUnrecoverableFormation,
// 对外 RecoverExpiredOutcome 仍回 UNKNOWN、不报错(现有调用方把 error 当本轮故障并 HoldRelay),但打 ERROR 并计数。
// 旧协议 / 明确采用 0 / 能力实例不符不是这一类(按设计 UNKNOWN,过截止后由 CountStrandedExpiredFormations 计数)。
// 两种判定都在任何 SQL 之前,零值 Store 即可。
func TestUnrecoverableFormationIsReportedButStaysUnknown(t *testing.T) {
	create := &battlepb.CreateBattleRequest{BattleId: 901, PlacementOperationId: "op", TargetInstanceId: "uuid", OutcomeProtocolVersion: 1,
		MatchMode: 3, BattleConfigId: 1, CreatedAtMs: 1, DeadlineMs: 30, Players: []*battlepb.BattlePlayerSnapshot{{PlayerId: 11}, {PlayerId: 22, TeamIndex: 1}}}
	mismatched := proto.Clone(create).(*battlepb.CreateBattleRequest)
	mismatched.BattleConfigId = 2
	raw, err := proto.Marshal(mismatched)
	require.NoError(t, err)
	formation := func() *pb.MatchFormationRecord {
		return &pb.MatchFormationRecord{BattleId: 901, OperationId: "op", TargetInstanceId: "uuid", Mode: 3, ConfigId: 1, CreatedMs: 1, DeadlineMs: 30,
			OutcomeProtocolVersion: 1, PreflightSupportedVersion: 1, PreflightInstanceId: "uuid", CreateRequest: raw,
			Members: []*pb.MatchFormationMember{{PlayerId: 11}, {PlayerId: 22}}}
	}
	ctx := context.Background()
	s := &Store{}
	require.True(t, recoverableOutcomeShape(formation()), "形状只看成局字段,不看 Create 正文")
	decision, err := s.recoverExpiredOutcome(ctx, formation())
	require.ErrorIs(t, err, ErrUnrecoverableFormation)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, decision)

	logs := logtest.NewCollector(t)
	decision, err = s.RecoverExpiredOutcome(ctx, formation())
	require.NoError(t, err, "对外不报错:不可恢复是永久状态,不是本轮故障")
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, decision)
	require.Contains(t, logs.String(), "battle_id=901")

	for name, edit := range map[string]func(*pb.MatchFormationRecord){
		"旧协议":    func(f *pb.MatchFormationRecord) { f.OutcomeProtocolVersion = 0 },
		"明确采用0":  func(f *pb.MatchFormationRecord) { f.AdoptedOutcomeObserved = true },
		"能力实例不符": func(f *pb.MatchFormationRecord) { f.PreflightInstanceId = "other-instance" },
		"无操作号":   func(f *pb.MatchFormationRecord) { f.OperationId = "" },
	} {
		f := formation()
		edit(f)
		require.False(t, recoverableOutcomeShape(f), name)
		decision, err := s.recoverExpiredOutcome(ctx, f)
		require.NoError(t, err, name)
		require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, decision, name)
	}
}

// ── 真 TiDB ────────────────────────────────────────────────────────────

// insertDueResult 插一行已到期、尚未发布的 COMPLETE 结果(列与 CommitBattleOutcome 写的一致)。
func insertDueResult(t *testing.T, s *Store, battle uint64) {
	t.Helper()
	raw := []byte(fmt.Sprintf("result-original-%d", battle))
	digest := sha256.Sum256(raw)
	_, err := s.DB.ExecContext(context.Background(), "INSERT INTO match_battle_completion(battle_id,raw_payload,payload_digest,format_version,archived_ms,source_topic,source_partition,source_offset,decision,source_kind,placement_operation_id,target_instance_id,outcome_protocol_version,result_published,next_retry_ms) VALUES(?,?,?,1,1,'',0,0,1,2,'op','instance',1,0,0)", battle, raw, digest[:])
	require.NoError(t, err)
}

// insertDueSource 插一行已到期的个人结算。inboxSeq=0 是首投行;否则是已入箱等 APPLIED 的行
// (inbox_seq 取 player+100,与 terminalInboxFixture 回的 seq 一致)。
func insertDueSource(t *testing.T, s *Store, battle, player uint64, waiting bool) {
	t.Helper()
	body := []byte(fmt.Sprintf("settlement-original-%d-%d", battle, player))
	digest := sha256.Sum256(body)
	seq, state := uint64(0), uint32(0)
	if waiting {
		seq, state = player+100, uint32(inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_PENDING)
	}
	_, err := s.DB.ExecContext(context.Background(), "INSERT INTO match_battle_settlement_source(battle_id,player_id,settlement_proto,payload_digest,archived_ms,owner_account,owner_revision,issued_at_ms,inbox_seq,inbox_state,dispatch_state,next_retry_ms) VALUES(?,?,?,?,1,'original-owner',1,1,?,?,1,0)", battle, player, body, digest[:], seq, state)
	require.NoError(t, err)
}

func countRows(t *testing.T, s *Store, query string) int {
	t.Helper()
	var n int
	require.NoError(t, s.DB.QueryRowContext(context.Background(), query).Scan(&n))
	return n
}

// N 行在 ceil(N/批量) 轮内投完:每轮只认领本类批量那么多行,三类各自独立计名额。
func TestOutcomeTiDBOutboxDrainsWithinCeilRounds(t *testing.T) {
	const rows, batch = 5, 2
	rounds := (rows + batch - 1) / batch
	ctx := context.Background()
	noPublish := func(context.Context, uint64, []byte) error { return nil }
	for _, tc := range []struct {
		name    string
		options OutcomeOutboxOptions
		seed    func(t *testing.T, s *Store, i int)
		state   inboxpb.PlayerInboxItemState
		pending string
	}{
		{"结果", OutcomeOutboxOptions{ResultBatch: batch}, func(t *testing.T, s *Store, i int) { insertDueResult(t, s, uint64(7000+i)) },
			inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_PENDING, "SELECT COUNT(*) FROM match_battle_completion WHERE result_published=0"},
		{"首投", OutcomeOutboxOptions{FirstBatch: batch}, func(t *testing.T, s *Store, i int) { insertDueSource(t, s, uint64(7100+i), uint64(7200+i), false) },
			inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_PENDING, "SELECT COUNT(*) FROM match_battle_settlement_source WHERE dispatch_state=1 AND inbox_seq=0"},
		{"等待", OutcomeOutboxOptions{WaitingBatch: batch}, func(t *testing.T, s *Store, i int) { insertDueSource(t, s, uint64(7300+i), uint64(7400+i), true) },
			inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_APPLIED, "SELECT COUNT(*) FROM match_battle_settlement_source WHERE dispatch_state=1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			for i := 0; i < rows; i++ {
				tc.seed(t, s, i)
			}
			client := &terminalInboxFixture{state: tc.state}
			resolve := func(context.Context) (inboxpb.PlayerInboxClient, error) { return client, nil }
			for round := 1; round <= rounds; round++ {
				require.NoError(t, s.PublishBattleOutcomes(ctx, resolve, terminalFixtureSecret, noPublish, tc.options))
				require.Equal(t, max(rows-round*batch, 0), countRows(t, s, tc.pending), "第 %d 轮之后还剩的行", round)
			}
		})
	}
}

// 两个副本、各 4 路并发同时投递:每一行只投一次(结果只发一次,每名成员只 Enqueue 一次),落败的认领记 claim_lost。
func TestOutcomeTiDBConcurrentReplicasDeliverEachRowOnce(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const rows = 6
	for i := 0; i < rows; i++ {
		insertDueResult(t, s, uint64(7500+i))
		insertDueSource(t, s, uint64(7600+i), uint64(7700+i), false)
	}
	inbox := &lockedInbox{inner: &terminalInboxFixture{state: inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_PENDING}}
	resolve := func(context.Context) (inboxpb.PlayerInboxClient, error) { return inbox, nil }
	var mu sync.Mutex
	published := map[uint64]int{}
	publish := func(_ context.Context, battle uint64, _ []byte) error {
		mu.Lock()
		defer mu.Unlock()
		published[battle]++
		return nil
	}
	rec := newOutcomeRecorder()
	options := OutcomeOutboxOptions{ResultBatch: 8, FirstBatch: 8, WaitingBatch: 8, Concurrency: 4, Observe: rec.observe}
	replica := *s
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, store := range []*Store{s, &replica} {
		go func(store *Store) {
			<-start
			results <- store.PublishBattleOutcomes(ctx, resolve, terminalFixtureSecret, publish, options)
		}(store)
	}
	close(start)
	for range 2 {
		require.NoError(t, <-results)
	}
	require.Len(t, published, rows)
	for battle, n := range published {
		require.Equal(t, 1, n, "battle=%d 只发一次", battle)
	}
	seen := inbox.seenPerPlayer()
	require.Len(t, seen, rows)
	for player, n := range seen {
		require.Equal(t, 1, n, "player=%d 只 Enqueue 一次", player)
	}
	enqueues, lookups := inbox.callCounts()
	require.Equal(t, rows, enqueues)
	require.Zero(t, lookups)
	require.Equal(t, rows, rec.count(OutboxKindResult, OutboxPublished))
	require.Equal(t, rows, rec.count(OutboxKindFirst, OutboxPending))
	require.Zero(t, countRows(t, s, "SELECT COUNT(*) FROM match_battle_completion WHERE result_published=0"))
	require.Zero(t, countRows(t, s, "SELECT COUNT(*) FROM match_battle_settlement_source WHERE inbox_seq=0"))
}

// ready 票巡检只在等待行的 next_retry_ms 到期时才 Lookup:还没入箱、退避未到、原 owner 不符、已销账都不到期。
func TestOutcomeTiDBReadySweepRespectsWaitingBackoff(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	_, request := terminalResultFixture(t, s, false)
	_, err := s.CommitBattleOutcome(ctx, request)
	require.NoError(t, err)
	ticket, err := s.Ticket(ctx, 11)
	require.NoError(t, err)
	require.EqualValues(t, 3, ticket.State)
	due, err := s.SettlementLookupDue(ctx, ticket)
	require.NoError(t, err)
	require.False(t, due, "还没入箱:Lookup 无从查起,交给发件箱首投")

	client := &terminalInboxFixture{state: inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_PENDING}
	resolve := func(context.Context) (inboxpb.PlayerInboxClient, error) { return client, nil }
	publish := func(context.Context, uint64, []byte) error { return nil }
	require.NoError(t, s.PublishBattleOutcomesOnce(ctx, resolve, terminalFixtureSecret, publish))
	due, err = s.SettlementLookupDue(ctx, ticket)
	require.NoError(t, err)
	require.False(t, due, "入箱 PENDING 之后按 5 秒起的退避,未到期不查")

	_, err = s.DB.ExecContext(ctx, "UPDATE match_battle_settlement_source SET next_retry_ms=0 WHERE battle_id=? AND player_id=11", request.BattleId)
	require.NoError(t, err)
	due, err = s.SettlementLookupDue(ctx, ticket)
	require.NoError(t, err)
	require.True(t, due)
	other := proto.Clone(ticket).(*pb.MatchAuthorityTicketRecord)
	other.OwnerRevision++
	due, err = s.SettlementLookupDue(ctx, other)
	require.NoError(t, err)
	require.False(t, due, "只认原 owner 的来源行")

	client.state = inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_APPLIED
	_, err = s.DB.ExecContext(ctx, "UPDATE match_battle_settlement_source SET next_retry_ms=0")
	require.NoError(t, err)
	require.NoError(t, s.PublishBattleOutcomesOnce(ctx, resolve, terminalFixtureSecret, publish))
	due, err = s.SettlementLookupDue(ctx, ticket)
	require.NoError(t, err)
	require.False(t, due, "已销账,不再查")
}

// 积压读数:三类条数与最老一行的年龄,随投递推进而变化。
func TestOutcomeTiDBBacklogCountsAndOldestAge(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	_, request := terminalResultFixture(t, s, false)
	_, err := s.CommitBattleOutcome(ctx, request)
	require.NoError(t, err)
	_, err = s.DB.ExecContext(ctx, "UPDATE match_battle_completion SET archived_ms=archived_ms-90000 WHERE battle_id=?", request.BattleId)
	require.NoError(t, err)
	backlog, err := s.ReadOutcomeOutboxBacklog(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, backlog.Result.Rows)
	require.GreaterOrEqual(t, backlog.Result.OldestAgeSeconds, 90.0)
	require.EqualValues(t, 2, backlog.First.Rows)
	require.Zero(t, backlog.Waiting.Rows)
	require.Zero(t, backlog.Waiting.OldestAgeSeconds, "没有积压时年龄为 0")

	client := &terminalInboxFixture{state: inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_PENDING}
	resolve := func(context.Context) (inboxpb.PlayerInboxClient, error) { return client, nil }
	require.NoError(t, s.PublishBattleOutcomesOnce(ctx, resolve, terminalFixtureSecret, func(context.Context, uint64, []byte) error { return nil }))
	backlog, err = s.ReadOutcomeOutboxBacklog(ctx)
	require.NoError(t, err)
	require.Zero(t, backlog.Result.Rows)
	require.Zero(t, backlog.Result.OldestAgeSeconds)
	require.Zero(t, backlog.First.Rows)
	require.EqualValues(t, 2, backlog.Waiting.Rows)
}

// 真 TiDB:过截止、形状合格的成局,Create 原件被改坏(配置号与成局不符)。到期恢复每次访问都打 ERROR 并计数,
// 不写终局、不动票、不让整轮失败;它也不在 SQL 存量 CountStrandedExpiredFormations 里(那里只数形状不合格的)。
func TestReadyTiDBUnrecoverableFormationIsReportedOnEachVisit(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	f, _ := terminalResultFixture(t, s, true)
	request := &battlepb.CreateBattleRequest{}
	require.NoError(t, proto.Unmarshal(f.CreateRequest, request))
	request.BattleConfigId++
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
	require.NoError(t, err)
	_, err = s.DB.ExecContext(ctx, "UPDATE match_formation SET create_request=?,retry_at_ms=0 WHERE battle_id=?", raw, f.BattleId)
	require.NoError(t, err)
	before := map[uint64]*pb.MatchAuthorityTicketRecord{}
	for _, player := range []uint64{11, 22} {
		before[player] = ticketRow(t, s, player)
	}
	logs := logtest.NewCollector(t)
	_, _, err = s.RecoverExpiredOutcomes(ctx, 0, 0)
	require.NoError(t, err, "不可恢复是永久状态,不算本轮故障")
	require.Contains(t, logs.String(), fmt.Sprintf("battle_id=%d", f.BattleId))
	row, err := readTerminal(ctx, s.DB, f.BattleId)
	require.NoError(t, err)
	require.Nil(t, row, "原件对不上不能写 ABORT")
	for player, ticket := range before {
		require.True(t, proto.Equal(ticket, ticketRow(t, s, player)), "不可恢复不能改写原票")
	}
	stranded, err := s.CountStrandedExpiredFormations(ctx)
	require.NoError(t, err)
	require.Zero(t, stranded, "形状合格,SQL 存量判不出;这一类由访问时计数")
}
