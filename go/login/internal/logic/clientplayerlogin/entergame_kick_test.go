package clientplayerloginlogic

// ReplaceLogin 踢线挪到 SetSession 提交之后(docs/design/scene-enter-chain-residuals.md §2,余项 a)。
//
// 旧代码先踢旧会话、后提交 SetSession:提交失败(失锁、ctx 取消、locator 拒绝)的链也会把当前合法会话踢掉。
// 这里盯住:只在确认提交之后踢;结果未知时独立 ctx 复读消歧,复读确认已提交时补挂断线租约;
// 同一连接 / 跨实例同号 / 本链实例号为空三种情形按 §2.2 ⑤ 不踢并计数。
//
// 本文件自带 harness,不复用、也不改 newEnterWiringHarness(player_identity_backfill_test.go):后者的假 locator
// SetSession / Reconnect 恒报错、GetSession 只回固定记录、SessionDetails 只有 SessionId,满足不了下面的用例(§2.7)。
//   - 直接驱动 applyLoadedPlayerSession(异步链的 apply 段),链 ctx 由用例掌握,才能模拟 onLost 取消;
//   - state 由生产的 buildEnterGameSessionState 从 SessionDetails 构造:gate_instance_id 非空、gate_node_id = 0。
//     这样 SetSession 成功之后本链的 Bind 在 buildGateCommandMessage 守卫三(node id 0)报错返回,碰不到 KafkaClient
//     (这里为 nil,碰到即 panic,整个测试二进制崩掉)。踢线目标取自 existing 记录(GateID "7"、实例 "uuid-7"),
//     走 kicker 接缝,同样碰不到 Kafka;
//   - 「本链实例号为空」的用例另设 gate_instance_id 为空,Bind 在守卫一报错返回。
// 辅助标识符统一加 kick 前缀,避免与同包已有的 enterWiring* / fenceWiring* 撞名。

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"login/internal/logic/pkg/sessionmanager"
	"login/internal/svc"
	pbbase "proto/common/base"
	loginpb "proto/login"
	plpb "proto/player_locator"
	"shared/accountdirectory"
)

const (
	kickPlayerID         = uint64(7101)
	kickAccount          = "acct-kick"
	kickChainSessionID   = uint32(5252)
	kickChainInstance    = "uuid-chain"
	kickOldSessionID     = uint32(1)
	kickOldGateID        = "7"
	kickOldInstance      = "uuid-7"
	kickRecordGeneration = uint64(1_000)
)

var errKickSetSessionUnavailable = errors.New("kick test: player_locator SetSession unavailable")

// kickEventLog 按发生顺序记下 locator 与 kicker 的调用,用来断言「先提交、后踢线」。
type kickEventLog struct {
	mu  sync.Mutex
	seq []string
}

func (e *kickEventLog) add(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seq = append(e.seq, name)
}

func (e *kickEventLog) snapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.seq...)
}

// kickCtxObservation 是假 locator 在**收到调用的那一刻**看到的 ctx 状态(调用返回后 ctx 会被调用方 cancel)。
type kickCtxObservation struct {
	err         error
	hasDeadline bool
	remaining   time.Duration
}

func observeKickCtx(ctx context.Context) kickCtxObservation {
	deadline, ok := ctx.Deadline()
	obs := kickCtxObservation{err: ctx.Err(), hasDeadline: ok}
	if ok {
		obs.remaining = time.Until(deadline)
	}
	return obs
}

// kickGetSessionReply 是 GetSession 的一次应答。echoCommitted = true 时回最近一次 SetSession 请求里的会话
// (模拟「locator 已写入、应答却丢了」);否则回 session(nil = 无会话)与 err。
type kickGetSessionReply struct {
	session       *plpb.PlayerSession
	err           error
	echoCommitted bool
}

// kickLocator 是 player_locator 的假客户端,只实现这段路会碰的 RPC;别的走内嵌的 nil 接口,调到即 panic。
type kickLocator struct {
	plpb.PlayerLocatorClient

	events *kickEventLog

	// 只在调用 applyLoadedPlayerSession 之前写。
	getSessionReplies []kickGetSessionReply // 按调用序取;超出的调用重复最后一个
	setSessionErr     error
	onSetSession      func() // SetSession 收到请求时先执行(模拟心跳丢锁 chainCancel)

	mu                sync.Mutex
	getSessionCalls   int
	getSessionCtxs    []kickCtxObservation
	setSessions       []*plpb.SetSessionRequest
	disconnecting     []*plpb.SetDisconnectingRequest
	disconnectingCtxs []kickCtxObservation
}

