package logic

// 2026-10-23 修复（审计 r2-guild#1）回归：历练 launch 恢复器只凭权威终态释放占用。
// 开局成功（match formation=4）后战斗不出结果的局，恢复器每次检查重放原 Start 的签名取消：截止前 match 只回 formation=4，
// RPC 也可能超时；只有 match 在权威截止后原子提交 SQL ABORT（回 formation=5、TerminalDecision=ABORT）才终结 launch。
//
// 2026-11-05 修复（r2-guild#1 评审）回归：恢复器只查仓储认领给它的到期原件（开局窗口内的原件不到期，见
// data/trial_launch_claim_tidb_test.go）；解不开的行跳过、不挡后面的原件；帮会已解散而战斗未终局时不报错（不 Hold 排空）；
// 释放之后推 ACTIVITY_CHANGED；一轮预算够就接着认领下一页。
//
// 2026-11-05 复审追加：实时路径 Start 被 match 确定地拒绝（或根本没发）时把原件提前交给恢复器，结局未知时不提前；
// 认领失败与轮预算尾部都不返回错误（不 Hold）；剩余预算不够一条检查就不认领；生产装配释放后推送；宽限盖住开局总预算。
//
// 仓储与取消适配器都是同一接口的替身（AGENTS §11.4），不连库、不起 match；时间注入 ActivityDeps.Now，不读墙钟
// （轮预算尾部那一条用「截止不足一条检查上限」这种与机器快慢无关的输入：剩余时间只会更少）。
// 仓储层的同一规则与真库释放由 data/trial_launch_abort_tidb_test.go、data/trial_launch_claim_tidb_test.go 覆盖。

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"guild/internal/config"
	"guild/internal/data"
	pb "proto/guild"
	matchpb "proto/match"
)

// launchRecoveryStore 在 fakeTrialStore 之上补 trialLaunchStore 的五个方法。
// 到期认领照生产仓储的语义:只交出到期的行,交出即认领(到期标记清掉,等测试推进「时间」再置上);
// 解不开的行与可解码的行一样被认领,单独列出;claimErr 非 nil 时交出这一页已认领的部分并报错(生产仓储出错时同样返回
// 出错之前已认领的部分)。FinishCancelledTrialLaunch 与生产仓储同一前置条件(data.TrialCancelReceiptIsTerminal),
// 终结后原件离开在途集合(生产仓储只认领 state 1/2)。ExpediteTrialLaunchCheck 只记下调用。
// 只在测试 goroutine 里同步调用，不另加锁。
type launchRecoveryStore struct {
	*fakeTrialStore
	pending     []*pb.GuildTrialLaunchOriginal
	undecodable []uint64
	due         map[uint64]bool
	finished    []*matchpb.CancelActivityBattleResponse
	claimErr    error
	claimLimits []int
	// expedited:每次 ExpediteTrialLaunchCheck 的 request_id;expediteCtxLive:当时 ctx 是否还没结束(须用独立预算)。
	expedited       []uint64
	expediteCtxLive []bool
}

func newLaunchRecoveryStore(pending ...*pb.GuildTrialLaunchOriginal) *launchRecoveryStore {
	return &launchRecoveryStore{fakeTrialStore: &fakeTrialStore{}, pending: pending, due: map[uint64]bool{}}
}

// makeDue 把全部在途行(含解不开的行)推到检查时刻 —— 相当于时间走过了它们的下一次检查时刻。
func (s *launchRecoveryStore) makeDue() {
	for _, original := range s.pending {
		s.due[original.RequestId] = true
	}
	for _, id := range s.undecodable {
		s.due[id] = true
	}
}

func (s *launchRecoveryStore) TrialLaunchEnabled() bool { return true }

func (s *launchRecoveryStore) PrepareTrialLaunch(context.Context, uint64, data.TrialBattleKey, uint32, []*pb.GuildTrialRoleIdentity, uint64) (*pb.GuildTrialLaunchOriginal, error) {
	return nil, errors.New("launch recovery never prepares a new original")
}

func (s *launchRecoveryStore) ExpediteTrialLaunchCheck(ctx context.Context, requestID uint64) error {
	s.expedited = append(s.expedited, requestID)
	s.expediteCtxLive = append(s.expediteCtxLive, ctx.Err() == nil)
	return nil
}

