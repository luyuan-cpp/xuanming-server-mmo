package authority

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"match/internal/metrics"
	battlepb "proto/battle"
	kafkapb "proto/contracts/kafka"
	pb "proto/match"
	inboxpb "proto/player_inbox"
	"shared/inboxcontract"
)

// 2026-11-05 修复的回归(全球局终局协议,cross-zone-matchmaking.md §13.3「2026-11-05 修复」):
//   - 凡持久的 v1 Create 原件,终局一定裁决得了:SaveSupportedIntent 在锁内跑 COMPLETE 只看原件的全部判据(completableOriginal),
//     早于成局时刻、参战名单与成局成员不符的原件拒收;
//   - CommitBattleOutcome 先复读既有终局再判原件冲突;
//   - COMPLETE 不拿 battle 主机的结束时刻与 match 侧的创建时刻比先后;
//   - 所有 ABORT 走 commitAbortTx,提交成功才计数;
//   - 原执行实例证明不会建房时提前 ABORT(AbortWithoutRoom);
//   - 到期恢复按车道分页(RecoverExpiredOutcomeLane)。
// 纯函数部分不碰 SQL;真 TiDB 部分沿用 MATCH_IT_TIDB / MATCH_IT_REQUIRE_TIDB 开关与 formation_recovery_exit_test.go 的夹具,
// 不起 Battle、不依赖真实墙钟:准入期限与截止都用数据库当前时刻前后的固定偏移注入。

var errUnreachableStore = errors.New("fixture: store unreachable")

type unreachableConnector struct{}

func (unreachableConnector) Connect(context.Context) (driver.Conn, error) {
	return nil, errUnreachableStore
}

func (unreachableConnector) Driver() driver.Driver { return unreachableDriver{} }

type unreachableDriver struct{}

func (unreachableDriver) Open(string) (driver.Conn, error) { return nil, errUnreachableStore }

