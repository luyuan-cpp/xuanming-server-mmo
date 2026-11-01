package server

import (
	"context"
	"time"

	"player_inbox/internal/inbox"
	"player_inbox/internal/logic"

	inboxpb "proto/player_inbox"

	"github.com/zeromicro/go-zero/core/logx"
)

// AdminStore 是运维面需要的存储能力(*inbox.Store 实现它)。
type AdminStore interface {
	Void(ctx context.Context, p inbox.VoidParams, ledgers inbox.LedgerReader) (*inboxpb.VoidPlayerInboxItemResponse, error)
	Summary(ctx context.Context, playerID uint64) (*inboxpb.GetPlayerInboxSummaryResponse, error)
	List(ctx context.Context, req *inboxpb.ListPlayerInboxItemsRequest) (*inboxpb.ListPlayerInboxItemsResponse, error)
	Lookup(ctx context.Context, req *inboxpb.LookupPlayerInboxRequest) (*inboxpb.LookupPlayerInboxResponse, error)
	ReconcilePlayer(ctx context.Context, req *inboxpb.ReconcilePlayerInboxRequest, ledgers inbox.LedgerReader) (*inboxpb.ReconcilePlayerInboxResponse, error)
}

// AdminServer 实现 inboxpb.PlayerInboxAdminServer。口令由 AdminAuthInterceptor 在进入这里之前校验。
type AdminServer struct {
	inboxpb.UnimplementedPlayerInboxAdminServer
	store    AdminStore
	sanction logic.GmSanctionDeps
	ledgers  inbox.LedgerReader
	budget   time.Duration
}

// NewAdminServer。sanction 是 GM 处置的依赖;ledgers 是读已落盘账本的适配器(对账与 VoidItem 强制模式共用)。
func NewAdminServer(store AdminStore, sanction logic.GmSanctionDeps, ledgers inbox.LedgerReader, budget time.Duration) *AdminServer {
	return &AdminServer{store: store, sanction: sanction, ledgers: ledgers, budget: budget}
}

// GmSanctionPlayer GM 处置入箱(幂等,设计 §6.1)。
func (s *AdminServer) GmSanctionPlayer(ctx context.Context, in *inboxpb.GmSanctionPlayerRequest) (*inboxpb.GmSanctionPlayerResponse, error) {
	ctx, cancel := withBudget(ctx, s.budget)
	defer cancel()
	return logic.GmSanctionPlayer(ctx, s.sanction, in, inbox.PeerAddr(ctx)), nil
}

// GetSummary 按玩家列出各域流的概况与 max_fence_epoch。
func (s *AdminServer) GetSummary(ctx context.Context, in *inboxpb.GetPlayerInboxSummaryRequest) (*inboxpb.GetPlayerInboxSummaryResponse, error) {
	ctx, cancel := withBudget(ctx, s.budget)
	defer cancel()
	resp, err := s.store.Summary(ctx, in.GetPlayerId())
	if err != nil {
		logx.WithContext(ctx).Errorf("[player_inbox] GetSummary failed player=%d %s: %v", in.GetPlayerId(), callerIdentity(ctx), err)
		return &inboxpb.GetPlayerInboxSummaryResponse{ErrorMessage: storeUnavailable(), PlayerId: in.GetPlayerId()}, nil
	}
	return resp, nil
}

// ListItems 按 player_id + domain + state 分页列条目。
func (s *AdminServer) ListItems(ctx context.Context, in *inboxpb.ListPlayerInboxItemsRequest) (*inboxpb.ListPlayerInboxItemsResponse, error) {
	ctx, cancel := withBudget(ctx, s.budget)
	defer cancel()
	resp, err := s.store.List(ctx, in)
	if err != nil {
		logx.WithContext(ctx).Errorf("[player_inbox] ListItems failed player=%d domain=%d %s: %v",
			in.GetPlayerId(), in.GetDomain(), callerIdentity(ctx), err)
		return &inboxpb.ListPlayerInboxItemsResponse{ErrorMessage: storeUnavailable()}, nil
	}
	return resp, nil
}

