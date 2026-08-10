package loginpregatelogic

import (
	"context"
	"time"

	"login/internal/config"
	"login/internal/constants"
	"login/internal/logic/pkg/loginqueue"
	"login/internal/logic/pkg/sessionmanager"
	"login/internal/svc"
	loginproto_database "proto/common/database"
	loginpb "proto/login"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

// gateTokenTTL covers the worst-case latency from "dispatcher signs the token"
// through "robot polls /api/queue-status, sees admit, calls VerifyGateToken on
// cpp gate" under 78k-concurrent ramp. Round 18 observed max=403s on that path
// with avg=66s after the R1 fix (commit fb0a9e9f9); the previous 5min budget
// left ~100s of head-room and got eaten by tail-latency spikes — see
// docs/design/stress-1zone-45k-2026-06-04-round18.md §R2. Raised to 10min so
// the TTL covers max + a 6x safety margin. Centralized here so the dispatcher
// (servicecontext.go) and the fast path agree — KEEP THE TWO IN SYNC.
const gateTokenTTL = 10 * time.Minute

type AssignGateLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAssignGateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AssignGateLogic {
	return &AssignGateLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// AssignGate is the main entry point for the "client wants to enter the world"
// pre-connection RPC.
//
// Decision tree (top to bottom; first match wins):
//
//  1. Reentry: req.queue_token already in queue
//     → ParseAndVerifyQueueToken, Lookup, return current state
//     (returns ADMITTED if dispatcher already promoted, else QUEUEING).
//  2. Reconnect/replace exemption: caller already has a live PlayerSession
//     → bypass queue, sign gate token bound to the existing gate.
//     (Disabled when Queue.Enabled=false; reconnect handling is unchanged.)
//  3. Queue disabled (kill switch): legacy behavior, sign and return.
//  4. Queue enabled + capacity available + queue empty: sign and return.
//  5. Otherwise: enqueue, return QUEUEING.
//
// Errors are surfaced via AssignGateResponse.error (HTTP 200 with body) so
// the Java Gateway shim doesn't need separate error-mapping plumbing.
func (l *AssignGateLogic) AssignGate(in *loginpb.AssignGateRequest) (*loginpb.AssignGateResponse, error) {
	queueCfg := config.AppConfig.Queue
	queueEnabled := queueCfg.Enabled && l.svcCtx.LoginQueue != nil

	// 1. Reentry — caller polled with an existing queue token.
	if queueEnabled && in.QueueToken != "" {
		return l.handleReentry(in.QueueToken)
	}

	// 2. Reconnect/replace exemption (queue path only): if a live session
	//    exists for this account, bypass the queue entirely. We deliberately
	//    gate this on queueEnabled — without the queue, the legacy fast path
	//    already handles reconnects implicitly via the gate's
	//    ClientTokenVerifyRequest → BindSession flow.
	//
	//    Why fast-path (not pin to existing gate): the existing
	//    PlayerSession.GateID is a numeric node id, not an IP:port. Looking
	//    up the IP:port would require another etcd query, and even then a
	//    rolling deploy may have replaced that gate already. The downstream
	//    Login → BindSession flow tolerates a different gate just fine
	//    because session state lives in player_locator, not on the gate
	//    itself. The point of the exemption is "don't make a player who's
	//    already in the world wait in line" — not "give them the same gate".
	if queueEnabled && in.Account != "" {
		if existing := l.lookupSessionByAccount(in.Account); existing != nil && existing.GateID != "" {
			logx.Infof("[loginqueue] bypass queue (reconnect) account=%s gate=%s", in.Account, existing.GateID)
			return l.signFastPath(in.ZoneId)
		}
	}

	// 3-4. Fast path: queue disabled OR capacity available.
	if !queueEnabled {
		return l.signFastPath(in.ZoneId)
	}

	free, capacity, online, admitted, err := loginqueue.FreeSlots(
		l.ctx,
		l.svcCtx.QueueCapacityProvider(),
		l.svcCtx.LoginQueue,
		in.ZoneId,
		queueCfg.SoftCapMultiplier,
	)
	if err != nil {
		// No gates / etcd error — surface as error, don't enqueue (a queue
		// without a downstream is a black hole).
		logx.Errorf("[loginqueue] freeSlots zone=%d err=%v", in.ZoneId, err)
		return &loginpb.AssignGateResponse{Status: uint32(loginqueue.StatusError), Error: err.Error()}, nil
	}

	queueLen, _ := l.svcCtx.LoginQueue.QueueLen(l.ctx, in.ZoneId)
	if free > 0 && queueLen == 0 {
		// 快速通道也必须原子占位,否则 free>0 只是一个瞬时快照:N 个并发请求会各读
		// 到同一个 free>0/queueLen==0、全部旁路队列签发 gate token,开服洪峰下把
		// cap 旁路超发。budget = cap-online = 在当前在线之上还允许的在途准入数;
		// TryReserveFastPathSlot 用 Lua 把"判定 admitted<budget + 占位"合成一步。
		budget := uint32(0)
		if capacity > online {
			budget = capacity - online
		}
		reserved, rErr := l.svcCtx.LoginQueue.TryReserveFastPathSlot(l.ctx, in.ZoneId, budget)
		if rErr != nil {
			logx.Errorf("[loginqueue] reserve fast-path slot zone=%d err=%v", in.ZoneId, rErr)
			// 占位失败(Redis 抖动)不放行快速通道 —— 落到下面入队,fail-closed。
		} else if reserved {
			logx.Debugf("[loginqueue] fast-path zone=%d free=%d online=%d admitted=%d budget=%d",
				in.ZoneId, free, online, admitted, budget)
			return l.signFastPath(in.ZoneId)
		}
		// 占位没抢到 = 名额已被并发请求占满,改走入队。
	}

	// 5. Enqueue.
	res, err := l.svcCtx.LoginQueue.Enqueue(l.ctx, in.ZoneId, in.Account, in.DeviceId)
	if err != nil {
		logx.Errorf("[loginqueue] Enqueue zone=%d err=%v", in.ZoneId, err)
		return &loginpb.AssignGateResponse{Status: uint32(loginqueue.StatusError), Error: "queue write failed"}, nil
	}

	return &loginpb.AssignGateResponse{
		Status:       uint32(loginqueue.StatusQueueing),
		QueueToken:   res.QueueToken,
		QueueRank:    res.Rank,
		QueueTotal:   res.Total,
		RetryAfterMs: queueCfg.DefaultRetryAfterMs,
	}, nil
}

// handleReentry resolves a queue_token and returns either ADMITTED (with
// gate token) or QUEUEING (with refreshed rank). Tokens that fail HMAC
// verification map to EXPIRED so the client falls back to a fresh
// /assign-gate call.
func (l *AssignGateLogic) handleReentry(token string) (*loginpb.AssignGateResponse, error) {
	if _, _, err := loginqueue.ParseAndVerifyQueueToken(l.svcCtx.QueueHmacSecret(), token); err != nil {
		logx.Infof("[loginqueue] reentry token rejected: %v", err)
		return &loginpb.AssignGateResponse{Status: uint32(loginqueue.StatusExpired), Error: err.Error()}, nil
	}
	// signFn signs the gate token at consume time (R17 R1 fix) so the
	// 5-min TTL starts when the client receives this response, not when
	// the dispatcher picked the gate minutes earlier.
	signFn := func(slot *loginqueue.AdmitSlot) (*loginqueue.AdmitToken, error) {
		return loginqueue.SignGateToken(&loginqueue.GateCandidate{
			NodeID: slot.NodeID,
			IP:     slot.IP,
			Port:   slot.Port,
			ZoneID: slot.ZoneID,
		}, l.svcCtx.QueueHmacSecret(), gateTokenTTL)
	}
	state, err := l.svcCtx.LoginQueue.Lookup(l.ctx, token, signFn)
	if err != nil {
		return &loginpb.AssignGateResponse{Status: uint32(loginqueue.StatusError), Error: "queue lookup failed"}, nil
	}
	resp := &loginpb.AssignGateResponse{Status: uint32(state.Status)}
	switch state.Status {
	case loginqueue.StatusAdmitted:
		resp.Ip = state.Admit.IP
		resp.Port = state.Admit.Port
		resp.TokenPayload = state.Admit.TokenPayload
		resp.TokenSignature = state.Admit.TokenSignature
		resp.TokenDeadline = state.Admit.TokenDeadline
	case loginqueue.StatusQueueing:
		resp.QueueToken = token
		resp.QueueRank = state.Rank
		resp.QueueTotal = state.Total
		resp.RetryAfterMs = config.AppConfig.Queue.DefaultRetryAfterMs
	}
	return resp, nil
}

// signFastPath signs a gate token directly (no queue involvement). This is
// the legacy AssignGate behavior used when Queue.Enabled=false or when the
// queue is empty AND capacity is available.
func (l *AssignGateLogic) signFastPath(zoneID uint32) (*loginpb.AssignGateResponse, error) {
	candidates, err := l.svcCtx.QueueCapacityProvider().CandidatesForZone(l.ctx, zoneID)
	if err != nil {
		logx.Errorf("[loginqueue] fast-path candidates zone=%d err=%v", zoneID, err)
		return &loginpb.AssignGateResponse{
			Status: uint32(loginqueue.StatusError),
			Error:  "no gate available for requested zone",
		}, nil
	}
	if len(candidates) == 0 {
		logx.Errorf("[loginqueue] fast-path zone=%d has zero candidates", zoneID)
		return &loginpb.AssignGateResponse{
			Status: uint32(loginqueue.StatusError),
			Error:  "no gate available for requested zone",
		}, nil
	}
	admit, err := loginqueue.PickAndSignGateToken(candidates, l.svcCtx.QueueHmacSecret(), gateTokenTTL)
	if err != nil {
		logx.Errorf("[loginqueue] fast-path sign zone=%d err=%v", zoneID, err)
		return &loginpb.AssignGateResponse{Status: uint32(loginqueue.StatusError), Error: "sign failed"}, nil
	}
	return &loginpb.AssignGateResponse{
		Status:         uint32(loginqueue.StatusAdmitted),
		Ip:             admit.IP,
		Port:           admit.Port,
		TokenPayload:   admit.TokenPayload,
		TokenSignature: admit.TokenSignature,
		TokenDeadline:  admit.TokenDeadline,
	}, nil
}

// lookupSessionByAccount returns a live PlayerSession for the given
// account, or nil if none / lookup fails. We resolve account → playerId
// via the existing account_data Redis key, then delegate to player_locator.
//
// This is best-effort: a failure here just means we treat the caller as a
// fresh login and route them through the queue, which is the conservative
// choice (no false bypass).
func (l *AssignGateLogic) lookupSessionByAccount(account string) *sessionmanager.PlayerSession {
	// account_data:{account} → UserAccounts proto with SimplePlayers list.
	// We pick the first player_id; full multi-character logic can be added
	// later if it turns out players actually maintain >1 active session.
	accountKey := constants.GetAccountDataKey(account)
	data, err := l.svcCtx.RedisClient.Get(l.ctx, accountKey).Bytes()
	if err != nil || len(data) == 0 {
		return nil
	}
	var ua loginproto_database.UserAccounts
	if err := proto.Unmarshal(data, &ua); err != nil {
		return nil
	}
	if ua.SimplePlayers == nil || len(ua.SimplePlayers.Players) == 0 {
		return nil
	}
	playerID := ua.SimplePlayers.Players[0].PlayerId
	if playerID == 0 {
		return nil
	}
	existing, err := sessionmanager.GetSession(l.ctx, l.svcCtx.PlayerLocatorClient, playerID)
	if err != nil {
		return nil
	}
	return existing
}
