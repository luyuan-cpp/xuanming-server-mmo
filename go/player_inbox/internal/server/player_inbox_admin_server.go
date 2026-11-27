package server

import (
	"context"
	"errors"
	"time"

	"player_inbox/internal/inbox"
	"player_inbox/internal/logic"
	"player_inbox/internal/metrics"

	base "proto/common/base"
	inboxpb "proto/player_inbox"
	"shared/dualcontrol"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// AdminStore 是运维面需要的存储能力(*inbox.Store 实现它)。
type AdminStore interface {
	Void(ctx context.Context, p inbox.VoidParams, ledgers inbox.LedgerReader) (*inboxpb.VoidPlayerInboxItemResponse, error)
	Summary(ctx context.Context, playerID uint64) (*inboxpb.GetPlayerInboxSummaryResponse, error)
	List(ctx context.Context, req *inboxpb.ListPlayerInboxItemsRequest) (*inboxpb.ListPlayerInboxItemsResponse, error)
	Lookup(ctx context.Context, req *inboxpb.LookupPlayerInboxRequest) (*inboxpb.LookupPlayerInboxResponse, error)
	ReconcilePlayer(ctx context.Context, req *inboxpb.ReconcilePlayerInboxRequest, ledgers inbox.LedgerReader) (*inboxpb.ReconcilePlayerInboxResponse, error)
	ItemGainBlocks(ctx context.Context, playerID uint64, blocks inbox.ItemGainBlockReader) (*inboxpb.GetPlayerItemGainBlocksResponse, error)
}

// AdminServer 实现 inboxpb.PlayerInboxAdminServer。口令由 AdminAuthInterceptor 在进入这里之前校验;
// 两个甲级写方法(GmSanctionPlayer、VoidItem)在这里再核双人复核批准记录(admitApproval)。
type AdminServer struct {
	inboxpb.UnimplementedPlayerInboxAdminServer
	store    AdminStore
	sanction logic.GmSanctionDeps
	ledgers  inbox.LedgerReader
	blocks   inbox.ItemGainBlockReader
	budget   time.Duration
	// allowUnapproved:PlayerInbox.AdminAllowUnapproved。缺省 false,两个甲级写方法必须带合格的批准记录。
	allowUnapproved bool
}

// NewAdminServer。sanction 是 GM 处置的依赖;ledgers 是读已落盘账本的适配器(对账与 VoidItem 强制模式共用);
// blocks 是读已落盘物品获取封禁表的适配器(GetItemGainBlocks);allowUnapproved 是 PlayerInbox.AdminAllowUnapproved。
func NewAdminServer(store AdminStore, sanction logic.GmSanctionDeps, ledgers inbox.LedgerReader, blocks inbox.ItemGainBlockReader,
	budget time.Duration, allowUnapproved bool) *AdminServer {
	if allowUnapproved {
		logx.Errorf("[player_inbox] PlayerInbox.AdminAllowUnapproved is on: GmSanctionPlayer and VoidItem without a dual-control approval record are accepted; keep it off outside local development")
	}
	return &AdminServer{store: store, sanction: sanction, ledgers: ledgers, blocks: blocks, budget: budget, allowUnapproved: allowUnapproved}
}

// admitApproval 把 shared/dualcontrol 的核对结论落成准入(docs/design/admin-identity-and-dual-control.md §5.5、§5.7),
// 在口令拦截器之后、任何读库写库与判重之前做:
//   - verdict 为 nil:放行,打一行 INFO 记下批准号与批准人(同一条目重复作废时审计行只记第一次,这一行每次都有);
//   - 不带记录(ErrMissing):只在 AdminAllowUnapproved 打开时放行,每一次都打 ERROR 并计数;
//   - 其余不合格一律 PermissionDenied,开关开着也一样。文本只点出哪一项不合格,不回显请求内容。
func (s *AdminServer) admitApproval(ctx context.Context, method, operator string, approval *base.AdminApproval, verdict error) error {
	log := logx.WithContext(ctx)
	switch {
	case verdict == nil:
		log.Infof("[player_inbox] %s dual-control approval accepted approval_id=%q approver=%q operator=%q %s",
			method, approval.GetApprovalId(), approval.GetApprover(), operator, callerIdentity(ctx))
		return nil
	case errors.Is(verdict, dualcontrol.ErrMissing) && s.allowUnapproved:
		metrics.ObserveAdminUnapprovedCommand(method)
		log.Errorf("[player_inbox] %s accepted without a dual-control approval record operator=%q (PlayerInbox.AdminAllowUnapproved is on) %s",
			method, operator, callerIdentity(ctx))
		return nil
	}
	log.Errorf("[player_inbox] Rejecting %s: %v %s", method, verdict, callerIdentity(ctx))
	return status.Errorf(codes.PermissionDenied, "%s: %v", method, verdict)
}

// GmSanctionPlayer GM 处置入箱(幂等,设计 §6.1)。批准号必须就是 request_id:一次批准只入箱一次,由同一个判重键保证。
func (s *AdminServer) GmSanctionPlayer(ctx context.Context, in *inboxpb.GmSanctionPlayerRequest) (*inboxpb.GmSanctionPlayerResponse, error) {
	verdict := dualcontrol.RequireForRequest(in.GetApproval(), in.GetOperator(), in.GetRequestId())
	if err := s.admitApproval(ctx, metrics.AdminMethodGmSanctionPlayer, in.GetOperator(), in.GetApproval(), verdict); err != nil {
		return nil, err
	}
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
// 两种模式都要双人复核批准记录;本接口没有请求号,批准号与批准人只进审计行。
func (s *AdminServer) VoidItem(ctx context.Context, in *inboxpb.VoidPlayerInboxItemRequest) (*inboxpb.VoidPlayerInboxItemResponse, error) {
	verdict := dualcontrol.Require(in.GetApproval(), in.GetOperator())
	if err := s.admitApproval(ctx, metrics.AdminMethodVoidItem, in.GetOperator(), in.GetApproval(), verdict); err != nil {
		return nil, err
	}
	ctx, cancel := withBudget(ctx, s.budget)
	defer cancel()
	resp, err := s.store.Void(ctx, inbox.VoidParams{
		PlayerID:   in.GetPlayerId(),
		Domain:     in.GetDomain(),
		Seq:        in.GetSeq(),
		Reason:     in.GetReason(),
		Operator:   in.GetOperator(),
		Note:       in.GetNote(),
		PeerAddr:   inbox.PeerAddr(ctx),
		Mode:       in.GetMode(),
		ApprovalID: in.GetApproval().GetApprovalId(),
		Approver:   in.GetApproval().GetApprover(),
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

// GetItemGainBlocks 查一名玩家的单玩家物品获取封禁:已落盘的本人封禁表,加上还没生效的封禁 / 解封单(设计 §2.2)。只读。
// 读收件箱库或已落盘存档失败都 in-band 回 kPlayerInboxStoreUnavailable(可重试),gRPC 层不报错。
func (s *AdminServer) GetItemGainBlocks(ctx context.Context, in *inboxpb.GetPlayerItemGainBlocksRequest) (*inboxpb.GetPlayerItemGainBlocksResponse, error) {
	ctx, cancel := withBudget(ctx, s.budget)
	defer cancel()
	resp, err := s.store.ItemGainBlocks(ctx, in.GetPlayerId(), s.blocks)
	if err != nil {
		logx.WithContext(ctx).Errorf("[player_inbox] GetItemGainBlocks failed player=%d %s: %v",
			in.GetPlayerId(), callerIdentity(ctx), err)
		return &inboxpb.GetPlayerItemGainBlocksResponse{ErrorMessage: storeUnavailable(), PlayerId: in.GetPlayerId()}, nil
	}
	return resp, nil
}
