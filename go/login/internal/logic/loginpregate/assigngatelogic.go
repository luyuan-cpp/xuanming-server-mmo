package loginpregatelogic

import (
	"context"
	"errors"
	"strconv"
	"time"

	"login/internal/config"
	"login/internal/logic/pkg/loginqueue"
	"login/internal/logic/pkg/sessionmanager"
	"login/internal/svc"
	base "proto/common/base"
	loginpb "proto/login"
	"shared/cellroute"

	"github.com/zeromicro/go-zero/core/logx"
)

// gateTokenTTL covers the worst-case latency from "dispatcher signs the token"
// through "robot polls /api/queue-status, sees admit, calls VerifyGateToken on
// cpp gate" under 78k-concurrent ramp. Round 18 observed max=403s on that path
// with avg=66s after the R1 fix (commit fb0a9e9f9); the previous 5min budget
// left ~100s of head-room and got eaten by tail-latency spikes — see
// docs/stress/stress-1zone-45k-2026-06-04-round18.md §R2. Raised to 10min so
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
//     → ParseAndVerifyQueueToken, Poll, return current state
//     (returns ADMITTED if dispatcher already promoted, else QUEUEING).
//     (2026-10-18 补注:也可能回 EXPIRED——admit_window_passed / meta_gone / queue_gone,
//     判定顺序见 loginqueue.Queue.Poll;传给 Poll 的 zone 一律取自已验签的令牌。)
//  2. Authenticate: account comes only from req.access_token (authenticateAssignGate, assigngate_auth.go).
//     Invalid token → UNAUTHENTICATED; no token → anonymous only while LegacyGateLoginEnabled.
//  3. Queue disabled (kill switch): legacy behavior, sign and return.
//  4. Queue enabled, but the request is for another zone (or zone 0): ERROR. This login only queues its own zone.
//  5. Reconnect/replace exemption: one of the account's roles already holds a live PlayerSession in this zone
//     (reconnectingPlayer) → hold an in-flight slot for it, bypass the queue and sign.
//     (Anonymous callers have no account to look up.)
//  6. Capacity available + queue empty: reserve an in-flight slot, sign and return.
//  7. Otherwise: enqueue (queue identity = authenticated account, "" if anonymous), return QUEUEING —
//     or QUEUE_FULL when the zone queue is at Queue.MaxQueueLength.
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

	// 2. 鉴权。身份只从 access token 解出,不看请求正文;排队重入在上面已经返回,排队令牌本身就是先前分配的凭证。
	account, denied := authenticateAssignGate(l.ctx, l.svcCtx.TokenManager, in.AccessToken, config.AppConfig.LegacyGateLoginEnabled)
	if denied != nil {
		return denied, nil
	}

	// 3. 排队关着:直接签票,与没有排队时相同。重连本来就由 gate 的验票与 BindSession 处理。
	if !queueEnabled {
		return l.signFastPath(in.ZoneId)
	}

	// 4. 排队开着时只接本区的请求。队列、名额与放行循环都按本 login 的 Node.ZoneId:别的 zone(含 0)的条目写进去
	//    没有 dispatcher 出队,只进不出。网关按 zone 选 login、zone <= 0 不转发,正常走不到这里;挡的是绕过网关直连的调用方。
	if ownZone := config.AppConfig.Node.ZoneId; in.ZoneId != ownZone {
		logx.Errorf("[loginqueue] AssignGate for zone %d reached the login of zone %d; rejected", in.ZoneId, ownZone)
		return &loginpb.AssignGateResponse{Status: uint32(loginqueue.StatusError), Error: "zone is not served by this login"}, nil
	}

	// 5. 重连 / 顶号免排队:已经在本区游戏里的人不用再排一次。签的仍是快速通道那样现挑的 gate,不钉原来那台:
	//    会话记的 GateID 只是节点号,那台 gate 可能已经换掉;会话状态在 player_locator,换一台 gate 照样接得上。
	//    免排队的人不经过名额判断,所以先给他记一枚在途占位,连上之前由占位替他占着名额(HoldReconnectSlot)。
	//    占位写不进不拦他:已经在游戏里的人不该因为 Redis 抖动被挡在外面,代价是这一个人在连上之前没被算进在途。
	if account != "" {
		if playerID, ok := l.reconnectingPlayer(account, in.ZoneId); ok {
			if err := l.svcCtx.LoginQueue.HoldReconnectSlot(l.ctx, in.ZoneId, playerID); err != nil {
				logx.Errorf("[loginqueue] hold reconnect slot zone=%d player=%d err=%v", in.ZoneId, playerID, err)
			}
			loginqueue.RecordAssign(in.ZoneId, loginqueue.AssignOutcomeReconnect)
			return l.signFastPathForPlayer(in.ZoneId, playerID)
		}
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

	// 6. 有空位、没人在排:快速通道。读不出队列长度时按「有人在排」处理,不走快速通道(不插队),落到下面入队。
	queueLen, lenErr := l.svcCtx.LoginQueue.QueueLen(l.ctx, in.ZoneId)
	if lenErr != nil {
		logx.Errorf("[loginqueue] queueLen zone=%d err=%v", in.ZoneId, lenErr)
	}
	if lenErr == nil && free > 0 && queueLen == 0 {
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
			loginqueue.RecordAssign(in.ZoneId, loginqueue.AssignOutcomeFastPath)
			return l.signFastPath(in.ZoneId)
		}
		// 占位没抢到 = 名额已被并发请求占满,改走入队。
	}

	// 7. 入队。排队身份是鉴权得到的账号:同一账号在本区只有一个条目,再次入队沿用原位置(loginqueue.Queue.Enqueue)。
	//    匿名分配为空串,每次都是新条目。队列到上限时不入队,回「排队已满」,客户端等够 retry_after_ms 再来。
	res, err := l.svcCtx.LoginQueue.Enqueue(l.ctx, in.ZoneId, account, in.DeviceId)
	if errors.Is(err, loginqueue.ErrQueueFull) {
		loginqueue.RecordAssign(in.ZoneId, loginqueue.AssignOutcomeQueueFull)
		return &loginpb.AssignGateResponse{
			Status:       uint32(loginqueue.StatusQueueFull),
			RetryAfterMs: loginqueue.RetryAfterWhenFull(),
		}, nil
	}
	if err != nil {
		logx.Errorf("[loginqueue] Enqueue zone=%d err=%v", in.ZoneId, err)
		return &loginpb.AssignGateResponse{Status: uint32(loginqueue.StatusError), Error: "queue write failed"}, nil
	}
	if res.Rejoined {
		loginqueue.RecordAssign(in.ZoneId, loginqueue.AssignOutcomeRejoined)
	} else {
		loginqueue.RecordAssign(in.ZoneId, loginqueue.AssignOutcomeEnqueued)
	}

	return &loginpb.AssignGateResponse{
		Status:       uint32(loginqueue.StatusQueueing),
		QueueToken:   res.QueueToken,
		QueueRank:    res.Rank,
		QueueTotal:   res.Total,
		RetryAfterMs: loginqueue.RetryAfterForRank(queueCfg.DefaultRetryAfterMs, res.Rank, queueCfg.AdmitTTL),
	}, nil
}