func (f *kickLocator) GetSession(ctx context.Context, _ *plpb.GetSessionRequest, _ ...grpc.CallOption) (*plpb.GetSessionResponse, error) {
	f.events.add("GetSession")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getSessionCtxs = append(f.getSessionCtxs, observeKickCtx(ctx))
	idx := f.getSessionCalls
	f.getSessionCalls++
	if len(f.getSessionReplies) == 0 {
		return &plpb.GetSessionResponse{}, nil
	}
	if idx >= len(f.getSessionReplies) {
		idx = len(f.getSessionReplies) - 1
	}
	reply := f.getSessionReplies[idx]
	if reply.err != nil {
		return nil, reply.err
	}
	session := reply.session
	if reply.echoCommitted {
		session = nil
		if n := len(f.setSessions); n > 0 {
			session = f.setSessions[n-1].GetSession()
		}
	}
	if session == nil {
		return &plpb.GetSessionResponse{}, nil
	}
	return &plpb.GetSessionResponse{Found: true, Session: proto.Clone(session).(*plpb.PlayerSession)}, nil
}

func (f *kickLocator) SetSession(_ context.Context, in *plpb.SetSessionRequest, _ ...grpc.CallOption) (*pbbase.Empty, error) {
	f.events.add("SetSession")
	if f.onSetSession != nil {
		f.onSetSession()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setSessions = append(f.setSessions, proto.Clone(in).(*plpb.SetSessionRequest))
	if f.setSessionErr != nil {
		return nil, f.setSessionErr
	}
	return &pbbase.Empty{}, nil
}

func (f *kickLocator) SetDisconnecting(ctx context.Context, in *plpb.SetDisconnectingRequest, _ ...grpc.CallOption) (*pbbase.Empty, error) {
	f.events.add("SetDisconnecting")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disconnecting = append(f.disconnecting, proto.Clone(in).(*plpb.SetDisconnectingRequest))
	f.disconnectingCtxs = append(f.disconnectingCtxs, observeKickCtx(ctx))
	return &pbbase.Empty{}, nil
}

func (f *kickLocator) committedGeneration(t *testing.T) uint64 {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.setSessions) != 1 {
		t.Fatalf("SetSession calls = %d, want exactly 1", len(f.setSessions))
	}
	gen := f.setSessions[0].GetSession().GetLoginGeneration()
	if gen == 0 {
		t.Fatal("SetSession carried login_generation 0")
	}
	return gen
}

// kickCall 是一次 KickSessionOnGate 调用的全部参数。
type kickCall struct {
	gateID          string
	gateInstanceID  string
	sessionID       uint32
	playerID        uint64
	loginGeneration uint64
}

// kickRecorder 是 replacedSessionKicker 的测试实现:记下参数,按 err 回结果,不碰 Kafka。
type kickRecorder struct {
	events *kickEventLog
	err    error

	mu    sync.Mutex
	calls []kickCall
}

func (k *kickRecorder) KickSessionOnGate(gateID string, gateInstanceID string, sessionID uint32, playerID uint64, loginGeneration uint64) error {
	k.events.add("Kick")
	k.mu.Lock()
	defer k.mu.Unlock()
	k.calls = append(k.calls, kickCall{gateID, gateInstanceID, sessionID, playerID, loginGeneration})
	return k.err
}

func (k *kickRecorder) recorded() []kickCall {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]kickCall(nil), k.calls...)
}

type kickHarness struct {
	l       *EnterGameLogic
	rdb     *redis.Client
	locator *kickLocator
	kicker  *kickRecorder
	events  *kickEventLog
	state   enterGameSessionState
	// bindSent 是最近一次 apply 记下的「Bind 已写进 Kafka」。
	bindSent bool
}