func (s *launchRecoveryStore) ClaimDueTrialLaunches(_ context.Context, limit int) (data.DueTrialLaunches, error) {
	s.claimLimits = append(s.claimLimits, limit)
	var out data.DueTrialLaunches
	read := 0
	for _, id := range s.undecodable {
		if s.due[id] && read < limit {
			read++
			s.due[id] = false
			out.Undecodable = append(out.Undecodable, id)
		}
	}
	for _, original := range s.pending {
		if s.due[original.RequestId] && read < limit {
			read++
			s.due[original.RequestId] = false
			out.Originals = append(out.Originals, original)
		}
	}
	out.More = read == limit
	return out, s.claimErr
}

func (s *launchRecoveryStore) FinishCancelledTrialLaunch(_ context.Context, original *pb.GuildTrialLaunchOriginal, receipt *matchpb.CancelActivityBattleResponse, _ uint64) error {
	if !data.TrialCancelReceiptIsTerminal(receipt) {
		return data.ErrWriteConflict
	}
	s.finished = append(s.finished, proto.Clone(receipt).(*matchpb.CancelActivityBattleResponse))
	s.pending = slices.DeleteFunc(s.pending, func(o *pb.GuildTrialLaunchOriginal) bool { return o.RequestId == original.RequestId })
	return nil
}

var _ trialLaunchStore = (*launchRecoveryStore)(nil)

// scriptedCancellation 按顺序回一串取消结果(最后一个重复)，并记下调用次数与每次重放的 request_id。
type scriptedCancellation struct {
	replies  []scriptedCancelReply
	calls    int
	requests []uint64
}

type scriptedCancelReply struct {
	receipt *matchpb.CancelActivityBattleResponse
	err     error
}

func (c *scriptedCancellation) CancelOriginal(_ context.Context, original *matchpb.StartActivityBattleRequest) (*matchpb.CancelActivityBattleResponse, error) {
	if original == nil || original.RequestId == 0 {
		return nil, errors.New("recovery must replay the original start")
	}
	reply := c.replies[min(c.calls, len(c.replies)-1)]
	c.calls++
	c.requests = append(c.requests, original.RequestId)
	if reply.err != nil {
		return nil, reply.err
	}
	return proto.Clone(reply.receipt).(*matchpb.CancelActivityBattleResponse), nil
}

func startedTrialLaunchOriginal() *pb.GuildTrialLaunchOriginal {
	return trialLaunchOriginalFor(381)
}

func trialLaunchOriginalFor(requestID uint64) *pb.GuildTrialLaunchOriginal {
	return &pb.GuildTrialLaunchOriginal{RequestId: requestID, GuildId: 44, ActivityId: 3, PeriodKey: 20261023, GuildPeriodKey: 20261023,
		InitiatorPlayerId: 22, DungeonId: 1, CreatedMs: 1_700_000_000_000,
		Roster: []*pb.GuildTrialRoleIdentity{{PlayerId: 11, Account: "seller", OwnerRevision: 1}, {PlayerId: 22, Account: "seller", OwnerRevision: 1}}}
}

// newLaunchRecovery 装配一个只记推送、不真推的恢复器。
func newLaunchRecovery(store *launchRecoveryStore, cancel TrialCancellation) (*trialLaunchRecovery, *[]*data.TrialLobby) {
	notified := &[]*data.TrialLobby{}
	deps := &ActivityDeps{trials: store, Cancellation: cancel, Now: func() time.Time { return time.UnixMilli(1_700_000_100_000) }}
	return &trialLaunchRecovery{d: deps, notify: func(lobby *data.TrialLobby) { *notified = append(*notified, lobby) }}, notified
}

func trialAbortReceipt(battleID uint64) *matchpb.CancelActivityBattleResponse {
	return &matchpb.CancelActivityBattleResponse{BattleId: battleID, FormationState: 5, TerminalDecision: matchpb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT}
}

