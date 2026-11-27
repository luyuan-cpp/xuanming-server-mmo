package server

import (
	"context"
	"errors"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	base "proto/common/base"
	dspb "proto/data_service"
	rankpb "proto/rank"
	"rank/internal/data"
	"rank/internal/metrics"
	"shared/domainhost"
	"shared/dualcontrol"
	table "shared/generated/pb/table"
	"shared/serviceauth"
)

// adminCaller 是后台(java/gm_admin)的调用方名(rank.yaml 的 Producers.admin)。只有它的删除要双人复核批准记录;
// match 摘已删角色是自动路径,以 match 调 RemoveRankEntry,不带批准记录,一刀切会让清理停摆。
const adminCaller = data.AdminCaller

type Server struct {
	rankpb.UnimplementedClientPlayerRankServer
	rankpb.UnimplementedRankAdminServer
	repo  *data.Repo
	auth  *serviceauth.Verifier
	names NameReader
	// allowUnapproved:配置 AdminAllowUnapproved。缺省 false,调用方 admin 的删除必须带双人复核批准记录。
	allowUnapproved bool
}

type NameReader interface {
	BatchGetPlayerName(context.Context, *dspb.BatchGetPlayerNameRequest, ...grpc.CallOption) (*dspb.BatchGetPlayerNameResponse, error)
}

