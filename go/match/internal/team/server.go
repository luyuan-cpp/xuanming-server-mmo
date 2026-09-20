package team

import (
	"context"
	"time"

	"match/internal/metrics"
	"match/internal/pkg/ctxkeys"

	base "proto/common/base"
	teampb "proto/team"

	"shared/serverbase"
)

// ClientPlayerTeam 的 gRPC 入口(设计文档 docs/design/team-system.md §A.3、§D.2)。
//
// 与 match 其余 service 共用同一个 zrpc server 与 sessionInterceptor(match_service.go),
// 本层只做三件事:自设请求预算、从 session 取身份(fail-closed)、记 team_rpc_total;业务全部委托 Service。

// teamRPCBudget 每个方法入口自设的请求预算(J-14 方案 a):赶在路由服 ForwardTimeoutMs=5000 之前把
// in-band 结果回去,且不改 match 的 zrpc Timeout(5000,改小会截断 WatchBattle 同步链)。
// go-zero 超时拦截器到期后 handler 仍会跑完,迟到写入由 expected_team_id 绑定与提交前 ctx 检查兜住(§D.6)。
const teamRPCBudget = 3500 * time.Millisecond

// RPC 名:team_rpc_total{method} 与 team_commit_retry_total{op} 的固定 label 值。
const (
	methodCreateTeam        = "CreateTeam"
	methodGetMyTeam         = "GetMyTeam"
	methodApplyJoinTeam     = "ApplyJoinTeam"
	methodHandleApplication = "HandleApplication"
	methodInviteToTeam      = "InviteToTeam"
	methodRespondInvite     = "RespondInvite"
	methodListMyInvites     = "ListMyInvites"
	methodLeaveTeam         = "LeaveTeam"
	methodKickMember        = "KickMember"
	methodTransferLeader    = "TransferLeader"
	methodDisbandTeam       = "DisbandTeam"
	methodStartTeamMatch    = "StartTeamMatch"
)

// team_rpc_total{outcome} 的固定取值。
const (
	rpcOutcomeOK        = "ok"
	rpcOutcomeRejected  = "rejected"
	rpcOutcomeInternal  = "internal"
	rpcOutcomeUnknown   = "unknown_code" // 码不在本二进制的码表里(码表漂移)
	rpcOutcomeNoSession = "no_session"
)

// Server 实现 teampb.ClientPlayerTeamServer。该 service 标了 OptionIsClientProtocolService=true:
// 只接客户端经 gate → 路由服转发来的请求,东西向调用不许加到这个 service 上。
type Server struct {
	teampb.UnimplementedClientPlayerTeamServer
	service *Service
}

// NewServer match_service.go 装配:teampb.RegisterClientPlayerTeamServer(grpcServer, team.NewServer(svc))。
func NewServer(service *Service) *Server {
	return &Server{service: service}
}

// callerOf 从 sessionInterceptor 放进 ctx 的 SessionDetails 取调用者。
// 缺 session 或 player_id=0 一律拒绝:team 请求体里没有 player_id,也就没有"退回请求体"的路径(§D.2)。
func callerOf(ctx context.Context) (uint64, bool) {
	detail, ok := ctxkeys.GetSessionDetails(ctx)
	if !ok || detail.GetPlayerId() == 0 {
		return 0, false
	}
	return detail.GetPlayerId(), true
}

// rpcOutcome 按生成的 tip 码表给回包定性(故障分类来自 Tip.xlsx 的 fault 列,不在这里手写集合)。
func rpcOutcome(code uint32) string {
	if code == 0 {
		return rpcOutcomeOK
	}
	switch serverbase.TipVerdict(code) {
	case serverbase.VerdictOK:
		return rpcOutcomeOK
	case serverbase.VerdictFault:
		return rpcOutcomeInternal
	case serverbase.VerdictBizReject:
		return rpcOutcomeRejected
	}
	return rpcOutcomeUnknown
}

// serveTeam 返回 TeamResponse 的方法的统一外壳。
func serveTeam[Req any](ctx context.Context, method string, in Req,
	call func(context.Context, uint64, Req) *teampb.TeamResponse,
) (*teampb.TeamResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, teamRPCBudget)
	defer cancel()
	caller, ok := callerOf(ctx)
	if !ok {
		metrics.ObserveTeamRPC(method, rpcOutcomeNoSession)
		return &teampb.TeamResponse{ErrorMessage: tipOf(ErrPlayerId, 0)}, nil
	}
	resp := call(ctx, caller, in)
	metrics.ObserveTeamRPC(method, rpcOutcome(resp.GetErrorMessage().GetId()))
	return resp, nil
}