func TestTrialLaunchRecoveryReleasesStartedBattleOnlyOnSQLAbort(t *testing.T) {
	const battleID = 900
	complete := matchpb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE
	abort := trialAbortReceipt(battleID)
	cancel := &scriptedCancellation{replies: []scriptedCancelReply{
		// 第 1 次检查：一次 RPC 超时，没有任何证明。
		{err: context.DeadlineExceeded},
		// 第 2 次：开局成功、截止前。
		{receipt: &matchpb.CancelActivityBattleResponse{BattleId: battleID, FormationState: 4}},
		// 第 3 次：已 COMPLETE，等结果事件。
		{receipt: &matchpb.CancelActivityBattleResponse{BattleId: battleID, FormationState: 4, TerminalDecision: complete}},
		// 第 4 次：自相矛盾的回执。
		{receipt: &matchpb.CancelActivityBattleResponse{BattleId: battleID, FormationState: 5, TerminalDecision: complete}},
		// 第 5 次：截止后 match 原子提交 SQL ABORT。
		{receipt: abort},
	}}
	store := newLaunchRecoveryStore(startedTrialLaunchOriginal())
	recovery, notified := newLaunchRecovery(store, cancel)
	ctx := context.Background()

	// 前四次：超时、formation=4、COMPLETE、矛盾回执，都不释放；formation=4 的两次只做幂等登记。
	// 结局未知的两次(超时、矛盾回执)回错误，调用方据此 Hold 排空证明；没有终态但结局明确的两次不算失败。
	wantErr := map[int]bool{1: true, 4: true}
	for check := 1; check <= 4; check++ {
		store.makeDue()
		err := recovery.round(ctx)
		require.Equal(t, wantErr[check], err != nil, "第 %d 次检查: %v", check, err)
		require.Equal(t, check, cancel.calls)
		require.Empty(t, store.finished, "第 %d 次不是权威终态，占用必须保留", check)
		require.Len(t, store.pending, 1)
		require.Empty(t, *notified)
	}
	require.Len(t, store.registered, 2, "formation=4 的两次各登记一次(仓储幂等)")
	for _, key := range store.registered {
		require.EqualValues(t, battleID, key.BattleID)
		require.EqualValues(t, 381, key.LaunchRequestID)
	}

	// 第五次拿到 SQL ABORT：恰好终结一次，回执原样交给仓储同事务落库，并给名单推一次变更。
	store.makeDue()
	require.NoError(t, recovery.round(ctx))
	require.Len(t, store.finished, 1)
	require.True(t, proto.Equal(abort, store.finished[0]))
	require.Empty(t, store.pending)
	require.Len(t, *notified, 1)
	require.EqualValues(t, 44, (*notified)[0].GuildID)
	require.Equal(t, []uint64{22, 11}, (*notified)[0].Roster, "推送名单与重放的原 Start 同一份:发起人在首位,其余升序")

	// 之后的检查认领不到已终结的原件，不再重放取消、不再释放。
	store.makeDue()
	require.NoError(t, recovery.round(ctx))
	require.Equal(t, 5, cancel.calls)
	require.Len(t, store.finished, 1)
	require.Len(t, *notified, 1)
}

// 仓储不交出的原件(开局窗口内 Start 还可能在路上、或别的副本已认领)恢复器一律不碰:不重放取消。
func TestTrialLaunchRecoveryChecksOnlyClaimedOriginals(t *testing.T) {
	cancel := &scriptedCancellation{replies: []scriptedCancelReply{{receipt: &matchpb.CancelActivityBattleResponse{RequestCancelled: true}}}}
	store := newLaunchRecoveryStore(trialLaunchOriginalFor(391))
	recovery, notified := newLaunchRecovery(store, cancel)
	ctx := context.Background()

	require.NoError(t, recovery.round(ctx))
	require.Zero(t, cancel.calls, "未到检查时刻的原件不能被取消:取消墓碑会让在途的合法 Start 被拒")
	require.Len(t, store.pending, 1)

	store.makeDue()
	require.NoError(t, recovery.round(ctx))
	require.Equal(t, 1, cancel.calls)
	require.Len(t, store.finished, 1, "过了窗口,零 battle 取消墓碑即可释放")
	require.Len(t, *notified, 1)
}

// 一行解不开的原件不再整页失败:跳过它(不算失败、不 Hold),同一页里排在它后面的原件照常恢复。
func TestTrialLaunchRecoverySkipsUndecodableOriginal(t *testing.T) {
	const battleID = 901
	cancel := &scriptedCancellation{replies: []scriptedCancelReply{{receipt: trialAbortReceipt(battleID)}}}
	store := newLaunchRecoveryStore(trialLaunchOriginalFor(392))
	store.undecodable = []uint64{7}
	store.makeDue()
	recovery, notified := newLaunchRecovery(store, cancel)

	require.NoError(t, recovery.round(context.Background()), "坏行不是本副本在途的动作,不能让它永远挡住排空证明")
	require.Equal(t, []uint64{392}, cancel.requests, "坏行不重放取消;它后面的原件照常恢复")
	require.Len(t, store.finished, 1)
	require.Len(t, *notified, 1)
	require.False(t, store.due[7], "坏行已被认领(推后检查时刻),不再占住到期队列的队头")
}

