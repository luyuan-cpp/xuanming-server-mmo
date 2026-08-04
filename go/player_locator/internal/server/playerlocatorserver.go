package server

import (
	"context"

	"player_locator/internal/logic"
	"player_locator/internal/svc"
	common "proto/common/base"
	pb "proto/player_locator"
)

type PlayerLocatorServer struct {
	svcCtx *svc.ServiceContext
	pb.UnimplementedPlayerLocatorServer
}

func NewPlayerLocatorServer(svcCtx *svc.ServiceContext) *PlayerLocatorServer {
	return &PlayerLocatorServer{svcCtx: svcCtx}
}

func (s *PlayerLocatorServer) SetLocation(ctx context.Context, in *pb.PlayerLocation) (*common.Empty, error) {
	l := logic.NewSetLocationLogic(ctx, s.svcCtx)
	return l.SetLocation(in)
}

func (s *PlayerLocatorServer) GetLocation(ctx context.Context, in *pb.PlayerId) (*pb.PlayerLocation, error) {
	l := logic.NewGetLocationLogic(ctx, s.svcCtx)
	return l.GetLocation(in)
}

func (s *PlayerLocatorServer) MarkOffline(ctx context.Context, in *pb.MarkOfflineRequest) (*common.Empty, error) {
	l := logic.NewMarkOfflineLogic(ctx, s.svcCtx)
	return l.MarkOffline(in)
}

func (s *PlayerLocatorServer) SetSession(ctx context.Context, in *pb.SetSessionRequest) (*common.Empty, error) {
	l := logic.NewSetSessionLogic(ctx, s.svcCtx)
	return l.SetSession(in)
}

func (s *PlayerLocatorServer) GetSession(ctx context.Context, in *pb.GetSessionRequest) (*pb.GetSessionResponse, error) {
	l := logic.NewGetSessionLogic(ctx, s.svcCtx)
	return l.GetSession(in)
}

func (s *PlayerLocatorServer) SetDisconnecting(ctx context.Context, in *pb.SetDisconnectingRequest) (*common.Empty, error) {
	l := logic.NewSetDisconnectingLogic(ctx, s.svcCtx)
	return l.SetDisconnecting(in)
}

func (s *PlayerLocatorServer) Reconnect(ctx context.Context, in *pb.ReconnectRequest) (*pb.ReconnectResponse, error) {
	l := logic.NewReconnectLogic(ctx, s.svcCtx)
	return l.Reconnect(in)
}