// newKickHarness:existing 是锁内首读到的会话记录(ONLINE → ReplaceLogin);chainInstance 是本链连接所在的
// gate 实例号(SessionDetails.gate_instance_id),gate_node_id 恒为 0(见文件头)。
func newKickHarness(t *testing.T, existing *plpb.PlayerSession, chainInstance string) *kickHarness {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	events := &kickEventLog{}
	locator := &kickLocator{
		events: events,
		// 默认:首读与复读都回 existing(复读看不到本链 = 未提交)。
		getSessionReplies: []kickGetSessionReply{{session: existing}},
	}
	kicker := &kickRecorder{events: events}
	l := NewEnterGameLogic(context.Background(), &svc.ServiceContext{
		RedisClient: rdb, AccountDirectory: fixtureDirectory(t, rdb),
		PlayerLocatorClient: locator,
	})
	l.kicker = kicker

	state := buildEnterGameSessionState(
		&loginpb.EnterGameRequest{PlayerId: kickPlayerID, RequestId: "kick-req"},
		&pbbase.SessionDetails{SessionId: kickChainSessionID, GateNodeId: 0, GateInstanceId: chainInstance},
		kickAccount,
	)
	fixtureDirectory(t, rdb).seed(kickAccount, &pbbase.AccountSimplePlayer{PlayerId: kickPlayerID})
	state.ownerRevision = 1
	return &kickHarness{l: l, rdb: rdb, locator: locator, kicker: kicker, events: events, state: state}
}

// kickExistingRecord 是被替换的旧会话记录:ONLINE(于是判 ReplaceLogin),gate "7"。
func kickExistingRecord(sessionID uint32, gateInstance string) *plpb.PlayerSession {
	return &plpb.PlayerSession{
		PlayerId:        kickPlayerID,
		SessionId:       sessionID,
		GateId:          kickOldGateID,
		GateInstanceId:  gateInstance,
		Account:         kickAccount,
		SessionVersion:  4,
		State:           plpb.PlayerSessionState_SESSION_STATE_ONLINE,
		LoginGeneration: kickRecordGeneration,
	}
}

func (h *kickHarness) apply(ctx context.Context) error {
	h.bindSent = false
	_, err := h.l.applyLoadedPlayerSession(ctx, h.state, &h.bindSent)
	return err
}

func TestApplyLoadedPlayerSessionRejectsFrozenOrChangedDirectoryBeforeSessionWrites(t *testing.T) {
	for _, change := range []string{"frozen", "revision", "account", "SQL-failed", "unready"} {
		t.Run(change, func(t *testing.T) {
			h := newKickHarness(t, kickExistingRecord(kickOldSessionID, kickOldInstance), kickChainInstance)
			directory := fixtureDirectory(t, h.rdb)
			directory.mu.Lock()
			switch change {
			case "frozen":
				directory.owners[kickPlayerID].TradeListingId = 101
			case "revision":
				directory.owners[kickPlayerID].OwnerRevision++
			case "account":
				directory.owners[kickPlayerID].Account = "new-owner"
			case "SQL-failed":
				directory.lookupErr = accountdirectory.ErrUnavailable
			case "unready":
				h.l.svcCtx.AccountDirectory = nil
			}
			directory.mu.Unlock()
			if err := h.apply(context.Background()); err == nil {
				t.Fatal("目录冻结、归属变化或不可读的旧异步链不能继续入场")
			}
			if events := h.events.snapshot(); len(events) != 0 {
				t.Fatalf("拒绝后不应读写locator、踢线或触及后续进场: %v", events)
			}
		})
	}
}

// kickCount 读 login_kick_total{result}。指标是进程级全局量,用例一律比较前后差值。
func kickCount(t *testing.T, result string) float64 {
	t.Helper()
	return createPlayerCounterValue(t, "login_kick_total", map[string]string{"result": result})
}

func requireKickDelta(t *testing.T, result string, before float64, want float64) {
	t.Helper()
	if got := kickCount(t, result) - before; got != want {
		t.Fatalf("login_kick_total{result=%q} delta = %v, want %v", result, got, want)
	}
}

// 旧代码先踢后提交:SetSession 失败、复读也不是本链时,旧会话已经被踢了(红)。
func TestEnterGame_ReplaceLoginSetSessionFailureDoesNotKick(t *testing.T) {
	h := newKickHarness(t, kickExistingRecord(kickOldSessionID, kickOldInstance), kickChainInstance)
	h.locator.setSessionErr = errKickSetSessionUnavailable
	before := kickCount(t, kickResultSkipUncommitted)

	err := h.apply(context.Background())

	if !errors.Is(err, errKickSetSessionUnavailable) {
		t.Fatalf("apply error = %v, want the SetSession error", err)
	}
	if calls := h.kicker.recorded(); len(calls) != 0 {
		t.Fatalf("kicks = %+v, want none: the chain never committed, the replaced session is still the legitimate one", calls)
	}
	requireKickDelta(t, kickResultSkipUncommitted, before, 1)
}