// unreachableDB 是每次取连接都失败的库:用来证明某条路径确实读了库,并且读失败时分类仍然正确。
func unreachableDB(t *testing.T) *sql.DB {
	t.Helper()
	db := sql.OpenDB(unreachableConnector{})
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// pureIntentFormation 是纯函数用例的成局行:两人、TiDB 时刻取一个足够大的值,减几秒不会下溢。
func pureIntentFormation() *pb.MatchFormationRecord {
	return &pb.MatchFormationRecord{BattleId: 901, OperationId: "op", TargetInstanceId: "uuid", Mode: 3, ConfigId: 1, State: 1,
		CreatedMs: 1_800_000_000_000, DeadlineMs: 1_800_000_300_000, Members: []*pb.MatchFormationMember{{PlayerId: 11}, {PlayerId: 22}}}
}

// v1IntentRequest 按 gather 的形状组 v1 Create 原件:成员序与成局一致、队号按序号,截止取成局行。
func v1IntentRequest(f *pb.MatchFormationRecord, createdAtMs, admitBeforeMs uint64) *battlepb.CreateBattleRequest {
	var players []*battlepb.BattlePlayerSnapshot
	for i, m := range f.Members {
		players = append(players, &battlepb.BattlePlayerSnapshot{PlayerId: m.PlayerId, TeamIndex: uint32(i)})
	}
	return &battlepb.CreateBattleRequest{BattleId: f.BattleId, PlacementOperationId: f.OperationId, TargetInstanceId: f.TargetInstanceId,
		BattleConfigId: f.ConfigId, MatchMode: f.Mode, OutcomeProtocolVersion: 1, CreatedAtMs: createdAtMs, DeadlineMs: f.DeadlineMs,
		AdmitBeforeMs: admitBeforeMs, Players: players}
}

func marshalIntent(t *testing.T, request *battlepb.CreateBattleRequest) []byte {
	t.Helper()
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
	require.NoError(t, err)
	return raw
}

// saveV1Intent 以本代租约经生产入口 SaveSupportedIntent 持久原件(能力证明固定为原实例 ABSENT + 支持协议 1)。
func saveV1Intent(t *testing.T, s *Store, work *pb.MatchFormationRecord, request *battlepb.CreateBattleRequest) error {
	t.Helper()
	_, err := s.SaveSupportedIntent(context.Background(), work, marshalIntent(t, request), &battlepb.QueryBattleCreationResponse{
		State: battlepb.BattleCreationState_BATTLE_CREATION_ABSENT, TargetInstanceId: work.TargetInstanceId, SupportedOutcomeProtocolVersion: 1})
	return err
}

// terminalEventFor 按持久 Create 原件组一份合法的两人终局原件(与 lateCompleteAfterAbort 同一组法)。
func terminalEventFor(t *testing.T, f *pb.MatchFormationRecord) *kafkapb.BattleResultEvent {
	t.Helper()
	original := &battlepb.CreateBattleRequest{}
	require.NoError(t, proto.Unmarshal(f.CreateRequest, original))
	event := completeResult()
	require.Len(t, original.Players, len(event.Settlements))
	event.BattleId = f.BattleId
	event.MatchMode = original.MatchMode
	event.BattleConfigId = original.BattleConfigId
	event.ActivityContext = original.ActivityContext
	event.FinishedAtMs = original.DeadlineMs
	for i, player := range original.Players {
		event.Teams[i].TeamIndex = player.TeamIndex
		event.Teams[i].PlayerIds = []uint64{player.PlayerId}
		event.Settlements[i].BattleId = f.BattleId
		event.Settlements[i].PlayerId = player.PlayerId
		event.Settlements[i].PlayerTeamIndex = player.TeamIndex
	}
	return event
}

func terminalRows(t *testing.T, s *Store, battleID uint64) int {
	t.Helper()
	var rows int
	require.NoError(t, s.DB.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM match_battle_completion WHERE battle_id=?", battleID).Scan(&rows))
	return rows
}

func settlementSources(t *testing.T, s *Store, battleID uint64) int {
	t.Helper()
	var rows int
	require.NoError(t, s.DB.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM match_battle_settlement_source WHERE battle_id=?", battleID).Scan(&rows))
	return rows
}

// abortedExpiredFormation 建一局已持久 v1 原件、已过截止与宽限、原租约已到期的成局(票 state 2),由到期恢复判 ABORT,
// 返回 ABORT 之后的成局行与原票。
func abortedExpiredFormation(t *testing.T, s *Store, battleID uint64) (*pb.MatchFormationRecord, []*pb.MatchAuthorityTicketRecord) {
	t.Helper()
	ctx := context.Background()
	tickets := exitFormationFixture(t, s, battleID, false)
	work, err := s.AcquireWork(ctx, battleID)
	require.NoError(t, err)
	persistV1Intent(t, s, work, true)
	require.NoError(t, s.Unknown(ctx, work))
	expireWorkLease(t, s, battleID)
	_, _, err = s.RecoverExpiredOutcomes(ctx, 0, 0)
	require.NoError(t, err)
	terminal, err := readTerminal(ctx, s.DB, battleID)
	require.NoError(t, err)
	require.NotNil(t, terminal)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, terminal.Decision)
	f, err := s.Formation(ctx, battleID)
	require.NoError(t, err)
	return f, tickets
}

// ---- 纯函数 ----

func TestCreateAdmissionClosedNeedsClockMarginAfterAdmitBefore(t *testing.T) {
	const admit = uint64(1_800_000_000_000)
	for _, c := range []struct {
		admit, stamp uint64
		closed       bool
		why          string
	}{
		{admit, admit - 1, false, "准入期限之前"},
		{admit, admit, false, "刚到期限:battle 主机时钟可能还慢一点"},
		{admit, admit + createAdmissionMarginMs - 1, false, "余量差 1 毫秒"},
		{admit, admit + createAdmissionMarginMs, true, "过了准入期限加时钟余量"},
		{0, ^uint64(0), false, "没有准入期限的原件判不了,交给截止裁决"},
		{^uint64(0), ^uint64(0), false, "先比较再相减,不溢出"},
	} {
		require.Equal(t, c.closed, CreateAdmissionClosed(c.admit, c.stamp), c.why)
	}
}

// 落库前的锁内判据就是 COMPLETE 只看原件的 completableOriginal(含截止 ABORT 读的 supportedOutcomeOriginal):创建时刻恰为成局时刻(gather 的下限)可以落库,
// 早 1 毫秒(修复前 match 本机时钟慢于 TiDB 的生产形状)就拒收;其余裁决不了的形状同样拒收。只拼候选,不改原行。
func TestValidateSupportedIntentRejectsOriginalNoTerminalCouldAdjudicate(t *testing.T) {
	f := pureIntentFormation()
	good := v1IntentRequest(f, f.CreatedMs, f.CreatedMs+30_000)
	require.NoError(t, validateSupportedIntent(f, marshalIntent(t, good), f.TargetInstanceId))
	type badShape struct {
		why  string
		edit func(*battlepb.CreateBattleRequest)
	}
	for _, c := range []badShape{
		{"本机时钟慢于 TiDB:创建时刻早于成局时刻 1 毫秒", func(r *battlepb.CreateBattleRequest) { r.CreatedAtMs = f.CreatedMs - 1 }},
		{"本机时钟快过整个截止窗口", func(r *battlepb.CreateBattleRequest) { r.CreatedAtMs = f.DeadlineMs }},
		{"截止与成局行不符", func(r *battlepb.CreateBattleRequest) { r.DeadlineMs++ }},
		{"成员数与成局不符", func(r *battlepb.CreateBattleRequest) { r.Players = r.Players[:1] }},
		{"配置与成局不符", func(r *battlepb.CreateBattleRequest) { r.BattleConfigId++ }},
		// 下面三条人数相同、身份版本时刻都对,修复前能落库;之后每次 COMPLETE 都被 validateTerminalResult 判「缺原成局成员」,
		// 全员白打一局、截止后作废(2026-11-05 评审修正:名单判据并入 completableOriginal)。
		{"成员号与成局不符(scene 回了别人的快照)", func(r *battlepb.CreateBattleRequest) { r.Players[1].PlayerId = 99 }},
		{"成员号为 0", func(r *battlepb.CreateBattleRequest) { r.Players[0].PlayerId = 0 }},
		{"成员重复", func(r *battlepb.CreateBattleRequest) { r.Players[1].PlayerId = r.Players[0].PlayerId }},
	} {
		bad := proto.Clone(good).(*battlepb.CreateBattleRequest)
		c.edit(bad)
		require.ErrorIs(t, validateSupportedIntent(f, marshalIntent(t, bad), f.TargetInstanceId), ErrUnterminableOriginal, c.why)
	}
	require.ErrorIs(t, validateSupportedIntent(f, marshalIntent(t, good), "other-instance"), ErrUnterminableOriginal, "能力证据的实例与目标不符")
	require.Empty(t, f.CreateRequest, "只拼候选,不改锁内原行")
	require.Zero(t, f.OutcomeProtocolVersion)

	// 名单只是 COMPLETE 的判据:截止 ABORT 读的 supportedOutcomeOriginal 不看名单,万一有名单不符的原件持久了,照样能被作废释放。
	stranger := proto.Clone(good).(*battlepb.CreateBattleRequest)
	stranger.Players[1].PlayerId = 99
	persisted := proto.Clone(f).(*pb.MatchFormationRecord)
	persisted.CreateRequest = marshalIntent(t, stranger)
	persisted.OutcomeProtocolVersion, persisted.PreflightSupportedVersion, persisted.PreflightInstanceId = 1, 1, f.TargetInstanceId
	_, err := supportedOutcomeOriginal(persisted)
	require.NoError(t, err, "截止 ABORT 的判据不变")
	_, _, err = completableOriginal(persisted)
	require.ErrorContains(t, err, "终局缺原成局成员")
}

// 结束时刻取 battle 主机时钟,创建时刻取 match 侧:battle 时钟稍慢、几秒就分出胜负的一局,结束时刻会早于创建时刻。
// 修复前这条比较让合法 COMPLETE 永久被拒、截止后整局作废;现在只要求非 0。
func TestTerminalResultDoesNotCompareCrossHostFinishTime(t *testing.T) {
	f := pureIntentFormation()
	request := v1IntentRequest(f, f.CreatedMs, f.CreatedMs+30_000)
	f.CreateRequest = marshalIntent(t, request)
	f.OutcomeProtocolVersion, f.PreflightSupportedVersion, f.PreflightInstanceId = 1, 1, f.TargetInstanceId
	e := completeResult()
	e.MatchMode, e.BattleConfigId = f.Mode, f.ConfigId
	e.FinishedAtMs = request.CreatedAtMs - 5_000
	require.NoError(t, validateTerminalResult(f, e, resultBytes(t, e)))
	e.FinishedAtMs = 0
	require.ErrorContains(t, validateTerminalResult(f, e, resultBytes(t, e)), "缺结束时刻")
}

// ---- 真 TiDB:落库前判据 ----

// 修复前:原件照常落库、照常发 Create,之后 COMPLETE 与 ABORT 都永久失败,全员票停在 2/3。
// 现在:SaveSupportedIntent 拒收,不落库也就不发 Create;gather 按发房前失败收尾,全员保留原入队时刻回队。
func TestSaveSupportedIntentTiDBRejectsOriginalOlderThanFormation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const battleID = 2401
	tickets := exitFormationFixture(t, s, battleID, true)
	work, err := s.AcquireWork(ctx, battleID)
	require.NoError(t, err)
	require.EqualValues(t, 1, work.State)
	stale := v1IntentRequest(work, work.CreatedMs-1, work.CreatedMs+30_000)
	require.ErrorIs(t, saveV1Intent(t, s, work, stale), ErrUnterminableOriginal)
	persisted, err := s.CreateIntentPersisted(ctx, work)
	require.NoError(t, err)
	require.False(t, persisted, "不合格的原件不落库")
	f, err := s.Formation(ctx, battleID)
	require.NoError(t, err)
	require.Empty(t, f.CreateRequest)
	require.EqualValues(t, 1, f.State)
	require.Zero(t, f.OutcomeProtocolVersion, "能力字段也没写")

	require.NoError(t, s.FinishFailed(ctx, work, nil), "无原件:按发房前失败收尾")
	for _, original := range tickets {
		current := ticketRow(t, s, original.PlayerId)
		require.EqualValues(t, 1, current.State, "排队来源回原队")
		require.Equal(t, original.Ticket, current.Ticket)
		require.Equal(t, original.EnqueuedMs, current.EnqueuedMs, "保留原等待时长")
		require.Zero(t, current.FormationId)
	}
}

// gather 的取法(本机墙钟与成局时刻取大)在本机慢于 TiDB 时得到恰为成局时刻的原件:能落库,过截止加宽限后到期恢复照常
// 判 ABORT、全员原票结束,并且只计一次 abort_deadline。
func TestSaveSupportedIntentTiDBGatherShapedOriginalReachesDeadlineAbort(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const battleID = 2402
	tickets := exitFormationFixture(t, s, battleID, false)
	work, err := s.AcquireWork(ctx, battleID)
	require.NoError(t, err)
	stamp, err := s.Now(ctx)
	require.NoError(t, err)
	// 成局行的两个时刻移到宽限之前(与 persistV1Intent 相同),模拟已过截止;本代租约不变。
	created, deadline := stamp-terminalOutcomeGraceMs-2000, stamp-terminalOutcomeGraceMs-1000
	_, err = s.DB.ExecContext(ctx, "UPDATE match_formation SET created_ms=?,deadline_ms=? WHERE battle_id=?", created, deadline, battleID)
	require.NoError(t, err)
	work.CreatedMs, work.DeadlineMs = created, deadline
	localNow := created - 5_000 // match 本机墙钟比 TiDB 慢 5 秒
	request := v1IntentRequest(work, max(localNow, work.CreatedMs), created+1_000)
	require.Equal(t, work.CreatedMs, request.CreatedAtMs)
	require.NoError(t, saveV1Intent(t, s, work, request))
	require.NoError(t, s.Unknown(ctx, work))
	expireWorkLease(t, s, battleID)

	before := metrics.GlobalFlowValue("terminal", terminalAbortDeadline)
	_, _, err = s.RecoverExpiredOutcomes(ctx, 0, 0)
	require.NoError(t, err)
	terminal, err := readTerminal(ctx, s.DB, battleID)
	require.NoError(t, err)
	require.NotNil(t, terminal, "原件可裁决,截止后有出口")
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, terminal.Decision)
	f, err := s.Formation(ctx, battleID)
	require.NoError(t, err)
	require.EqualValues(t, 5, f.State)
	for _, original := range tickets {
		require.EqualValues(t, 4, ticketRow(t, s, original.PlayerId).State)
	}
	require.Equal(t, before+1, metrics.GlobalFlowValue("terminal", terminalAbortDeadline), "提交成功计一次")
	_, _, err = s.RecoverExpiredOutcomes(ctx, 0, 0)
	require.NoError(t, err)
	require.Equal(t, before+1, metrics.GlobalFlowValue("terminal", terminalAbortDeadline), "已有终局不重复计数")
}