// 帮会已解散、战斗还没终局(formation 1..4):没有可登记的对局行,恢复器等权威终态,不报错 —— 报错会触发全局 Hold。
func TestTrialLaunchRecoveryDisbandedGuildWaitsWithoutError(t *testing.T) {
	const battleID = 902
	cancel := &scriptedCancellation{replies: []scriptedCancelReply{
		{receipt: &matchpb.CancelActivityBattleResponse{BattleId: battleID, FormationState: 4}},
		{receipt: trialAbortReceipt(battleID)},
	}}
	store := newLaunchRecoveryStore(trialLaunchOriginalFor(393))
	store.registerErr = data.ErrGuildGone
	recovery, notified := newLaunchRecovery(store, cancel)
	ctx := context.Background()

	store.makeDue()
	require.NoError(t, recovery.round(ctx))
	require.Equal(t, 1, cancel.calls)
	require.Empty(t, store.finished, "formation=4 不是终态,帮会解散也不是释放证明")
	require.Len(t, store.pending, 1)
	require.Empty(t, *notified)

	// 截止后的 SQL ABORT 照常释放(仓储的 FinishCancelledTrialLaunch 容忍帮会已解散)。
	store.makeDue()
	require.NoError(t, recovery.round(ctx))
	require.Len(t, store.finished, 1)
	require.Empty(t, store.pending)
	require.Len(t, *notified, 1)
}

// 一轮不止一页:到期原件多于一页时,预算够就接着认领下一页,吞吐不被「每轮 20 条」封顶。
// 每页只认领剩余预算查得完的条数(trialLaunchChecksWithin),所以这里一页至多 2 条、一共十几页。
func TestTrialLaunchRecoveryClaimsFurtherPagesWithinBudget(t *testing.T) {
	cancel := &scriptedCancellation{replies: []scriptedCancelReply{{receipt: &matchpb.CancelActivityBattleResponse{RequestCancelled: true}}}}
	var originals []*pb.GuildTrialLaunchOriginal
	for id := uint64(1000); id < 1000+trialLaunchClaimPage+5; id++ {
		originals = append(originals, trialLaunchOriginalFor(id))
	}
	store := newLaunchRecoveryStore(originals...)
	store.makeDue()
	recovery, notified := newLaunchRecovery(store, cancel)

	require.NoError(t, recovery.round(context.Background()))
	require.Equal(t, trialLaunchClaimPage+5, cancel.calls)
	require.Empty(t, store.pending)
	require.Len(t, *notified, trialLaunchClaimPage+5)
	seen := map[uint64]bool{}
	for _, id := range cancel.requests {
		require.False(t, seen[id], "同一轮里一条原件只查一次")
		seen[id] = true
	}
}

// 认领本身失败(到期扫描或 CAS 出错)时,没认领到的原件本副本什么都没发出:只计数,不返回错误 —— Hold 是粘性的,
// 一次 TiDB 抖动不能卡住本副本的排空证明直到重启。出错之前已认领的那部分照常查完。
func TestTrialLaunchRecoveryClaimFailureDoesNotHold(t *testing.T) {
	cancel := &scriptedCancellation{replies: []scriptedCancelReply{{receipt: &matchpb.CancelActivityBattleResponse{RequestCancelled: true}}}}
	store := newLaunchRecoveryStore(trialLaunchOriginalFor(394))
	store.claimErr = errors.New("tidb: region is unavailable")
	store.makeDue()
	recovery, notified := newLaunchRecovery(store, cancel)

	require.NoError(t, recovery.round(context.Background()))
	require.Len(t, store.claimLimits, 1, "认领出错,本轮结束")
	require.Equal(t, []uint64{394}, cancel.requests, "出错之前已认领的原件照常查")
	require.Len(t, store.finished, 1)
	require.Len(t, *notified, 1)

	// 什么都没认领到、只有错误:同样不报错,也不发任何取消。
	store.claimLimits = nil
	require.NoError(t, recovery.round(context.Background()))
	require.Len(t, store.claimLimits, 1)
	require.Equal(t, 1, cancel.calls)
}

