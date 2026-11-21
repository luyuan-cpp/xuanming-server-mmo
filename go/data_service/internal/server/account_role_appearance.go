package server

import (
	"context"
	"strings"

	dspb "proto/data_service"
	"shared/accountdirectory"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const maxAppearanceIDBytes = 64

// UpdateRoleAppearance 处理 scene 发起的角色永久外观同步(docs/design/character-appearance-switch.md)。
// 只认 scene 的调用证明(与入场登记、改名共用 RoleAdmissionAuth)。
func (s *DataServiceServer) UpdateRoleAppearance(ctx context.Context, in *dspb.UpdateRoleAppearanceRequest) (*dspb.UpdateRoleAppearanceResponse, error) {
	if s.svcCtx.RoleAppearances == nil || s.svcCtx.RoleAdmissionAuth == nil {
		return nil, status.Error(codes.Unavailable, "role appearance service is unavailable")
	}
	caller, err := s.svcCtx.RoleAdmissionAuth.Verify(ctx, accountdirectory.MethodPrefix+"UpdateRoleAppearance", in)
	if err != nil || caller != "scene" {
		return nil, status.Error(codes.PermissionDenied, "invalid caller proof for role appearance update")
	}
	appearanceID := strings.TrimSpace(in.GetAppearanceId())
	if in.GetPlayerId() == 0 || appearanceID == "" || len(appearanceID) > maxAppearanceIDBytes {
		return nil, status.Error(codes.InvalidArgument, "player_id and appearance_id must be valid")
	}
	updated, err := s.svcCtx.RoleAppearances.UpdateRoleAppearance(ctx, in.GetPlayerId(), appearanceID)
	if err != nil {
		return nil, directoryError(err)
	}
	return &dspb.UpdateRoleAppearanceResponse{Updated: updated}, nil
}