// LookupByRequest 按 (player_id, producer_kind, request_id) 查条目与结局。
func (s *AdminServer) LookupByRequest(ctx context.Context, in *inboxpb.LookupPlayerInboxRequest) (*inboxpb.LookupPlayerInboxResponse, error) {
	ctx, cancel := withBudget(ctx, s.budget)
	defer cancel()
	resp, err := s.store.Lookup(ctx, in)
	if err != nil {
		logx.WithContext(ctx).Errorf("[player_inbox] LookupByRequest failed player=%d request_id=%d %s: %v",
			in.GetPlayerId(), in.GetRequestId(), callerIdentity(ctx), err)
		return &inboxpb.LookupPlayerInboxResponse{ErrorMessage: storeUnavailable()}, nil
	}
	return resp, nil
}

// VoidItem 未投递 → VOIDED;已投递 → 只挂 VOID 指令(IB-5)。同事务写审计。
// 强制模式(mode=FORCE_MISMATCH)经已落盘账本证明后终结已投递的行,账本读取器与 ReconcilePlayer 共用装配层那一个;
// 读账本失败与库故障一样 in-band 回 kPlayerInboxStoreUnavailable(可重试)。
func (s *AdminServer) VoidItem(ctx context.Context, in *inboxpb.VoidPlayerInboxItemRequest) (*inboxpb.VoidPlayerInboxItemResponse, error) {
	ctx, cancel := withBudget(ctx, s.budget)
	defer cancel()
	resp, err := s.store.Void(ctx, inbox.VoidParams{
		PlayerID: in.GetPlayerId(),
		Domain:   in.GetDomain(),
		Seq:      in.GetSeq(),
		Reason:   in.GetReason(),
		Operator: in.GetOperator(),
		Note:     in.GetNote(),
		PeerAddr: inbox.PeerAddr(ctx),
		Mode:     in.GetMode(),
	}, s.ledgers)
	if tip := roleNotVerified(err); tip != nil {
		logx.WithContext(ctx).Infof("[player_inbox] VoidItem role not verified player=%d domain=%d seq=%d mode=%s operator=%q %s: %v",
			in.GetPlayerId(), in.GetDomain(), in.GetSeq(), in.GetMode(), in.GetOperator(), callerIdentity(ctx), err)
		return &inboxpb.VoidPlayerInboxItemResponse{ErrorMessage: tip}, nil
	}
	if err != nil {
		logx.WithContext(ctx).Errorf("[player_inbox] VoidItem failed player=%d domain=%d seq=%d mode=%s operator=%q %s: %v",
			in.GetPlayerId(), in.GetDomain(), in.GetSeq(), in.GetMode(), in.GetOperator(), callerIdentity(ctx), err)
		return &inboxpb.VoidPlayerInboxItemResponse{ErrorMessage: storeUnavailable()}, nil
	}
	return resp, nil
}

// ReconcilePlayer 单个玩家比对收件箱与已落盘账本(设计 §9.3)。只读。
func (s *AdminServer) ReconcilePlayer(ctx context.Context, in *inboxpb.ReconcilePlayerInboxRequest) (*inboxpb.ReconcilePlayerInboxResponse, error) {
	ctx, cancel := withBudget(ctx, s.budget)
	defer cancel()
	resp, err := s.store.ReconcilePlayer(ctx, in, s.ledgers)
	if err != nil {
		logx.WithContext(ctx).Errorf("[player_inbox] ReconcilePlayer failed player=%d domain=%d %s: %v",
			in.GetPlayerId(), in.GetDomain(), callerIdentity(ctx), err)
		return &inboxpb.ReconcilePlayerInboxResponse{ErrorMessage: storeUnavailable()}, nil
	}
	return resp, nil
}