// ---- 真 TiDB:终局回放先于原件冲突 ----

// 原件永久不合格(个人结算超 Inbox 上限)的 COMPLETE,在 ABORT 已落库后要回 ABORT,battle 才能收尾删房;
// 修复前它永远收到「原件冲突」。身份不符的请求仍是原件冲突,也不产生任何个人来源。
func TestCommitBattleOutcomeTiDBReplaysAbortForPermanentlyInvalidOriginal(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const battleID = 2403
	f, _ := abortedExpiredFormation(t, s, battleID)
	limit, ok := inboxcontract.MaxPayloadBytes(inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_BATTLE_SETTLEMENT)
	require.True(t, ok)
	event := terminalEventFor(t, f)
	padSettlementTo(t, event.Settlements[0], limit+1)
	raw := resultBytes(t, event)
	require.ErrorContains(t, validateTerminalPayload(event, raw), "Inbox载荷上限", "夹具确实是永久不合格的原件")
	decision, err := s.CommitBattleOutcome(ctx, &pb.CommitBattleOutcomeRequest{BattleId: f.BattleId, PlacementOperationId: f.OperationId, TargetInstanceId: f.TargetInstanceId, RawResultPayload: raw})
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, decision)
	_, err = s.CommitBattleOutcome(ctx, &pb.CommitBattleOutcomeRequest{BattleId: f.BattleId, PlacementOperationId: "other-operation", TargetInstanceId: f.TargetInstanceId, RawResultPayload: raw})
	require.ErrorIs(t, err, ErrBattleOutcomeOriginal, "别的原操作不能借这份 ABORT")
	requireOnlyVoidSources(t, s, battleID, len(f.Members))
	require.Equal(t, 1, terminalRows(t, s, battleID))
}