// New 装配服务端;allowUnapproved 是配置 AdminAllowUnapproved,打开时启动打一行 ERROR。
func New(repo *data.Repo, auth *serviceauth.Verifier, names NameReader, allowUnapproved bool) *Server {
	if allowUnapproved {
		logx.Errorf("[RankAdmin] AdminAllowUnapproved is on: removals by caller %s without a dual-control approval record are accepted; keep it off outside local development", adminCaller)
	}
	return &Server{repo: repo, auth: auth, names: names, allowUnapproved: allowUnapproved}
}
func (s *Server) enrich(ctx context.Context, entries []*rankpb.RankEntry, self *rankpb.RankEntry) error {
	if s.names == nil {
		return errors.New("排行榜权威名称服务未装配")
	}
	seen := map[uint64]bool{}
	request := &dspb.BatchGetPlayerNameRequest{}
	all := append(append([]*rankpb.RankEntry(nil), entries...), self)
	for _, entry := range all {
		if entry != nil && !seen[entry.EntityId] {
			seen[entry.EntityId] = true
			request.PlayerIds = append(request.PlayerIds, entry.EntityId)
		}
	}
	if len(request.PlayerIds) == 0 {
		return nil
	}
	bounded, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	names, err := s.names.BatchGetPlayerName(bounded, request)
	if err != nil {
		return err
	}
	for _, entry := range all {
		if entry != nil {
			entry.Name = names.GetNames()[entry.EntityId]
		}
	}
	return nil
}
func rejection(err error) *base.TipInfoMessage {
	if err == nil {
		return nil
	}
	code := uint32(table.CommonError_kServiceUnavailable)
	switch {
	case errors.Is(err, data.ErrInvalid):
		code = uint32(table.CommonError_kInvalidParameter)
	case errors.Is(err, data.ErrNotFound):
		code = uint32(table.RankError_kRankBoardNotFound)
	case errors.Is(err, data.ErrClosed):
		code = uint32(table.RankError_kRankGenerationClosed)
	case errors.Is(err, data.ErrConflict):
		code = uint32(table.RankError_kRankRequestConflict)
	case errors.Is(err, data.ErrCursor):
		code = uint32(table.RankError_kRankSettlementPending)
	case errors.Is(err, data.ErrDenied):
		code = uint32(table.CommonError_kInvalidParameter)
	}
	return &base.TipInfoMessage{Id: code}
}
func (s *Server) caller(ctx context.Context, method string, request proto.Message) (string, error) {
	caller, err := s.auth.Verify(ctx, method, request)
	if err != nil {
		return "", status.Error(codes.Unauthenticated, "内部调用无权访问")
	}
	return caller, nil
}
func (s *Server) GetRankPage(ctx context.Context, in *rankpb.GetRankPageRequest) (*rankpb.GetRankPageResponse, error) {
	player, ok := domainhost.Player(ctx)
	if !ok {
		return &rankpb.GetRankPageResponse{ErrorMessage: rejection(data.ErrInvalid)}, nil
	}
	// 新的通用榜公开读面只开放全服榜；其它scope不能靠客户端伪造scope_id越权。
	if in.GetBoard().GetScope() != rankpb.RankScope_RANK_SCOPE_GLOBAL {
		return &rankpb.GetRankPageResponse{ErrorMessage: rejection(data.ErrDenied)}, nil
	}
	result, err := s.repo.Page(ctx, player, in)
	if err != nil {
		return &rankpb.GetRankPageResponse{ErrorMessage: rejection(err)}, nil
	}
	if err = s.enrich(ctx, result.Entries, result.Self); err != nil {
		return &rankpb.GetRankPageResponse{ErrorMessage: rejection(err)}, nil
	}
	return result, nil
}
func (s *Server) GetRankAround(ctx context.Context, in *rankpb.GetRankAroundRequest) (*rankpb.GetRankAroundResponse, error) {
	player, ok := domainhost.Player(ctx)
	if !ok || in.GetBoard().GetScope() != rankpb.RankScope_RANK_SCOPE_GLOBAL {
		return &rankpb.GetRankAroundResponse{ErrorMessage: rejection(data.ErrInvalid)}, nil
	}
	result, err := s.repo.Around(ctx, player, in)
	if err != nil {
		return &rankpb.GetRankAroundResponse{ErrorMessage: rejection(err)}, nil
	}
	if err = s.enrich(ctx, result.Entries, result.Self); err != nil {
		return &rankpb.GetRankAroundResponse{ErrorMessage: rejection(err)}, nil
	}
	return result, nil
}
func (s *Server) OpenRankGeneration(ctx context.Context, in *rankpb.OpenRankGenerationRequest) (*rankpb.OpenRankGenerationResponse, error) {
	caller, err := s.caller(ctx, rankpb.RankAdmin_OpenRankGeneration_FullMethodName, in)
	if err != nil {
		return nil, err
	}
	result, err := s.repo.Open(ctx, caller, in)
	if err != nil {
		return &rankpb.OpenRankGenerationResponse{ErrorMessage: rejection(err)}, nil
	}
	return result, nil
}
func (s *Server) SubmitRankScores(ctx context.Context, in *rankpb.SubmitRankScoresRequest) (*rankpb.SubmitRankScoresResponse, error) {
	caller, err := s.caller(ctx, rankpb.RankAdmin_SubmitRankScores_FullMethodName, in)
	if err != nil {
		return nil, err
	}
	result, err := s.repo.Submit(ctx, caller, in)
	if err != nil {
		return &rankpb.SubmitRankScoresResponse{ErrorMessage: rejection(err)}, nil
	}
	return result, nil
}
func (s *Server) FreezeRankGeneration(ctx context.Context, in *rankpb.FreezeRankGenerationRequest) (*rankpb.FreezeRankGenerationResponse, error) {
	caller, err := s.caller(ctx, rankpb.RankAdmin_FreezeRankGeneration_FullMethodName, in)
	if err != nil {
		return nil, err
	}
	result, err := s.repo.Freeze(ctx, caller, in)
	if err != nil {
		return &rankpb.FreezeRankGenerationResponse{ErrorMessage: rejection(err)}, nil
	}
	return result, nil
}
func (s *Server) RemoveRankEntry(ctx context.Context, in *rankpb.RemoveRankEntryRequest) (*rankpb.RankAdminResponse, error) {
	return s.remove(ctx, rankpb.RankAdmin_RemoveRankEntry_FullMethodName, metrics.AdminMethodRemoveEntry, in, false)
}
func (s *Server) DeleteRankGeneration(ctx context.Context, in *rankpb.RemoveRankEntryRequest) (*rankpb.RankAdminResponse, error) {
	return s.remove(ctx, rankpb.RankAdmin_DeleteRankGeneration_FullMethodName, metrics.AdminMethodDeleteGeneration, in, true)
}