// 轮预算的尾部:剩余预算不够一条检查的上限时既不认领、也不开查,round 返回 nil。否则最后一条的取消 RPC 被轮预算截断成
// DeadlineExceeded,每一轮都人为制造一次「结局未知」去 Hold 排空证明;认领了却查不了的原件还会白白拉长退避。
// 截止取一条检查上限的一半:不论机器快慢,剩余时间只会更少,结论与墙钟无关。
func TestTrialLaunchRecoveryStartsNoCheckItCannotFinish(t *testing.T) {
	cancel := &scriptedCancellation{replies: []scriptedCancelReply{{receipt: &matchpb.CancelActivityBattleResponse{RequestCancelled: true}}}}
	store := newLaunchRecoveryStore(trialLaunchOriginalFor(396))
	store.makeDue()
	recovery, _ := newLaunchRecovery(store, cancel)
	ctx, stop := context.WithTimeout(context.Background(), trialLaunchCallBudget/2)
	defer stop()

	require.NoError(t, recovery.round(ctx))
	require.Empty(t, store.claimLimits, "查不完的就不认领")
	require.Zero(t, cancel.calls)
	require.True(t, store.due[396], "原件仍到期,下一轮照常认领")
}

// 一页认领多少条 = 剩余预算里还能完整查几条(每条按上限算),封顶一页;不够一条就是 0。
func TestTrialLaunchChecksWithinBudget(t *testing.T) {
	require.Zero(t, trialLaunchChecksWithin(-time.Second))
	require.Zero(t, trialLaunchChecksWithin(0))
	require.Zero(t, trialLaunchChecksWithin(trialLaunchCallBudget-time.Millisecond))
	require.Equal(t, 1, trialLaunchChecksWithin(trialLaunchCallBudget))
	require.Equal(t, 1, trialLaunchChecksWithin(2*trialLaunchCallBudget-time.Millisecond), "向下取整:不认领查不完的那一条")
	require.Equal(t, int(trialLaunchRoundBudget/trialLaunchCallBudget), trialLaunchChecksWithin(trialLaunchRoundBudget))
	require.Equal(t, trialLaunchClaimPage, trialLaunchChecksWithin(time.Hour), "封顶一页")
	require.GreaterOrEqual(t, trialLaunchRoundBudget, trialLaunchCallBudget, "一轮至少要放得下一条检查,否则恢复器永远不查")

	ctx, stop := context.WithCancel(context.Background())
	stop()
	require.Zero(t, trialLaunchChecksLeft(ctx), "没有截止或已结束的 ctx 一条都不查")
}

// 宽限(data.TrialRecoveryGrace)要盖住一次开局许可可能被使用的全部时间,否则恢复器的取消会抢在在途的 Start 之前,
// 把合法的 Start 拒成「服务繁忙」:末人同意的请求在开局窗口里 Prepare、再发 Start;请求本身不超过 config.MaxRpcTimeoutMs,
// 单次 Start 不超过 config.MaxMatchRpcTimeoutMs;另留 10s 给各 tidb-server 本地时钟差与 match 提交晚于传播截止。
// 其中任何一项调大到盖不住,这里都会红(旧断言「宽限 ≥ 占用余量」两边是同一个常量,恒真)。
func TestTrialRecoveryGraceCoversLaunchBudget(t *testing.T) {
	const clockAndCommitSlack = 10 * time.Second
	launchBudget := trialLaunchWindow +
		time.Duration(config.MaxRpcTimeoutMs)*time.Millisecond +
		time.Duration(config.MaxMatchRpcTimeoutMs)*time.Millisecond +
		clockAndCommitSlack
	require.Greater(t, data.TrialRecoveryGrace, launchBudget)
}