// handleReentry resolves a queue_token and returns either ADMITTED (with
// gate token) or QUEUEING (with refreshed rank). Tokens that fail HMAC
// verification map to EXPIRED so the client falls back to a fresh
// /assign-gate call.
func (l *AssignGateLogic) handleReentry(token string) (*loginpb.AssignGateResponse, error) {
	// zoneID 必须取自已验签的排队令牌:Poll 拿它拼队列键并核对 meta.zone_id(2026-10-18),
	// 不得用请求里的 zone 或常量代替;传 0 会被当成损坏、回 EXPIRED(meta_gone)。
	_, zoneID, err := loginqueue.ParseAndVerifyQueueTokenMulti(l.svcCtx.QueueTokenVerifySecrets(), token)
	if err != nil {
		logx.Infof("[loginqueue] reentry token rejected: %v", err)
		return &loginpb.AssignGateResponse{Status: uint32(loginqueue.StatusExpired), Error: err.Error()}, nil
	}
	// 取走时现签(R17 R1),TTL 从这次回包起算,而不是从 dispatcher 几分钟前挑 gate 时算。签之前复核放行时选定的 gate,
	// 它已停写、满载或不在候选里就按快速通道的规则重挑(loginqueue.NewAdmitSigner,与 QueryQueueStatus 同一份)。
	signFn := loginqueue.NewAdmitSigner(l.ctx, l.svcCtx.QueueCapacityProvider(), l.svcCtx.GateTokenSigningSecret(), gateTokenTTL)
	state, err := l.svcCtx.LoginQueue.Poll(l.ctx, token, zoneID, signFn)
	if err != nil {
		return &loginpb.AssignGateResponse{Status: uint32(loginqueue.StatusError), Error: "queue lookup failed"}, nil
	}
	resp := &loginpb.AssignGateResponse{Status: uint32(state.Status)}
	switch state.Status {
	case loginqueue.StatusAdmitted:
		resp.Ip = state.Admit.IP
		resp.Port = state.Admit.Port
		resp.WsUrl = state.Admit.WsURL
		resp.TokenPayload = state.Admit.TokenPayload
		resp.TokenSignature = state.Admit.TokenSignature
		resp.TokenDeadline = state.Admit.TokenDeadline
	case loginqueue.StatusQueueing:
		resp.QueueToken = token
		resp.QueueRank = state.Rank
		resp.QueueTotal = state.Total
		resp.RetryAfterMs = loginqueue.RetryAfterForRank(config.AppConfig.Queue.DefaultRetryAfterMs, state.Rank, config.AppConfig.Queue.AdmitTTL)
	}
	return resp, nil
}

