package server

import (
	"context"

	"match/internal/logic"
	"match/internal/svc"

	matchpb "proto/match"
)

// MatchInternalServer 是 match 内部服务 MatchInternal 的 gRPC 入口薄包装(照 MatchServiceServer):
// 每个 RPC 委托对应 logic,不写业务。设计见 docs/design/guild-phase2/06-activities.md §6.18.3。
//
// 活动开局的合法调用方是 go/guild(帮会同道历练全员同意后确认开战),SetSpectateFeatured 的合法调用方是运营工具
// (serviceauth 调用方 ops,见 logic/setspectatefeaturedlogic.go)。客户端来源(带会话 metadata)的调用
// 在 match_service.go 的 sessionInterceptor 就被 PermissionDenied,logic 入口再防一道;
// v1 另靠部署层 NetworkPolicy 收口(契约偏差 13)。
type MatchInternalServer struct {
	svcCtx *svc.ServiceContext
	matchpb.UnimplementedMatchInternalServer
}

func NewMatchInternalServer(svcCtx *svc.ServiceContext) *MatchInternalServer {
	return &MatchInternalServer{
		svcCtx: svcCtx,
	}
}

// StartActivityBattle 活动开局:同步返回 battle_id,gather 异步进行。
func (s *MatchInternalServer) StartActivityBattle(ctx context.Context, in *matchpb.StartActivityBattleRequest) (*matchpb.StartActivityBattleResponse, error) {
	l := logic.NewStartActivityBattleLogic(ctx, s.svcCtx)
	return l.StartActivityBattle(in)
}

func (s *MatchInternalServer) CancelActivityBattle(ctx context.Context, in *matchpb.CancelActivityBattleRequest) (*matchpb.CancelActivityBattleResponse, error) {
	return logic.NewCancelActivityBattleLogic(ctx, s.svcCtx).CancelActivityBattle(in)
}

func (s *MatchInternalServer) CommitBattleOutcome(ctx context.Context, in *matchpb.CommitBattleOutcomeRequest) (*matchpb.CommitBattleOutcomeResponse, error) {
	return logic.CommitBattleOutcome(ctx, s.svcCtx, in)
}

// SetSpectateFeatured 运营把一场公开局标成 / 取消标成赛事局(battle-spectate-tiered-fanout.md §15.6)。
func (s *MatchInternalServer) SetSpectateFeatured(ctx context.Context, in *matchpb.SetSpectateFeaturedRequest) (*matchpb.SetSpectateFeaturedResponse, error) {
	return logic.NewSetSpectateFeaturedLogic(ctx, s.svcCtx).SetSpectateFeatured(in)
}