// 实时路径(startTrialAtMatch):match 确定地没开成 —— 同步回了任一拒绝码,或请求预算见底根本没发 Start —— 时把原件提前交给
// 恢复器(否则名单上的人每次业务拒绝都要多挂约 35s 不能再建房或同意);Start 结局未知(传输错误、超时、回成功却没给
// battle_id)时不提前,开成了也不提前。提前检查用独立预算,请求预算见底也照样提交。
func TestStartTrialAtMatchExpeditesOnlySettledFailures(t *testing.T) {
	reject := func(code matchpb.ActivityBattleReject) *matchpb.StartActivityBattleResponse {
		return &matchpb.StartActivityBattleResponse{Reject: code, OffenderPlayerId: 11}
	}
	cases := []struct {
		name            string
		resp            *matchpb.StartActivityBattleResponse
		err             error
		requestTimedOut bool // 请求预算已见底:callTrialMatch 不调 match
		wantStarted     bool
		wantExpedite    bool
	}{
		{name: "成员离线", resp: reject(matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_MEMBER_OFFLINE), wantExpedite: true},
		{name: "成员在战斗中", resp: reject(matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_MEMBER_IN_BATTLE), wantExpedite: true},
		{name: "成员未就绪", resp: reject(matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_MEMBER_NOT_READY), wantExpedite: true},
		{name: "请求非法", resp: reject(matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INVALID_ARGUMENT), wantExpedite: true},
		{name: "match 内部故障", resp: reject(matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INTERNAL), wantExpedite: true},
		{name: "不认识的拒绝码", resp: reject(matchpb.ActivityBattleReject(99)), wantExpedite: true},
		{name: "预算见底没发 Start", requestTimedOut: true, wantExpedite: true},
		{name: "调用超时", err: context.DeadlineExceeded},
		{name: "gRPC Unavailable", err: status.Error(codes.Unavailable, "match is down")},
		{name: "回成功却没有 battle_id", resp: &matchpb.StartActivityBattleResponse{}},
		{name: "开局成功", resp: &matchpb.StartActivityBattleResponse{BattleId: 903}, wantStarted: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := newLaunchRecoveryStore()
			match := &fakeMatch{resp: c.resp, err: c.err}
			d := &ActivityDeps{trials: store, Match: match, MatchBudget: defaultMatchBudget}
			original := trialLaunchOriginalFor(397)
			battle := data.TrialBattleKey{GuildID: original.GuildId, ActivityID: original.ActivityId, PeriodKey: original.PeriodKey,
				GuildPeriodKey: original.GuildPeriodKey, InitiatorPlayerID: original.InitiatorPlayerId, LaunchRequestID: original.RequestId}
			ctx := context.Background()
			if c.requestTimedOut {
				expired, stop := context.WithTimeout(ctx, 0)
				defer stop()
				ctx = expired
			}

			battleID, end := startTrialAtMatch(ctx, d, trialLaunchLobby(original), original.DungeonId, battle)
			require.Equal(t, c.wantStarted, !end.failed() && battleID != 0)
			if c.requestTimedOut {
				require.Zero(t, match.callCount(), "前置:预算见底时不调 match")
			}
			if !c.wantExpedite {
				require.Empty(t, store.expedited, "Start 结局未知或已开局,不提前检查")
				return
			}
			require.Equal(t, []uint64{397}, store.expedited)
			require.Equal(t, []bool{true}, store.expediteCtxLive, "提前检查用独立预算,不继承已见底的请求预算")
		})
	}

	// 没有永久原件(LaunchRequestID=0,未启用 launch 原件的装配):没有可提前的检查。
	store := newLaunchRecoveryStore()
	d := &ActivityDeps{trials: store, Match: &fakeMatch{resp: reject(matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_MEMBER_OFFLINE)}, MatchBudget: defaultMatchBudget}
	original := trialLaunchOriginalFor(397)
	_, end := startTrialAtMatch(context.Background(), d, trialLaunchLobby(original), original.DungeonId, data.TrialBattleKey{GuildID: original.GuildId})
	require.True(t, end.failed())
	require.Empty(t, store.expedited)
}

// 生产装配(newTrialLaunchRecovery,StartTrialBackground 用)释放之后给名单推 ACTIVITY_CHANGED:漏装 notify 这里会红。
func TestTrialLaunchRecoveryProductionWiringPushesRoster(t *testing.T) {
	useTrialTables(t, nil, nil)
	store := newLaunchRecoveryStore(trialLaunchOriginalFor(398))
	cancel := &scriptedCancellation{replies: []scriptedCancelReply{{receipt: &matchpb.CancelActivityBattleResponse{RequestCancelled: true}}}}
	f := newTrialFixture(t, activityGuild(1, 42, 43), func(d *ActivityDeps) {
		d.trials = store
		d.Cancellation = cancel
	})
	store.makeDue()

	require.NoError(t, newTrialLaunchRecovery(f.l).round(context.Background()))
	require.Len(t, store.finished, 1)
	pushes := f.activityPushes()
	require.Len(t, pushes, 1)
	require.EqualValues(t, 44, pushes[0].change.GetGuildId())
	require.Equal(t, []uint64{22, 11}, pushes[0].recipients, "推给原件名单的全体(发起人在首位),不排除任何人")
}