// signFastPath signs a gate token directly (no queue involvement). This is
// the legacy AssignGate behavior used when Queue.Enabled=false or when the
// queue is empty AND capacity is available.
func (l *AssignGateLogic) signFastPath(zoneID uint32) (*loginpb.AssignGateResponse, error) {
	return l.signFastPathForPlayer(zoneID, 0)
}

func (l *AssignGateLogic) signFastPathForPlayer(zoneID uint32, playerID uint64) (*loginpb.AssignGateResponse, error) {
	candidates, err := l.svcCtx.QueueCapacityProvider().CandidatesForZone(l.ctx, zoneID)
	if err != nil {
		logx.Errorf("[loginqueue] fast-path candidates zone=%d err=%v", zoneID, err)
		return &loginpb.AssignGateResponse{
			Status: uint32(loginqueue.StatusError),
			Error:  "no gate available for requested zone",
		}, nil
	}
	candidates = selectCellAffinityCandidates(l.svcCtx.CellRouter, playerID, candidates)
	if len(candidates) == 0 {
		logx.Errorf("[loginqueue] fast-path zone=%d has zero candidates", zoneID)
		return &loginpb.AssignGateResponse{
			Status: uint32(loginqueue.StatusError),
			Error:  "no gate available for requested zone",
		}, nil
	}
	// 签出的票记进本进程的在途账,本进程后面的挑选会把它算进这台 gate 的负载(loginqueue/gate_inflight.go)。
	admit, err := loginqueue.PickAndSignGateToken(candidates, l.svcCtx.GateInflight(), l.svcCtx.GateTokenSigningSecret(), gateTokenTTL)
	if err != nil {
		logx.Errorf("[loginqueue] fast-path sign zone=%d err=%v", zoneID, err)
		return &loginpb.AssignGateResponse{Status: uint32(loginqueue.StatusError), Error: "sign failed"}, nil
	}
	return &loginpb.AssignGateResponse{
		Status:         uint32(loginqueue.StatusAdmitted),
		Ip:             admit.IP,
		Port:           admit.Port,
		WsUrl:          admit.WsURL,
		TokenPayload:   admit.TokenPayload,
		TokenSignature: admit.TokenSignature,
		TokenDeadline:  admit.TokenDeadline,
	}, nil
}