// 锁外复读时还没有终局、进锁之前另一副本刚提交了截止 ABORT(原票已置 4):跳过锁外复读直接进锁内提交点,确定性地复现。
// 修复前这里回「原成员或归属失效」的永久冲突;现在复读到既有 ABORT 并回放。
func TestCommitBattleOutcomeTiDBLockedPathReplaysAbortCommittedByAnotherReplica(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const battleID = 2404
	f, tickets := abortedExpiredFormation(t, s, battleID)
	for _, original := range tickets {
		require.EqualValues(t, 4, ticketRow(t, s, original.PlayerId).State, "前提:原票已随 ABORT 结束")
	}
	event := terminalEventFor(t, f)
	req := &pb.CommitBattleOutcomeRequest{BattleId: f.BattleId, PlacementOperationId: f.OperationId, TargetInstanceId: f.TargetInstanceId, RawResultPayload: resultBytes(t, event)}
	decision, err := s.commitBattleOutcomeLocked(ctx, req, event)
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, decision)
	requireOnlyVoidSources(t, s, battleID, len(f.Members))
	require.Equal(t, 1, terminalRows(t, s, battleID))
}

// 锁内那一次复读终局:COMPLETE 已提交之后原票是 3,terminalFormation 照样拿得到锁;同一原件跳过锁外复读直接进锁内提交点,
// 要回放 COMPLETE,不插第二份终局、不动个人来源。删掉锁内复读,这里会撞终局主键、回存储错误,battle 收到 Unavailable 后
// 每 5 秒重放、永远拿不到决定。同一原操作换了原字节仍按原件冲突拒绝。
// (锁内复读排在 state 5 与原件判定之前这一点没有可达输入能区分先后:把 formation 置 5 的提交都在同一事务里关掉原票,
// 走到锁内时原票必是 2/3、formation 不是 5;落库的原件也都已过 completableOriginal。)
func TestCommitBattleOutcomeTiDBLockedPathReplaysCommittedComplete(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	f, req := terminalResultFixture(t, s, false)
	decision, err := s.CommitBattleOutcome(ctx, req)
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE, decision)
	event := &kafkapb.BattleResultEvent{}
	require.NoError(t, proto.Unmarshal(req.RawResultPayload, event))

	decision, err = s.commitBattleOutcomeLocked(ctx, req, event)
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE, decision)
	require.Equal(t, 1, terminalRows(t, s, f.BattleId))
	require.Equal(t, 2, settlementSources(t, s, f.BattleId))

	changed := proto.Clone(event).(*kafkapb.BattleResultEvent)
	changed.Settlements[0].GoldGain++
	other := proto.Clone(req).(*pb.CommitBattleOutcomeRequest)
	other.RawResultPayload = resultBytes(t, changed)
	_, err = s.commitBattleOutcomeLocked(ctx, other, changed)
	require.ErrorIs(t, err, ErrBattleOutcomeOriginal, "同一原操作换了原字节")
	require.Equal(t, 1, terminalRows(t, s, f.BattleId))
	require.Equal(t, 2, settlementSources(t, s, f.BattleId))
}

