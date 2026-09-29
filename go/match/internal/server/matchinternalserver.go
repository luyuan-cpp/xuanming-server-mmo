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
// 唯一合法调用方是 go/guild(帮会同道历练全员同意后确认开战)。客户端来源(带会话 metadata)的调用
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