func (s *Server) CreateTeam(ctx context.Context, in *teampb.CreateTeamRequest) (*teampb.TeamResponse, error) {
	return serveTeam(ctx, methodCreateTeam, in, s.service.CreateTeam)
}

func (s *Server) GetMyTeam(ctx context.Context, in *teampb.GetMyTeamRequest) (*teampb.TeamResponse, error) {
	return serveTeam(ctx, methodGetMyTeam, in, s.service.GetMyTeam)
}

func (s *Server) ApplyJoinTeam(ctx context.Context, in *teampb.ApplyJoinTeamRequest) (*teampb.TeamResponse, error) {
	return serveTeam(ctx, methodApplyJoinTeam, in, s.service.ApplyJoinTeam)
}

func (s *Server) HandleApplication(ctx context.Context, in *teampb.HandleApplicationRequest) (*teampb.TeamResponse, error) {
	return serveTeam(ctx, methodHandleApplication, in, s.service.HandleApplication)
}

func (s *Server) InviteToTeam(ctx context.Context, in *teampb.InviteToTeamRequest) (*teampb.TeamResponse, error) {
	return serveTeam(ctx, methodInviteToTeam, in, s.service.InviteToTeam)
}

func (s *Server) RespondInvite(ctx context.Context, in *teampb.RespondInviteRequest) (*teampb.TeamResponse, error) {
	return serveTeam(ctx, methodRespondInvite, in, s.service.RespondInvite)
}

func (s *Server) LeaveTeam(ctx context.Context, in *teampb.LeaveTeamRequest) (*teampb.TeamResponse, error) {
	return serveTeam(ctx, methodLeaveTeam, in, s.service.LeaveTeam)
}

func (s *Server) KickMember(ctx context.Context, in *teampb.KickMemberRequest) (*teampb.TeamResponse, error) {
	return serveTeam(ctx, methodKickMember, in, s.service.KickMember)
}

func (s *Server) TransferLeader(ctx context.Context, in *teampb.TransferLeaderRequest) (*teampb.TeamResponse, error) {
	return serveTeam(ctx, methodTransferLeader, in, s.service.TransferLeader)
}

func (s *Server) DisbandTeam(ctx context.Context, in *teampb.DisbandTeamRequest) (*teampb.TeamResponse, error) {
	return serveTeam(ctx, methodDisbandTeam, in, s.service.DisbandTeam)
}

func (s *Server) StartTeamMatch(ctx context.Context, in *teampb.StartTeamMatchRequest) (*teampb.TeamResponse, error) {
	return serveTeam(ctx, methodStartTeamMatch, in, s.service.StartTeamMatch)
}

// ListMyInvites 回包类型不同,单独写外壳(语义与 serveTeam 相同)。
func (s *Server) ListMyInvites(ctx context.Context, in *teampb.ListMyInvitesRequest) (*teampb.ListMyInvitesResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, teamRPCBudget)
	defer cancel()
	caller, ok := callerOf(ctx)
	if !ok {
		metrics.ObserveTeamRPC(methodListMyInvites, rpcOutcomeNoSession)
		return &teampb.ListMyInvitesResponse{ErrorMessage: tipOf(ErrPlayerId, 0)}, nil
	}
	resp := s.service.ListMyInvites(ctx, caller, in)
	metrics.ObserveTeamRPC(methodListMyInvites, rpcOutcome(resp.GetErrorMessage().GetId()))
	return resp, nil
}

// NotifyTeamSnapshot / NotifyTeamInvite / NotifyTeamEvent:S2C 推送借 rpc 声明拿消息号的占位
// (实际下行走 Kafka gate PushToPlayerEvent)。客户端误调时直接返回 Empty,不读不写、不打指标(§D.2)。
func (s *Server) NotifyTeamSnapshot(context.Context, *teampb.TeamSnapshotS2C) (*base.Empty, error) {
	return &base.Empty{}, nil
}

func (s *Server) NotifyTeamInvite(context.Context, *teampb.TeamInviteS2C) (*base.Empty, error) {
	return &base.Empty{}, nil
}

func (s *Server) NotifyTeamEvent(context.Context, *teampb.TeamEventS2C) (*base.Empty, error) {
	return &base.Empty{}, nil
}