// 事件序列必须是 GetSession → SetSession → Kick;踢线参数取 existing 的寻址、本链玩家与本链铸出的代次(旧代码顺序相反,红)。
func TestEnterGame_ReplaceLoginKicksOnlyAfterSetSessionCommits(t *testing.T) {
	h := newKickHarness(t, kickExistingRecord(kickOldSessionID, kickOldInstance), kickChainInstance)
	before := kickCount(t, kickResultSent)

	err := h.apply(context.Background())
	// 提交之后本链的 Bind 因 gate_node_id = 0 被守卫拒掉,链按失败收尾;这不影响踢线已发生。
	if err == nil || !strings.Contains(err.Error(), "gate node id is 0") {
		t.Fatalf("apply error = %v, want the chain to reach Bind and stop at the node-id guard", err)
	}

	if got, want := h.events.snapshot(), []string{"GetSession", "SetSession", "Kick"}; !slices.Equal(got, want) {
		t.Fatalf("event sequence = %v, want %v (kick only after the session record is committed)", got, want)
	}
	want := kickCall{
		gateID:          kickOldGateID,
		gateInstanceID:  kickOldInstance,
		sessionID:       kickOldSessionID,
		playerID:        kickPlayerID,
		loginGeneration: h.locator.committedGeneration(t),
	}
	if calls := h.kicker.recorded(); len(calls) != 1 || calls[0] != want {
		t.Fatalf("kicks = %+v, want exactly [%+v]", calls, want)
	}
	requireKickDelta(t, kickResultSent, before, 1)
}

// 同一连接重登(会话号与实例号都相同):不踢,否则本连接的 gate 会话仍绑着更小的代次,ClassifyKick 会判 kKick 自踢(回归)。
// 计数也要落在 skip_same_session:§2.2 ⑤ 第 3、4 行踢线次数都是 0,只能靠 result 区分「同一连接」与「跨实例同号」。
func TestEnterGame_SameSessionSameInstanceDoesNotKick(t *testing.T) {
	h := newKickHarness(t, kickExistingRecord(kickChainSessionID, kickChainInstance), kickChainInstance)
	beforeSame := kickCount(t, kickResultSkipSameSession)
	beforeCross := kickCount(t, kickResultSkipCrossInstance)

	_ = h.apply(context.Background())

	if calls := h.kicker.recorded(); len(calls) != 0 {
		t.Fatalf("kicks = %+v, want none for the same connection", calls)
	}
	requireKickDelta(t, kickResultSkipSameSession, beforeSame, 1)
	requireKickDelta(t, kickResultSkipCrossInstance, beforeCross, 0)
}

// 跨实例同号(gate 以同一 node_id 重启,同号落到新实例):按 §12.3 定论只计数不踢(result=skip_cross_instance)。
// 设计 §2.7 原表此行是「踢 1 次」,§12.3 定论改为不踢;旧代码同号直接跳过,踢线次数相同,只差计数。
func TestEnterGame_SameSessionDifferentInstanceSkipsCrossInstance(t *testing.T) {
	h := newKickHarness(t, kickExistingRecord(kickChainSessionID, "uuid-old"), kickChainInstance)
	before := kickCount(t, kickResultSkipCrossInstance)

	_ = h.apply(context.Background())

	if calls := h.kicker.recorded(); len(calls) != 0 {
		t.Fatalf("kicks = %+v, want none: cross-instance same-session kicks stay off until the scene-side fence lands", calls)
	}
	requireKickDelta(t, kickResultSkipCrossInstance, before, 1)
}