// 结束时刻早于原件创建时刻(battle 主机时钟慢 5 秒的秒杀局)的合法终局照常 COMPLETE。
func TestCommitBattleOutcomeTiDBShortBattleOnSlowBattleClockCompletes(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	f, req := terminalResultFixture(t, s, false)
	original := &battlepb.CreateBattleRequest{}
	require.NoError(t, proto.Unmarshal(f.CreateRequest, original))
	e := completeResult()
	e.MatchMode, e.BattleConfigId = f.Mode, f.ConfigId
	e.FinishedAtMs = original.CreatedAtMs - 5_000
	req.RawResultPayload = resultBytes(t, e)
	decision, err := s.CommitBattleOutcome(ctx, req)
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE, decision)
	require.Equal(t, 2, settlementSources(t, s, f.BattleId))
}

// ---- 真 TiDB:提前 ABORT ----

// closedAdmissionOffsetMs 让原件准入期限落在 SQL 当前时刻加余量之前:准入已关闭。
var closedAdmissionOffsetMs = -int64(createAdmissionMarginMs) - 1_000

// noRoomFixture 建一局已持久 v1 原件、还没建成(state 2)的成局,原件准入期限 = SQL 当前时刻 + admitOffsetMs。
// 返回持租原行(本代工作租约仍有效)。
func noRoomFixture(t *testing.T, s *Store, battleID uint64, admitOffsetMs int64) (*pb.MatchFormationRecord, []*pb.MatchAuthorityTicketRecord) {
	t.Helper()
	ctx := context.Background()
	tickets := exitFormationFixture(t, s, battleID, false)
	work, err := s.AcquireWork(ctx, battleID)
	require.NoError(t, err)
	stamp, err := s.Now(ctx)
	require.NoError(t, err)
	require.NoError(t, saveV1Intent(t, s, work, v1IntentRequest(work, work.CreatedMs, uint64(int64(stamp)+admitOffsetMs))))
	f, err := s.Formation(ctx, battleID)
	require.NoError(t, err)
	require.EqualValues(t, 2, f.State)
	return f, tickets
}

