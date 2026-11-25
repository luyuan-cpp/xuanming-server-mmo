package server

import (
	"context"

	"data_service/internal/store"
	dspb "proto/data_service"
	"shared/accountdirectory"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// SyncAccountStage1Tradable 处理 scene 发起的账号级阶段一可交易装备材料配额同步（跨角色/删角共享终身 60 份硬上限）。
// 只认 scene 的调用证明（与入场登记、改名、外观同步共用 RoleAdmissionAuth）。
func (s *DataServiceServer) SyncAccountStage1Tradable(ctx context.Context, in *dspb.SyncAccountStage1TradableRequest) (*dspb.SyncAccountStage1TradableResponse, error) {
	if s.svcCtx.AccountTradableQuota == nil || s.svcCtx.RoleAdmissionAuth == nil {
		return nil, status.Error(codes.Unavailable, "account tradable material quota service is unavailable")
	}
	caller, err := s.svcCtx.RoleAdmissionAuth.Verify(ctx, accountdirectory.MethodPrefix+"SyncAccountStage1Tradable", in)
	if err != nil || caller != "scene" {
		return nil, status.Error(codes.PermissionDenied, "invalid caller proof for account tradable quota sync")
	}
	if in.GetPlayerId() == 0 || in.GetDesiredCharTotal() > store.Stage1TradableMaterialLifetimeLimit {
		return nil, status.Error(codes.InvalidArgument, "player_id and desired_char_total must be valid")
	}
	found, confirmed, used, err := s.svcCtx.AccountTradableQuota.SyncAccountStage1Tradable(ctx, in.GetPlayerId(), in.GetDesiredCharTotal())
	if err != nil {
		return nil, directoryError(err)
	}
	return &dspb.SyncAccountStage1TradableResponse{
		Found:              found,
		ConfirmedCharTotal: confirmed,
		AccountUsedTotal:   used,
	}, nil
}