// 本链实例号为空:这条链必在 Bind 的守卫一失败,无论会话号都不踢(旧代码会话号不同的那例会先踢掉旧设备,红)。
func TestEnterGame_ChainWithoutGateInstanceDoesNotKick(t *testing.T) {
	cases := map[string]*plpb.PlayerSession{
		"same session id":      kickExistingRecord(kickChainSessionID, kickOldInstance),
		"different session id": kickExistingRecord(kickOldSessionID, kickOldInstance),
	}
	for name, existing := range cases {
		t.Run(name, func(t *testing.T) {
			h := newKickHarness(t, existing, "")
			before := kickCount(t, kickResultSkipNoTarget)

			_ = h.apply(context.Background())

			if calls := h.kicker.recorded(); len(calls) != 0 {
				t.Fatalf("kicks = %+v, want none when the chain has no gate instance", calls)
			}
			requireKickDelta(t, kickResultSkipNoTarget, before, 1)
		})
	}
}

// 心跳丢锁取消链 ctx(onLost → chainCancel),SetSession 因此失败,复读未见本链:不踢(旧代码踢线不带 ctx,照发,红)。
func TestEnterGame_CanceledChainDoesNotKick(t *testing.T) {
	h := newKickHarness(t, kickExistingRecord(kickOldSessionID, kickOldInstance), kickChainInstance)
	chainCtx, chainCancel := context.WithCancel(context.Background())
	defer chainCancel()
	h.locator.onSetSession = chainCancel
	h.locator.setSessionErr = context.Canceled
	before := kickCount(t, kickResultSkipUncommitted)

	err := h.apply(chainCtx)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("apply error = %v, want context.Canceled from SetSession", err)
	}
	if calls := h.kicker.recorded(); len(calls) != 0 {
		t.Fatalf("kicks = %+v, want none from a canceled chain", calls)
	}
	requireKickDelta(t, kickResultSkipUncommitted, before, 1)
}

// newKickUnknownOutcomeHarness:SetSession 写进去了、应答却报错;复读回本链刚写下的记录。
func newKickUnknownOutcomeHarness(t *testing.T) *kickHarness {
	t.Helper()
	h := newKickHarness(t, kickExistingRecord(kickOldSessionID, kickOldInstance), kickChainInstance)
	h.locator.setSessionErr = errKickSetSessionUnavailable
	h.locator.getSessionReplies = []kickGetSessionReply{
		{session: kickExistingRecord(kickOldSessionID, kickOldInstance)},
		{echoCommitted: true},
	}
	return h
}

func TestEnterGame_UnknownOutcomeProbeConfirmsCommitKicks(t *testing.T) {
	h := newKickUnknownOutcomeHarness(t)
	before := kickCount(t, kickResultSent)

	err := h.apply(context.Background())

	if !errors.Is(err, errKickSetSessionUnavailable) {
		t.Fatalf("apply error = %v, want the original SetSession error (the chain still fails: bindSessionFence never ran)", err)
	}
	want := kickCall{
		gateID:          kickOldGateID,
		gateInstanceID:  kickOldInstance,
		sessionID:       kickOldSessionID,
		playerID:        kickPlayerID,
		loginGeneration: h.locator.committedGeneration(t),
	}
	if calls := h.kicker.recorded(); len(calls) != 1 || calls[0] != want {
		t.Fatalf("kicks = %+v, want exactly [%+v]", calls, want)
	}
	requireKickDelta(t, kickResultSent, before, 1)
}

// 复读确认已提交时补挂断线租约:SetDisconnecting 恰好 1 次,参数为 (玩家, 本会话号, 本链实例号),
// 收到的 ctx 未取消、带截止时间(独立于链 ctx)。
func TestEnterGame_ProbeConfirmedCommitPlacesDisconnectLease(t *testing.T) {
	h := newKickUnknownOutcomeHarness(t)
	chainCtx, chainCancel := context.WithCancel(context.Background())
	defer chainCancel()
	// 结果未知的常见成因:链 ctx 在 SetSession 途中被取消。补挂租约必须不受它影响。
	h.locator.onSetSession = chainCancel

	_ = h.apply(chainCtx)

	h.locator.mu.Lock()
	defer h.locator.mu.Unlock()
	if len(h.locator.disconnecting) != 1 {
		t.Fatalf("SetDisconnecting calls = %d, want exactly 1 (otherwise the committed record stays ONLINE without a lease)", len(h.locator.disconnecting))
	}
	req := h.locator.disconnecting[0]
	if req.GetPlayerId() != kickPlayerID || req.GetSessionId() != kickChainSessionID || req.GetGateInstanceId() != kickChainInstance {
		t.Fatalf("SetDisconnecting = player %d session %d instance %q, want %d/%d/%q",
			req.GetPlayerId(), req.GetSessionId(), req.GetGateInstanceId(), kickPlayerID, kickChainSessionID, kickChainInstance)
	}
	obs := h.locator.disconnectingCtxs[0]
	if obs.err != nil {
		t.Fatalf("SetDisconnecting ctx already done (%v); the lease must not inherit the canceled chain ctx", obs.err)
	}
	if !obs.hasDeadline {
		t.Fatal("SetDisconnecting ctx has no deadline; the compensation must be bounded")
	}
}