func noRoomProofAt(f *pb.MatchFormationRecord, observedMs uint64) NoRoomProof {
	return NoRoomProof{OperationId: f.OperationId, TargetInstanceId: f.TargetInstanceId, ObservedAfterMs: observedMs}
}

func requireStillOccupied(t *testing.T, s *Store, battleID uint64, tickets []*pb.MatchAuthorityTicketRecord, why string) {
	t.Helper()
	require.Zero(t, terminalRows(t, s, battleID), why)
	f, err := s.Formation(context.Background(), battleID)
	require.NoError(t, err)
	require.EqualValues(t, 2, f.State, why)
	for _, original := range tickets {
		require.EqualValues(t, 2, ticketRow(t, s, original.PlayerId).State, why)
	}
}

// 准入还没关(battle 按本机时钟仍可能接这份 Create),或证据是在准入关闭之前取得的:都不算证据,什么都不改。
func TestAbortWithoutRoomTiDBNeedsProofObservedAfterAdmissionClosed(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	open, openTickets := noRoomFixture(t, s, 2501, 60_000)
	stamp, err := s.Now(ctx)
	require.NoError(t, err)
	decision, err := s.AbortWithoutRoom(ctx, open, noRoomProofAt(open, stamp))
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, decision)
	requireStillOccupied(t, s, 2501, openTickets, "准入未关不能提前 ABORT")

	closed, closedTickets := noRoomFixture(t, s, 2502, closedAdmissionOffsetMs)
	request := &battlepb.CreateBattleRequest{}
	require.NoError(t, proto.Unmarshal(closed.CreateRequest, request))
	decision, err = s.AbortWithoutRoom(ctx, closed, noRoomProofAt(closed, request.AdmitBeforeMs))
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, decision, "证据时刻没过准入期限加余量,锁内时刻再晚也不算")
	requireStillOccupied(t, s, 2502, closedTickets, "证据早于准入关闭")

	wrong := noRoomProofAt(closed, stamp)
	wrong.TargetInstanceId = "other-instance"
	_, err = s.AbortWithoutRoom(ctx, closed, wrong)
	require.Error(t, err, "别的实例给的证据不算")
	requireStillOccupied(t, s, 2502, closedTickets, "证据实例不符")
}

// 证据成立:同一事务写 ABORT(来源 MATCH_NO_ROOM_ABORT、没有 Battle 原字节)、formation 2→5、全员原票 2→4,计一次 abort_no_room。
// 重放只读到既有 ABORT、不重复计数;迟到的 COMPLETE 只能读到 ABORT;之后的到期恢复不写第二份。
func TestAbortWithoutRoomTiDBReleasesOnceAndLateCompleteReadsAbort(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const battleID = 2503
	f, tickets := noRoomFixture(t, s, battleID, closedAdmissionOffsetMs)
	before := metrics.GlobalFlowValue("terminal", terminalAbortNoRoom)
	observed, err := s.Now(ctx)
	require.NoError(t, err)
	decision, err := s.AbortWithoutRoom(ctx, f, noRoomProofAt(f, observed))
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, decision)
	require.Equal(t, 1, terminalRows(t, s, battleID))
	var formatVersion, sourceKind uint32
	require.NoError(t, s.DB.QueryRowContext(ctx, "SELECT format_version,source_kind FROM match_battle_completion WHERE battle_id=?", battleID).Scan(&formatVersion, &sourceKind))
	require.Zero(t, formatVersion, "提前 ABORT 没有 Battle 原字节")
	require.EqualValues(t, pb.MatchBattleCompletionSourceKind_MATCH_BATTLE_COMPLETION_SOURCE_MATCH_NO_ROOM_ABORT, sourceKind, "与截止裁决分开审计")
	closed, err := s.Formation(ctx, battleID)
	require.NoError(t, err)
	require.EqualValues(t, 5, closed.State)
	for _, original := range tickets {
		require.EqualValues(t, 4, ticketRow(t, s, original.PlayerId).State, "原票与 ABORT 同一事务结束")
	}
	require.Equal(t, before+1, metrics.GlobalFlowValue("terminal", terminalAbortNoRoom))

	decision, err = s.AbortWithoutRoom(ctx, f, noRoomProofAt(f, observed))
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, decision, "重放读到既有 ABORT")
	require.Equal(t, before+1, metrics.GlobalFlowValue("terminal", terminalAbortNoRoom), "回放不计数")

	lateCompleteAfterAbort(t, s, closed)
	_, _, err = s.RecoverExpiredOutcomes(ctx, 0, 0)
	require.NoError(t, err)
	require.Equal(t, 1, terminalRows(t, s, battleID), "之后的到期恢复不写第二份")
}