// remove 是移除成员与隐藏整代共用的入口:验调用方 → 核双人复核批准记录 → 交给 repo 判重与写入 → 写一行审计日志。
// 批准记录的核对在判重之前:不合格的请求不碰库,也就占不了 (board, generation, request_id) 这个判重键。
func (s *Server) remove(ctx context.Context, fullMethod, method string, in *rankpb.RemoveRankEntryRequest, hide bool) (*rankpb.RankAdminResponse, error) {
	caller, err := s.caller(ctx, fullMethod, in)
	if err != nil {
		return nil, err
	}
	if err = s.admitApproval(ctx, method, caller, in); err != nil {
		return nil, err
	}
	tip := rejection(s.repo.Remove(ctx, caller, in, hide))
	// tip 为 0 是写成或同一请求的重放;数据层的错误文本不进日志,只记 tip。
	logx.WithContext(ctx).Infof("[RankAdmin] %s caller=%s board_id=%d generation=%d entity_id=%d request_id=%d approval_id=%q proposer=%q approver=%q tip=%d",
		method, caller, in.GetBoardId(), in.GetGeneration(), in.GetEntityId(), in.GetRequestId(),
		in.GetApproval().GetApprovalId(), in.GetApproval().GetProposer(), in.GetApproval().GetApprover(), tip.GetId())
	return &rankpb.RankAdminResponse{ErrorMessage: tip}, nil
}

// admitApproval 核对后台双人复核的批准记录(docs/design/admin-identity-and-dual-control.md §3 甲级、§5.5):
// 调用方 admin 必须带,而且 request_id 就是批准号(按 (board, generation, request_id) 判重,一次批准只执行一次);请求里没有操作人字段,
// 不比发起人。其它调用方(match 摘已删角色)不要求,带了照样核。不带记录的 admin 删除只在部署显式打开 AdminAllowUnapproved
// (本机开发)时接受,每次打 ERROR 并计数。拒绝回 PermissionDenied(确定的拒绝),文本与 trade 的带内原因码相同。
func (s *Server) admitApproval(ctx context.Context, method, caller string, in *rankpb.RemoveRankEntryRequest) error {
	approval := in.GetApproval()
	if approval == nil {
		if caller != adminCaller {
			return nil
		}
		if !s.allowUnapproved {
			logx.WithContext(ctx).Errorf("[RankAdmin] %s refused: dual-control approval record is missing caller=%s board_id=%d generation=%d request_id=%d",
				method, caller, in.GetBoardId(), in.GetGeneration(), in.GetRequestId())
			return status.Error(codes.PermissionDenied, "dual_control_approval_required")
		}
		metrics.ObserveAdminUnapproved(method)
		logx.WithContext(ctx).Errorf("[RankAdmin] %s accepted without a dual-control approval record caller=%s board_id=%d generation=%d request_id=%d (AdminAllowUnapproved is on)",
			method, caller, in.GetBoardId(), in.GetGeneration(), in.GetRequestId())
		return nil
	}
	// 删除请求没有操作人字段:显式传发起人,两人记在批准记录里。
	if err := dualcontrol.RequireForRequest(approval, approval.GetProposer(), in.GetRequestId()); err != nil {
		logx.WithContext(ctx).Errorf("[RankAdmin] %s refused: %v caller=%s board_id=%d generation=%d request_id=%d",
			method, err, caller, in.GetBoardId(), in.GetGeneration(), in.GetRequestId())
		return status.Error(codes.PermissionDenied, "dual_control_approval_invalid: "+err.Error())
	}
	return nil
}
