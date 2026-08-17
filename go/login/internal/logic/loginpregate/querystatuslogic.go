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

	if _, _, err := loginqueue.ParseAndVerifyQueueTokenMulti(l.svcCtx.QueueTokenVerifySecrets(), in.QueueToken); err != nil {
		return &loginpb.QueryQueueStatusResponse{
			Status: uint32(loginqueue.StatusExpired),
			Error:  err.Error(),
		}, nil
	}

	state, err := l.svcCtx.LoginQueue.Lookup(l.ctx, in.QueueToken, func(slot *loginqueue.AdmitSlot) (*loginqueue.AdmitToken, error) {
		// Sign at consume time so the 5-min token TTL begins now (R17 R1
		// fix). The dispatcher only picked the gate; signing here means a
		// client that polled slowly still gets a full-TTL token.
		return loginqueue.SignGateToken(&loginqueue.GateCandidate{
			NodeID: slot.NodeID,
			IP:     slot.IP,
			Port:   slot.Port,
			ZoneID: slot.ZoneID,
		}, l.svcCtx.GateTokenSigningSecret(), gateTokenTTL)
	})
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