// 只有当前工作租约的持有者能提前 ABORT:租约被接手后旧证据持有者回 ErrWorkLease、什么都不改,新持租者照样能判。
// 已建成(state 4)的成局不属于「不会建房」,即使准入已关也回 UNKNOWN。
func TestAbortWithoutRoomTiDBRefusesStaleLeaseAndStartedFormation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const battleID = 2504
	stale, tickets := noRoomFixture(t, s, battleID, closedAdmissionOffsetMs)
	expireWorkLease(t, s, battleID)
	next, err := s.AcquireWork(ctx, battleID)
	require.NoError(t, err)
	observed, err := s.Now(ctx)
	require.NoError(t, err)
	_, err = s.AbortWithoutRoom(ctx, stale, noRoomProofAt(stale, observed))
	require.ErrorIs(t, err, ErrWorkLease)
	requireStillOccupied(t, s, battleID, tickets, "失租的旧 worker 不能提交")
	decision, err := s.AbortWithoutRoom(ctx, next, noRoomProofAt(next, observed))
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, decision)

	const started = 2505
	work, startedTickets := noRoomFixture(t, s, started, closedAdmissionOffsetMs)
	require.NoError(t, s.RecordBattleAdopted(ctx, work, work.TargetInstanceId, 1))
	require.NoError(t, s.Finish(ctx, work, true))
	decision, err = s.AbortWithoutRoom(ctx, work, noRoomProofAt(work, observed))
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, decision)
	require.Zero(t, terminalRows(t, s, started))
	for _, original := range startedTickets {
		require.EqualValues(t, 3, ticketRow(t, s, original.PlayerId).State, "已建成的局等 COMPLETE 或截止")
	}
}

// 提前 ABORT 与迟到的 COMPLETE 同时进锁:不论谁先拿到原 P→formation 锁,终局只有一行,两个调用都无错地回同一个决定,
// 个人来源与原票状态与胜出的决定一致。断言对任意交错都成立,不依赖执行顺序。
func TestAbortWithoutRoomTiDBRacesCompleteForOneTerminal(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const battleID = 2506
	f, tickets := noRoomFixture(t, s, battleID, closedAdmissionOffsetMs)
	event := terminalEventFor(t, f)
	req := &pb.CommitBattleOutcomeRequest{BattleId: f.BattleId, PlacementOperationId: f.OperationId, TargetInstanceId: f.TargetInstanceId, RawResultPayload: resultBytes(t, event)}
	observed, err := s.Now(ctx)
	require.NoError(t, err)
	replica := *s
	type result struct {
		decision pb.BattleOutcomeDecision
		err      error
	}
	var commit, abort result
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		commit.decision, commit.err = s.CommitBattleOutcome(ctx, req)
	}()
	go func() {
		defer wg.Done()
		<-start
		abort.decision, abort.err = replica.AbortWithoutRoom(ctx, f, noRoomProofAt(f, observed))
	}()
	close(start)
	wg.Wait()
	require.NoError(t, commit.err)
	require.NoError(t, abort.err)
	require.Equal(t, 1, terminalRows(t, s, battleID))
	terminal, err := readTerminal(ctx, s.DB, battleID)
	require.NoError(t, err)
	require.Equal(t, terminal.Decision, commit.decision)
	require.Equal(t, terminal.Decision, abort.decision)
	switch terminal.Decision {
	case pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE:
		require.Equal(t, 2, settlementSources(t, s, battleID))
		for _, original := range tickets {
			require.EqualValues(t, 3, ticketRow(t, s, original.PlayerId).State)
		}
	case pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT:
		requireOnlyVoidSources(t, s, battleID, len(f.Members))
		for _, original := range tickets {
			require.EqualValues(t, 4, ticketRow(t, s, original.PlayerId).State)
		}
	default:
		t.Fatalf("终局决定非法: %v", terminal.Decision)
	}
}

// ---- 真 TiDB:到期恢复分车道 ----