func TestEnterGame_UnconfirmedOutcomePlacesNoLease(t *testing.T) {
	h := newKickHarness(t, kickExistingRecord(kickOldSessionID, kickOldInstance), kickChainInstance)
	h.locator.setSessionErr = errKickSetSessionUnavailable

	_ = h.apply(context.Background())

	h.locator.mu.Lock()
	defer h.locator.mu.Unlock()
	if n := len(h.locator.disconnecting); n != 0 {
		t.Fatalf("SetDisconnecting calls = %d, want 0 when the probe does not see this chain's record", n)
	}
}

// 链 ctx 已取消,复读仍能执行:第二次 GetSession(复读)收到的 ctx 未取消、带不超过 setSessionProbeTimeout 的截止时间。
func TestEnterGame_ProbeUsesIndependentDeadline(t *testing.T) {
	h := newKickUnknownOutcomeHarness(t)
	chainCtx, chainCancel := context.WithCancel(context.Background())
	defer chainCancel()
	h.locator.onSetSession = chainCancel

	_ = h.apply(chainCtx)

	h.locator.mu.Lock()
	defer h.locator.mu.Unlock()
	if h.locator.getSessionCalls != 2 {
		t.Fatalf("GetSession calls = %d, want 2 (first read + one probe)", h.locator.getSessionCalls)
	}
	probe := h.locator.getSessionCtxs[1]
	if probe.err != nil {
		t.Fatalf("probe ctx already done (%v); it must be detached from the canceled chain ctx", probe.err)
	}
	if !probe.hasDeadline || probe.remaining > setSessionProbeTimeout {
		t.Fatalf("probe ctx deadline present=%v remaining=%v, want a deadline within %v", probe.hasDeadline, probe.remaining, setSessionProbeTimeout)
	}
}

// 提交之后踢线写 Kafka 失败:只计 send_failed,链照常往下走到 Bind(提交已经发生,不能因踢线失败而失败)。
func TestEnterGame_KickSendFailureDoesNotFailChain(t *testing.T) {
	h := newKickHarness(t, kickExistingRecord(kickOldSessionID, kickOldInstance), kickChainInstance)
	h.kicker.err = errors.New("kick test: kafka unavailable")
	before := kickCount(t, kickResultSendFailed)

	err := h.apply(context.Background())

	if err == nil || !strings.Contains(err.Error(), "gate node id is 0") {
		t.Fatalf("apply error = %v, want the chain to continue past the failed kick and stop at the Bind node-id guard", err)
	}
	requireKickDelta(t, kickResultSendFailed, before, 1)
}

// 复读判定:会话号与本链相同、代次等于本链铸出值且非 0,才算本链已提交。
func TestSetSessionProbeConfirmsCommit(t *testing.T) {
	const gen = uint64(1_760_000_000_001)
	cases := []struct {
		name   string
		record *sessionmanager.PlayerSession
		gen    uint64
		want   bool
	}{
		{"this chain's record", &sessionmanager.PlayerSession{SessionID: kickChainSessionID, LoginGeneration: gen}, gen, true},
		{"no record", nil, gen, false},
		{"other session", &sessionmanager.PlayerSession{SessionID: kickOldSessionID, LoginGeneration: gen}, gen, false},
		{"same session, other login", &sessionmanager.PlayerSession{SessionID: kickChainSessionID, LoginGeneration: gen - 1}, gen, false},
		{"zero generation cannot identify the commit", &sessionmanager.PlayerSession{SessionID: kickChainSessionID}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := setSessionProbeConfirmsCommit(tc.record, kickChainSessionID, tc.gen); got != tc.want {
				t.Fatalf("setSessionProbeConfirmsCommit = %v, want %v", got, tc.want)
			}
		})
	}
}
