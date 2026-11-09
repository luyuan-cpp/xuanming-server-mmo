package loginpregatelogic

import (
	"context"

	"login/internal/config"
	"login/internal/logic/pkg/loginqueue"
	"login/internal/svc"
	loginpb "proto/login"

	"github.com/zeromicro/go-zero/core/logx"
)

// QueryQueueStatusLogic backs the /api/queue-status poll loop.
//
// Lifecycle of a queue token across this RPC:
//
//	first poll  → token verifies + queueId still in ZSET → QUEUEING (rank, total)
//	dispatcher promotes → admit:{queueId} populated with AdmitSlot (gate pick only)
//	next poll   → consumeAdmitAndSign fires → ADMITTED (gate token signed NOW)
//	subsequent  → admit consumed; token-index TTL still alive but ZSET miss → EXPIRED
//	              (2026-10-15 更正:判据不是 ZSET 落空——那只说明已出队,在途窗口里也落空、回名次 0;
//	              是 meta 有 admitted_at 而放行槽已不在,见 loginqueue.Queue.Poll。放行槽没人来取、
//	              按 AdmitTTL 过期之后再查同样回 EXPIRED。此前代码实际回 QUEUEING、名次 0,与本注释不符)
//	              (2026-10-18 补注:在途窗口(已出队、未写放行)已不存在——出队与写 admitted_at 在同一段放行
//	              脚本里原子完成;排队中的轮询会续空闲 TTL(meta / token 索引 / 队列键 PTTL < QueueEntryTTL
//	              时续到 1.25 倍);不在队列又没放行回 EXPIRED(queue_gone);排队令牌的绝对过期改为 24h
//	              (loginqueue.queueTokenMaxAge)。Poll 的 zone 取自已验签的令牌,见 loginqueue.Queue.Poll。)
//
// The ADMITTED → EXPIRED transition deliberately gives the client exactly
// one chance to read the gate token. If the client drops it (network blip
// between this RPC and the gate TCP connect), they'll see EXPIRED on the
// next poll and the UI should fall back to /api/assign-gate (which is
// idempotent against the queue: enqueue with a fresh queueId).
//
// Signing happens HERE (consume time), not at dispatcher admit time —
// see consumeAdmitAndSign for the R17 R1 fix rationale.
type QueryQueueStatusLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewQueryQueueStatusLogic(ctx context.Context, svcCtx *svc.ServiceContext) *QueryQueueStatusLogic {
	return &QueryQueueStatusLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *QueryQueueStatusLogic) QueryQueueStatus(in *loginpb.QueryQueueStatusRequest) (*loginpb.QueryQueueStatusResponse, error) {
	if l.svcCtx.LoginQueue == nil {
		// Queue disabled — surface as EXPIRED so client falls back to a
		// fresh /assign-gate. Not "error" because the queue path was
		// switched off intentionally; the legacy fast path remains usable.
		loginqueue.RecordExpiredQueueDisabled()
		return &loginpb.QueryQueueStatusResponse{
			Status: uint32(loginqueue.StatusExpired),
			Error:  "queue disabled",
		}, nil
	}

	// zoneID 必须取自已验签的排队令牌:Poll 拿它拼队列键并核对 meta.zone_id(2026-10-18),
	// 不得用常量代替;传 0 会被当成损坏、回 EXPIRED(meta_gone)。
	_, zoneID, err := loginqueue.ParseAndVerifyQueueTokenMulti(l.svcCtx.QueueTokenVerifySecrets(), in.QueueToken)
	if err != nil {
		return &loginpb.QueryQueueStatusResponse{
			Status: uint32(loginqueue.StatusExpired),
			Error:  err.Error(),
		}, nil
	}

	// 取走时现签(R17 R1):dispatcher 只挑了 gate,在这里签,轮询慢的客户端拿到的票也是完整 TTL。
	// 签之前复核放行时选定的 gate,它已停写、满载或不在候选里就重挑(loginqueue.NewAdmitSigner,与 AssignGate 重入同一份)。
	signFn := loginqueue.NewAdmitSigner(l.ctx, l.svcCtx.QueueCapacityProvider(), l.svcCtx.GateTokenSigningSecret(), gateTokenTTL)
	state, err := l.svcCtx.LoginQueue.Poll(l.ctx, in.QueueToken, zoneID, signFn)
	if err != nil {
		// Redis flap or similar — log so on-call can correlate with Redis
		// metrics. We deliberately don't expose the raw error to the client
		// (could leak Redis topology) — generic message is enough.
		logx.Errorf("[loginqueue] QueryQueueStatus lookup err=%v", err)
		return &loginpb.QueryQueueStatusResponse{
			Status: uint32(loginqueue.StatusError),
			Error:  "queue lookup failed",
		}, nil
	}

	resp := &loginpb.QueryQueueStatusResponse{Status: uint32(state.Status)}
	switch state.Status {
	case loginqueue.StatusAdmitted:
		resp.Ip = state.Admit.IP
		resp.Port = state.Admit.Port
		resp.TokenPayload = state.Admit.TokenPayload
		resp.TokenSignature = state.Admit.TokenSignature
		resp.TokenDeadline = state.Admit.TokenDeadline
	case loginqueue.StatusQueueing:
		resp.QueueRank = state.Rank
		resp.QueueTotal = state.Total
		resp.RetryAfterMs = config.AppConfig.Queue.DefaultRetryAfterMs
	}
	return resp, nil
}