// 一条车道只裁决 battle_id 落在本车道的局,翻到末尾回零值;另一条车道的局留给它自己的车道;车道参数非法直接报错。
func TestRecoverExpiredOutcomeLaneTiDBOnlyTouchesItsLane(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const first, second = 2601, 2602
	laneOf := func(id uint64) uint64 { return id % ExpiredOutcomeLanes }
	require.NotEqual(t, laneOf(first), laneOf(second), "夹具的两局落在不同车道")
	for _, id := range []uint64{first, second} {
		exitFormationFixture(t, s, id, false)
		work, err := s.AcquireWork(ctx, id)
		require.NoError(t, err)
		persistV1Intent(t, s, work, true)
		require.NoError(t, s.Unknown(ctx, work))
		expireWorkLease(t, s, id)
	}
	next, err := s.RecoverExpiredOutcomeLane(ctx, ExpiredOutcomeLanes, laneOf(first), ExpiredOutcomeCursor{})
	require.NoError(t, err)
	require.EqualValues(t, first, next.AfterBattle)
	require.Equal(t, 1, terminalRows(t, s, first))
	require.Zero(t, terminalRows(t, s, second), "别的车道不动")
	next, err = s.RecoverExpiredOutcomeLane(ctx, ExpiredOutcomeLanes, laneOf(first), next)
	require.NoError(t, err)
	require.Equal(t, ExpiredOutcomeCursor{}, next, "扫到末尾回零值")
	_, err = s.RecoverExpiredOutcomeLane(ctx, ExpiredOutcomeLanes, laneOf(second), ExpiredOutcomeCursor{})
	require.NoError(t, err)
	require.Equal(t, 1, terminalRows(t, s, second))
	for _, bad := range [][2]uint64{{0, 0}, {4, 4}} {
		_, err = s.RecoverExpiredOutcomeLane(ctx, bad[0], bad[1], ExpiredOutcomeCursor{})
		require.Error(t, err)
	}
}

// 一条车道积压超过一页(9 局,每页 8 局)时跨拍翻页:第一页裁决最早的 8 局、游标停在第 8 局,第二页从游标接着裁决第 9 局,
// 第三页扫到末尾回零值。插在中间的另一车道的局始终不动。游标只决定访问顺序,9 局各只落一份 ABORT。
func TestRecoverExpiredOutcomeLaneTiDBPagesThroughBacklogAcrossRounds(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const firstID, pageSize = uint64(2701), ExpiredOutcomePageSize
	lane := firstID % ExpiredOutcomeLanes
	var backlog []uint64
	for i := uint64(0); i <= pageSize; i++ {
		backlog = append(backlog, firstID+i*ExpiredOutcomeLanes) // 同一车道的 9 局
	}
	other := firstID + 1 // 插在中间、属于另一条车道
	require.NotEqual(t, lane, other%ExpiredOutcomeLanes)
	for _, id := range append(append([]uint64(nil), backlog...), other) {
		exitFormationFixture(t, s, id, false)
		work, err := s.AcquireWork(ctx, id)
		require.NoError(t, err)
		persistV1Intent(t, s, work, true)
		require.NoError(t, s.Unknown(ctx, work))
		expireWorkLease(t, s, id) // 全部 retry_at_ms=1,页内按 battle_id 排序
	}

	next, err := s.RecoverExpiredOutcomeLane(ctx, ExpiredOutcomeLanes, lane, ExpiredOutcomeCursor{})
	require.NoError(t, err)
	require.Equal(t, ExpiredOutcomeCursor{AfterRetryMs: 1, AfterBattle: backlog[pageSize-1]}, next, "第一页满,游标停在第 8 局")
	for _, id := range backlog[:pageSize] {
		require.Equal(t, 1, terminalRows(t, s, id))
	}
	require.Zero(t, terminalRows(t, s, backlog[pageSize]), "第 9 局留给下一拍")

	next, err = s.RecoverExpiredOutcomeLane(ctx, ExpiredOutcomeLanes, lane, next)
	require.NoError(t, err)
	require.Equal(t, ExpiredOutcomeCursor{AfterRetryMs: 1, AfterBattle: backlog[pageSize]}, next)
	require.Equal(t, 1, terminalRows(t, s, backlog[pageSize]), "下一拍从游标接着翻")

	next, err = s.RecoverExpiredOutcomeLane(ctx, ExpiredOutcomeLanes, lane, next)
	require.NoError(t, err)
	require.Equal(t, ExpiredOutcomeCursor{}, next, "扫到末尾回零值")
	for _, id := range backlog {
		require.Equal(t, 1, terminalRows(t, s, id), "每局一份")
		f, err := s.Formation(ctx, id)
		require.NoError(t, err)
		require.EqualValues(t, 5, f.State)
	}
	require.Zero(t, terminalRows(t, s, other), "另一车道的局不动")
}
