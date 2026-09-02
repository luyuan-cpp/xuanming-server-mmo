package server

import (
	"context"

	"match/internal/logic"
	"match/internal/svc"

	base "proto/common/base"
	matchpb "proto/match"
)

// MatchServiceServer 是 gRPC 入口薄包装(照 scene_manager 的
// internal/server 模式):每个 RPC 委托对应 logic,不写业务。
type MatchServiceServer struct {
	svcCtx *svc.ServiceContext
	matchpb.UnimplementedMatchServiceServer
}

func NewMatchServiceServer(svcCtx *svc.ServiceContext) *MatchServiceServer {
	return &MatchServiceServer{
		svcCtx: svcCtx,
	}
}

// JoinQueue 玩家入队(客户端经 gate gRPC 直达)。
func (s *MatchServiceServer) JoinQueue(ctx context.Context, in *matchpb.JoinQueueRequest) (*matchpb.JoinQueueResponse, error) {
	l := logic.NewJoinQueueLogic(ctx, s.svcCtx)
	return l.JoinQueue(in)
}

// CancelQueue 取消排队。
func (s *MatchServiceServer) CancelQueue(ctx context.Context, in *matchpb.CancelQueueRequest) (*base.Empty, error) {
	l := logic.NewCancelQueueLogic(ctx, s.svcCtx)
	return l.CancelQueue(in)
}

// GetQueueStatus 查询排队状态。
func (s *MatchServiceServer) GetQueueStatus(ctx context.Context, in *matchpb.GetQueueStatusRequest) (*matchpb.GetQueueStatusResponse, error) {
	l := logic.NewGetQueueStatusLogic(ctx, s.svcCtx)
	return l.GetQueueStatus(in)
}

// ChallengePlayer 场景发起 PK(切磋)。
func (s *MatchServiceServer) ChallengePlayer(ctx context.Context, in *matchpb.ChallengePlayerRequest) (*matchpb.ChallengePlayerResponse, error) {
	l := logic.NewChallengeLogic(ctx, s.svcCtx)
	return l.ChallengePlayer(in)
}

// RespondChallenge 应战 / 拒战。
func (s *MatchServiceServer) RespondChallenge(ctx context.Context, in *matchpb.RespondChallengeRequest) (*matchpb.RespondChallengeResponse, error) {
	l := logic.NewChallengeLogic(ctx, s.svcCtx)
	return l.RespondChallenge(in)
}

// NotifyChallengeInvite / NotifyChallengeResult:S2C 推送消息借 service 声明
// 拿 message id 的占位 RPC,实际下行走 Kafka gate PushToPlayerEvent。
func (s *MatchServiceServer) NotifyChallengeInvite(ctx context.Context, in *matchpb.ChallengeInviteS2C) (*base.Empty, error) {
	l := logic.NewChallengeLogic(ctx, s.svcCtx)
	return l.NotifyChallengeInvite(in)
}

func (s *MatchServiceServer) NotifyChallengeResult(ctx context.Context, in *matchpb.ChallengeResultS2C) (*base.Empty, error) {
	l := logic.NewChallengeLogic(ctx, s.svcCtx)
	return l.NotifyChallengeResult(in)
}

// WatchBattle 观战接入(二期,设计文档 §10;battle_id=0 随机观战)。
func (s *MatchServiceServer) WatchBattle(ctx context.Context, in *matchpb.WatchBattleRequest) (*matchpb.WatchBattleResponse, error) {
	l := logic.NewWatchBattleLogic(ctx, s.svcCtx)
	return l.WatchBattle(in)
}

// ListWatchableBattles 可观战列表(二期,设计文档 §10)。
func (s *MatchServiceServer) ListWatchableBattles(ctx context.Context, in *matchpb.ListWatchableBattlesRequest) (*matchpb.ListWatchableBattlesResponse, error) {
	l := logic.NewListWatchableBattlesLogic(ctx, s.svcCtx)
	return l.ListWatchableBattles(in)
}