// selectCellAffinityCandidates prefers gates whose ZoneID matches the player's deterministic CellID
// when CellRouter is enabled, falling back to the full candidate set if none advertise that cell.
func selectCellAffinityCandidates(router *cellroute.Router, playerID uint64, candidates []loginqueue.GateCandidate) []loginqueue.GateCandidate {
	if router == nil || playerID == 0 || len(candidates) == 0 {
		return candidates
	}
	loc, err := router.Route(playerID)
	if err != nil {
		return candidates
	}
	matched := make([]loginqueue.GateCandidate, 0, len(candidates))
	for _, c := range candidates {
		if c.ZoneID == loc.CellID {
			matched = append(matched, c)
		}
	}
	if len(matched) == 0 {
		return candidates
	}
	return matched
}

// reconnectingPlayer 判断账号是不是已经在本区游戏里(重连 / 顶号),是就返回那个角色的 player_id。
//
// 读账号的角色列表与各角色的会话都是尽力而为:读不到就当成新登录去排队,只会少免、不会错免。
func (l *AssignGateLogic) reconnectingPlayer(account string, zoneID uint32) (uint64, bool) {
	if l.svcCtx.AccountDirectory == nil {
		return 0, false
	}
	roles, err := l.svcCtx.AccountDirectory.Read(l.ctx, account, false)
	if err != nil {
		return 0, false
	}
	liveSession := func(playerID uint64) (string, uint32) {
		session, err := sessionmanager.GetSession(l.ctx, l.svcCtx.PlayerLocatorClient, playerID)
		if err != nil || session == nil {
			return "", 0
		}
		return session.GateID, session.GateZoneID
	}
	zoneGates := func() map[string]struct{} {
		candidates, err := l.svcCtx.QueueCapacityProvider().CandidatesForZone(l.ctx, zoneID)
		if err != nil {
			return nil
		}
		ids := make(map[string]struct{}, len(candidates))
		for _, c := range candidates {
			ids[strconv.FormatUint(uint64(c.NodeID), 10)] = struct{}{}
		}
		return ids
	}
	return findReconnectingPlayer(roles.GetPlayers(), zoneID, liveSession, zoneGates)
}

// findReconnectingPlayer 在账号的角色里找一个已经占着本区一条 gate 连接的:它有在线会话(liveSession 返回
// 会话所在 gate 节点号与 gate_zone_id,没有会话返回空串),并且这个会话就在本区。
//
// 账号的每个角色都要看(2026-11-20 起;此前只看第一个角色,多角色账号用别的角色在线时重连会被排队)。
//
// 「会话在不在本区」分两档判定:
//   - 会话带 gate_zone_id(非 0):以 gate_zone_id 为权威,等于本区即免排队、不等即不免,不读 gate 列表。
//     合服并入角色即使原 gate 掉线也能免排队;本区角色跨区到别的区在线也不会误免本区排队。
//   - 旧版会话(gate_zone_id == 0):兼容兜底 —— 建角区是本区直接算;建角区不是本区时,仅当其会话正连在
//     本区现有 gate 上才算(惰性读一次 zoneGates)。
func findReconnectingPlayer(
	roles []*base.AccountSimplePlayer,
	zoneID uint32,
	liveSession func(playerID uint64) (gateID string, gateZoneID uint32),
	zoneGates func() map[string]struct{},
) (uint64, bool) {
	var gates map[string]struct{}
	gatesLoaded := false
	for _, role := range roles {
		playerID := role.GetPlayerId()
		if playerID == 0 {
			continue
		}
		gateID, gateZoneID := liveSession(playerID)
		if gateID == "" {
			continue
		}
		if gateZoneID != 0 {
			if gateZoneID == zoneID {
				return playerID, true
			}
			continue
		}
		if role.GetZoneId() == zoneID {
			return playerID, true
		}
		if !gatesLoaded {
			gates = zoneGates()
			gatesLoaded = true
		}
		if _, onOwnGate := gates[gateID]; onOwnGate {
			return playerID, true
		}
	}
	return 0, false
}
